package app

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	generatedweb "github.com/Suknna/quoin/internal/gen/web"
	lintelruntime "github.com/Suknna/quoin/internal/lintel/runtime"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/connections"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"github.com/Suknna/quoin/internal/quoin/secrets"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"google.golang.org/grpc"
)

type servers struct {
	public *http.Server
	ops    *sharedops.Server
	relay  *grpc.Server
}

type apiServer struct {
	auth                 *auth.Service
	db                   *sql.DB
	alerts               *alerts.Service
	reveals              *secrets.Store
	commands             *commandReplay
	runtime              *qruntime.Service
	connections          *connections.Service
	analyses             *analysis.Service
	artifacts            *artifact.Store
	rootKey              func() ([]byte, error)
	probeDispatchFunc    func(ctx context.Context, attemptID int64, summary connections.Summary, epoch uint64, bootID string, grantID int64, input []byte) error
	cancelDispatchFunc   func(ctx context.Context, attemptID int64) error
	analysisDispatchFunc func(ctx context.Context, attemptID int64) error
}

// NewAPIServer is the testable constructor for the Quoin public surface.
// rootKeyFile feeds the credential envelope codec (T07); pass an empty
// string only in tests that never touch connections.
func NewAPIServer(service *auth.Service, db *sql.DB, rootKeyFile string) *apiServer {
	application := &apiServer{
		auth: service, db: db,
		alerts:   alerts.NewService(db),
		reveals:  secrets.NewStore(),
		commands: newCommandReplay(),
		runtime:  qruntime.NewService(db),
		analyses: analysis.NewService(db),
	}
	application.rootKey = func() ([]byte, error) {
		if rootKeyFile == "" {
			return nil, fmt.Errorf("root key file not configured")
		}
		return os.ReadFile(rootKeyFile)
	}
	application.connections = connections.NewService(db, application.rootKey)
	connections.SetReleaseVersion(buildinfo.Release)
	attempt.SetReleaseVersion(buildinfo.Release)
	connections.ProbeContractSource = func() string { return string(gencontracts.ConnectionProbesYAML) }
	return application
}

type authInput struct {
	Session string `cookie:"__Host-quoin-session"`
}

type userOutput struct {
	CacheControl string    `header:"Cache-Control"`
	Pragma       string    `header:"Pragma"`
	Body         auth.User `json:"body"`
}

type loginInput struct {
	UserAgent string `header:"User-Agent"`
	Body      struct {
		Username string `json:"username" maxLength:"200"`
		Password string `json:"password" minLength:"15" maxLength:"128"`
	}
}

type loginOutput struct {
	SetCookie    string    `header:"Set-Cookie"`
	CacheControl string    `header:"Cache-Control"`
	Pragma       string    `header:"Pragma"`
	Body         auth.User `json:"body"`
}

type passwordInput struct {
	// Flattened rather than embedded: huma v2.39.1 does not bind cookie
	// parameters from embedded structs when the input also has a Body.
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		CurrentPassword string `json:"currentPassword" minLength:"15" maxLength:"128"`
		NewPassword     string `json:"newPassword" minLength:"15" maxLength:"128"`
	}
}

type noContentOutput struct {
	SetCookie     string `header:"Set-Cookie,omitempty"`
	ClearSiteData string `header:"Clear-Site-Data,omitempty"`
	CacheControl  string `header:"Cache-Control,omitempty"`
	Pragma        string `header:"Pragma,omitempty"`
}

type runtimeSlot struct {
	Slot              string  `json:"slot"`
	State             string  `json:"state"`
	CurrentGeneration int64   `json:"currentGeneration"`
	RowVersion        int64   `json:"rowVersion"`
	Connected         bool    `json:"connected"`
	BootID            string  `json:"bootId,omitempty"`
	ConnectionEpoch   *uint64 `json:"connectionEpoch,omitempty"`
}

type runtimeStatus struct {
	Plinth runtimeSlot `json:"plinth"`
	Lintel runtimeSlot `json:"lintel"`
}

type runtimeOutput struct {
	Body runtimeStatus `json:"body"`
}

func Run(ctx context.Context, config contract.QuoinConfig) error {
	if config.Component != "quoin" {
		return fmt.Errorf("configuration component must be quoin")
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		return err
	}
	defer database.Close()
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		return err
	}
	hasUsers, err := authService.HasUsers(ctx)
	if err != nil {
		return err
	}
	if !hasUsers {
		return fmt.Errorf("no administrator exists; run attached Admin bootstrap first")
	}
	application := NewAPIServer(authService, database.SQL, config.RootKeyFile)
	serverSet, err := application.newServers(config)
	if err != nil {
		return err
	}
	serviceToken, err := os.ReadFile(config.SteleServiceTokenFile)
	if err != nil {
		return fmt.Errorf("read Stele service token: %w", err)
	}
	serverSet.relay = grpc.NewServer()
	RegisterSteleRelay(serverSet.relay, NewSteleRelayServer(application.alerts, serviceToken))
	artifactStore, err := artifact.NewStore(database.SQL, filepath.Join(config.DataDirectory, "artifacts"))
	if err != nil {
		return fmt.Errorf("open artifact store: %w", err)
	}
	application.artifacts = artifactStore
	// The attempt ledger seals a tool call before its tool_result read
	// grant (the frozen grant closure requires the succeeded state); wire
	// the grant write into CompleteToolCall's transaction.
	application.analyses.Attempts().ToolResultGrants = artifactStore.InsertToolResultGrant
	controlService := NewRuntimeControl(application.runtime, buildinfo.Release, lintelruntime.EmptyCatalogDigest(), application.connections)
	controlService.Analyses = application.analyses
	controlService.Artifacts = artifactStore
	application.probeDispatchFunc = controlService.dispatchAttempt
	application.cancelDispatchFunc = controlService.dispatchCancelRouted
	application.analysisDispatchFunc = controlService.dispatchAnalysisAttempt
	RegisterRuntimeControl(serverSet.relay, controlService)
	RegisterArtifactService(serverSet.relay, NewArtifactService(application.runtime, artifactStore))
	// T12: the periodic lease sweeper converges attempts whose runtime
	// disappeared without reconnecting (RUNTIME-TASK-006).
	go controlService.RunLeaseSweeper(ctx)
	return serverSet.run(ctx, config)
}

func (application *apiServer) newServers(config contract.QuoinConfig) (*servers, error) {
	public, err := NewHandler(application, config.PublicOrigin)
	if err != nil {
		return nil, err
	}
	opsServer, err := sharedops.New("quoin", ":9090", sharedops.Ready)
	if err != nil {
		return nil, err
	}
	return &servers{public: &http.Server{Addr: ":8080", Handler: public, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}, ops: opsServer}, nil
}

// NewHandler builds the same-origin public surface: the real Huma API plus the
// embedded web shell, behind the frozen CSRF and security-header gates.
func NewHandler(application *apiServer, publicOrigin string) (http.Handler, error) {
	mux := http.NewServeMux()
	apiConfig := huma.DefaultConfig("Quoin v1 API", "1.0.0-draft")
	apiConfig.OpenAPIPath = ""
	apiConfig.DocsPath = ""
	apiConfig.SchemasPath = ""
	// An empty transformer list plus nil CreateHooks disable Huma's $schema/Link
	// response injection so bodies match the frozen OpenAPI schemas exactly.
	apiConfig.Transformers = []huma.Transformer{}
	apiConfig.CreateHooks = nil
	api := humago.New(mux, apiConfig)
	application.register(api)
	application.registerAlertStream(mux)
	application.registerTaskStream(mux)
	// The artifact download streams raw bytes with the frozen security
	// headers (HTTP-FILE-003), so it owns the response head directly.
	mux.HandleFunc("GET /api/v1/artifacts/{artifactId}/content", application.downloadArtifactContent)
	application.registerStatic(mux)

	csrf := http.NewCrossOriginProtection()
	if err := csrf.AddTrustedOrigin(publicOrigin); err != nil {
		return nil, fmt.Errorf("configure public Origin: %w", err)
	}
	return securityHeaders(requireBrowserOrigin(csrf.Handler(mux))), nil
}

func (application *apiServer) register(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "login"}, application.login)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/auth/me", OperationID: "getCurrentUser"}, application.me)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/auth/password", OperationID: "changeOwnPassword", DefaultStatus: http.StatusNoContent}, application.changePassword)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/logout", OperationID: "logout", DefaultStatus: http.StatusNoContent}, application.logout)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/runtime", OperationID: "getRuntimeStatus"}, application.runtimeStatus)
	application.registerAlertRoutes(api)
	application.registerAdminUserRoutes(api)
	application.registerRuntimeRoutes(api)
	application.registerConnectionRoutes(api)
	application.registerAnalysisRoutes(api)
	application.registerTaskSnapshot(api)
	application.registerEvidenceRoutes(api)
}

func (application *apiServer) login(ctx context.Context, input *loginInput) (*loginOutput, error) {
	result, retryAfter, err := application.auth.Login(ctx, input.Body.Username, input.Body.Password, input.UserAgent)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			// HTTP-ERROR-006: 429 responses carry a Retry-After header with the
			// predictable recovery window (cooldown remainder in seconds).
			limited := huma.NewError(http.StatusTooManyRequests, "Too Many Requests")
			return nil, huma.ErrorWithHeaders(limited, http.Header{"Retry-After": {strconv.Itoa(int(retryAfter.Round(time.Second).Seconds()))}})
		}
		if errors.Is(err, auth.ErrUnauthenticated) {
			return nil, huma.Error401Unauthorized("用户名或密码错误")
		}
		return nil, huma.Error500InternalServerError("登录暂时不可用", err)
	}
	return &loginOutput{SetCookie: sessionCookie(result.Bearer, 7*24*time.Hour), CacheControl: "no-store", Pragma: "no-cache", Body: result.User}, nil
}

func (application *apiServer) me(ctx context.Context, input *authInput) (*userOutput, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, authFailure(err, "读取当前用户")
	}
	return &userOutput{CacheControl: "no-store", Pragma: "no-cache", Body: session.User}, nil
}

func (application *apiServer) changePassword(ctx context.Context, input *passwordInput) (*noContentOutput, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, authFailure(err, "保存新密码")
	}
	if err := application.auth.ChangePassword(ctx, session, input.Body.CurrentPassword, input.Body.NewPassword); err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			return nil, huma.Error422UnprocessableEntity("当前密码不正确")
		}
		if errors.Is(err, auth.ErrPasswordPolicy) {
			return nil, huma.Error422UnprocessableEntity("新密码不满足密码策略，请更换后重试。")
		}
		// Infrastructure failures (database, lock contention) are server-side
		// faults and must not surface raw internal error text to clients.
		return nil, huma.Error500InternalServerError("暂时无法保存新密码，请重试。", err)
	}
	return &noContentOutput{CacheControl: "no-store", Pragma: "no-cache"}, nil
}

func (application *apiServer) logout(ctx context.Context, input *authInput) (*noContentOutput, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, authFailure(err, "完成登出")
	}
	application.reveals.InvalidateSession(secrets.SessionDigest(input.Session))
	application.runtime.InvalidateSession(secrets.SessionDigest(input.Session))
	if err := application.auth.Logout(ctx, session); err != nil {
		return nil, huma.Error500InternalServerError("无法完成登出", err)
	}
	return &noContentOutput{SetCookie: sessionCookie("", -time.Hour), ClearSiteData: `"cache", "cookies", "storage"`, CacheControl: "no-store", Pragma: "no-cache"}, nil
}

func (application *apiServer) runtimeStatus(ctx context.Context, input *authInput) (*runtimeOutput, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取 Runtime 状态"); err != nil {
		return nil, err
	}
	output := &runtimeOutput{}
	for _, slot := range []string{"plinth", "lintel"} {
		view, err := application.runtime.View(ctx, slot)
		if err != nil {
			return nil, huma.Error500InternalServerError("无法读取 Runtime 状态", err)
		}
		rendered := runtimeSlot{
			Slot: view.Slot, State: string(view.State),
			CurrentGeneration: view.CurrentGeneration, RowVersion: view.RowVersion,
			Connected: view.Connected,
		}
		// Transient projection fields exist only while connected
		// (RuntimeSlot contract: connected=false carries no boot/epoch).
		if view.Connected {
			rendered.BootID = view.BootID
			epoch := view.ConnectionEpoch
			rendered.ConnectionEpoch = epoch
		}
		if slot == "plinth" {
			output.Body.Plinth = rendered
		} else {
			output.Body.Lintel = rendered
		}
	}
	return output, nil
}

// authFailure maps authentication outcomes once for every session-carrying
// endpoint: only deterministic rejection is 401; infrastructure faults are
// logged server-side and surfaced as 500 without internal detail.
func authFailure(err error, action string) error {
	if errors.Is(err, auth.ErrUnauthenticated) {
		return huma.Error401Unauthorized("请重新登录")
	}
	sharedops.LogEvent("quoin", "error", "auth.session_read_failed", err.Error())
	return huma.Error500InternalServerError("暂时无法"+action+"，请重试。", err)
}

func (application *apiServer) registerStatic(mux *http.ServeMux) {
	root, err := fs.Sub(generatedweb.Files, "dist")
	if err != nil {
		panic(err)
	}
	mux.HandleFunc("GET /", func(writer http.ResponseWriter, request *http.Request) {
		name := strings.TrimPrefix(path.Clean(request.URL.Path), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		data, readErr := fs.ReadFile(root, name)
		if readErr != nil {
			if strings.HasPrefix(request.URL.Path, "/api/") || strings.Contains(path.Base(name), ".") {
				http.NotFound(writer, request)
				return
			}
			name = "index.html"
			data, readErr = fs.ReadFile(root, name)
		}
		if readErr != nil {
			http.Error(writer, "frontend unavailable", http.StatusServiceUnavailable)
			return
		}
		if name == "index.html" {
			writer.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasPrefix(name, "assets/") {
			writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		writer.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data)
	})
}

func (serverSet *servers) run(ctx context.Context, config contract.QuoinConfig) error {
	runtimeListener, err := net.Listen("tcp", ":8443")
	if err != nil {
		return fmt.Errorf("listen Runtime gRPC: %w", err)
	}
	tlsConfig, err := tls.LoadX509KeyPair(config.RuntimeTLSCertificateFile, config.RuntimeTLSPrivateKeyFile)
	if err != nil {
		return fmt.Errorf("load Runtime TLS identity: %w", err)
	}
	errCh := make(chan error, 4)
	go func() { errCh <- serverSet.public.ListenAndServe() }()
	go func() {
		// grpc-go enforces ALPN "h2" for TLS clients (>=1.67); the Runtime
		// listener must offer it.
		runtimeTLS := &tls.Config{Certificates: []tls.Certificate{tlsConfig}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}
		errCh <- serverSet.relay.Serve(tls.NewListener(runtimeListener, runtimeTLS))
	}()
	go func() { errCh <- serverSet.ops.Run(ctx) }()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, grpc.ErrServerStopped) {
			return err
		}
	}
	// OPS-SHUTDOWN-001: 60s total grace; the ops drain happened above, so this
	// closing budget stays inside the reserved >=15s connection-close window.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverSet.relay.GracefulStop()
	_ = serverSet.public.Shutdown(shutdownCtx)
	return nil
}

func requireBrowserOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet || request.Method == http.MethodHead || request.Method == http.MethodOptions {
			next.ServeHTTP(writer, request)
			return
		}
		hasMetadata := request.Header.Get("Origin") != "" || request.Header.Get("Sec-Fetch-Site") != ""
		_, hasSessionCookie := findSessionCookie(request)
		isLogin := request.URL.Path == "/api/v1/auth/login"
		// x-quoin-security.csrf: unsafe cookie-carrying requests and login must
		// present same-origin browser metadata; non-browser writers without a
		// session cookie are handled by authentication, not CSRF.
		if !hasMetadata && (isLogin || hasSessionCookie) {
			http.Error(writer, "same-origin request metadata is required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func findSessionCookie(request *http.Request) (string, bool) {
	cookie, err := request.Cookie("__Host-quoin-session")
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(writer, request)
	})
}

func sessionCookie(value string, maxAge time.Duration) string {
	cookie := &http.Cookie{Name: "__Host-quoin-session", Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(maxAge.Seconds())}
	if maxAge < 0 {
		cookie.Expires = time.Unix(1, 0).UTC()
	}
	return cookie.String()
}
