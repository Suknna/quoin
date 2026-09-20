package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	appinspection "github.com/Suknna/quoin/internal/quoin/app/inspection"
	appinvestigation "github.com/Suknna/quoin/internal/quoin/app/investigation"
	appknowledge "github.com/Suknna/quoin/internal/quoin/app/knowledge"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/businessview"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/feedback"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/investigation"
	"github.com/Suknna/quoin/internal/quoin/knowledge"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
	"github.com/Suknna/quoin/internal/quoin/observation"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"github.com/Suknna/quoin/internal/quoin/secrets"
	"github.com/Suknna/quoin/internal/quoin/upgrade"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type servers struct {
	public         *http.Server
	ops            *sharedops.Server
	relay          *grpc.Server
	upgradeGate    *upgradeGate
	beforeShutdown func()
}

type apiServer struct {
	reader      execution.Reader
	readerWired bool
	auth        *auth.Service
	// providers is the login-channel registry (ADR-0010); /api/v1/auth/config
	// projects it and the login page renders from that projection.
	providers                    *auth.Registry
	dataDirectory                string
	db                           *sql.DB
	alerts                       *alerts.Service
	platformFaults               *alerts.PlatformFaultReporter
	stelePublicURL               string
	reveals                      *secrets.Store
	commands                     *commandReplay
	runtime                      *qruntime.Service
	connections                  *connections.Service
	analyses                     *analysis.Service
	investigations               *investigation.Service
	inspections                  *inspection.Service
	views                        *businessview.Service
	observations                 *observation.Service
	feedbackService              *feedback.Service
	knowledgeService             *knowledge.Service
	investigationUpload          *appinvestigation.Handler
	artifacts                    *artifact.Store
	backups                      *backup.Service
	maintenance                  *maintenance.Service
	rootKey                      func() ([]byte, error)
	backupCopy                   func(io.Writer, io.Reader) (int64, error)
	backupAuthorize              func(context.Context, string, string) (auth.Session, error)
	probeDispatchFunc            func(ctx context.Context, attemptID int64, summary connections.Summary, epoch uint64, bootID string, grantID int64, input []byte) error
	cancelDispatchFunc           func(ctx context.Context, attemptID int64) error
	analysisDispatchFunc         func(ctx context.Context, attemptID int64) error
	knowledgeDispatchFunc        func(ctx context.Context, attemptID int64) error
	investigationDispatchFunc    func(ctx context.Context, attemptID int64) error
	inspectionDispatchFunc       func(ctx context.Context)
	inspectionCancelDispatchFunc func(ctx context.Context, attemptID int64) error
	pluginRegistry               *plugins.Registry
	enabledPlugins               []string
	// Upgrade maintenance authorities (T36): the prepare command, the drain
	// reconciler, and the live HTTP surface swap hooks.
	upgradeService    *upgrade.Service
	upgradeReconciler *upgrade.Reconciler
	// upgradeBackups is the maintenance boot's backup authority, kept for the
	// lifecycle owner (tests) to observe that it serves from the shared
	// read-only pool; it is deliberately not the HTTP-facing application.backups.
	upgradeBackups              *backup.Service
	upgradeGate                 *upgradeGate
	setReadiness                func(sharedops.Readiness)
	setMaintenanceReason        func(string, bool)
	onUpgradeMaintenanceEntered func()
	onUpgradeMaintenanceExit    func()
}

// attachmentLimitBytes resolves the deployment message-level attachment
// boundary (HTTP-FILE-002: default 10 MiB, deployment-tunable through
// QUOIN_ATTACHMENT_LIMIT_BYTES).
func attachmentLimitBytes() int64 {
	if raw := os.Getenv("QUOIN_ATTACHMENT_LIMIT_BYTES"); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
			return value
		}
	}
	return investigation.DefaultAttachmentLimitBytes
}

// NewAPIServer is the testable constructor for the Quoin public surface.
// rootKeyFile feeds the credential envelope codec (T07); pass an empty
// string only in tests that never touch connections.
func NewAPIServer(service *auth.Service, db *sql.DB, rootKeyFile string) *apiServer {
	application := newAPIServer(service, db, rootKeyFile)
	// Derived projections reconcile at boot: the knowledge search docs are
	// rebuilt from their authority (current ∧ not exited) so a torn history
	// cannot silently hide confirmed knowledge from retrieval.
	rebuildCtx, rebuildCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := application.knowledgeService.RebuildSearchDocs(rebuildCtx); err != nil {
		sharedops.LogEvent("quoin", "error", "knowledge.search_docs_rebuild_failed", err.Error())
	}
	rebuildCancel()
	return application
}

// NewMaintenanceAPIServer constructs only durable domain authorities. It does
// not reconcile derived data, dispatch work, start GC, or start schedulers.
func NewMaintenanceAPIServer(service *auth.Service, db *sql.DB, rootKeyFile string) *apiServer {
	return newAPIServer(service, db, rootKeyFile)
}

// SetStelePublicURL projects the deployment-owned external receiver endpoint.
// It must be configured at process construction, never inferred from an HTTP
// Host or forwarding header controlled by an untrusted upstream.
func (application *apiServer) SetStelePublicURL(publicURL string) {
	application.stelePublicURL = publicURL
}

func newAPIServer(service *auth.Service, db *sql.DB, rootKeyFile string) *apiServer {
	alertService := alerts.NewService(db)
	application := &apiServer{
		auth:             service,
		db:               db,
		alerts:           alertService,
		platformFaults:   alerts.NewPlatformFaultReporter(alertService),
		reveals:          secrets.NewStore(),
		commands:         newCommandReplay(),
		runtime:          qruntime.NewService(),
		analyses:         analysis.NewService(db),
		investigations:   investigation.NewService(db),
		inspections:      inspection.NewService(db),
		views:            businessview.NewService(db),
		feedbackService:  feedback.NewService(db),
		knowledgeService: knowledge.NewService(db),
		maintenance:      maintenance.NewService(db),
		upgradeService:   upgrade.NewService(db),
	}
	application.initPluginRegistry()
	application.rootKey = func() ([]byte, error) {
		if rootKeyFile == "" {
			return nil, fmt.Errorf("root key file not configured")
		}
		return os.ReadFile(rootKeyFile)
	}
	// The silent-deployment default keeps tests and the maintenance surface
	// working; Run re-resolves the deployment YAML before any serving starts.
	if service, err := newSourceObservationService(db, nil); err == nil {
		application.observations = service
	}
	application.connections = connections.NewService(db, application.rootKey)
	// ADR-0004：接入启用即用。默认基础巡检计划在启用事务内幂等创建（仅人工
	// 运行，不产生定时模型费用）；失败回滚整个启用，确保不会出现"已启用却
	// 无即用计划"的中间态。hook 收到连接执行器 runner 的受守卫事务句柄，
	// 不能逃逸或提交该事务。
	application.connections.SetPostEnableInTx(func(ctx context.Context, conn execution.Executor, name string) error {
		var connectionID int64
		if err := conn.QueryRowContext(ctx, `SELECT id FROM connections WHERE name=?`, name).Scan(&connectionID); err != nil {
			return err
		}
		return application.inspections.EnsureDefaultPlanOn(ctx, conn, connectionID, name)
	})
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
	SetCookie     string `header:"Set-Cookie"`
	ClearSiteData string `header:"Clear-Site-Data"`
	CacheControl  string `header:"Cache-Control"`
	Pragma        string `header:"Pragma"`
}

type runtimeSlot struct {
	Slot            string  `json:"slot"`
	Connected       bool    `json:"connected"`
	BootID          string  `json:"bootId,omitempty"`
	ConnectionEpoch *uint64 `json:"connectionEpoch,omitempty"`
	LastSeenAt      string  `json:"lastSeenAt,omitempty"`
	ReleaseVersion  string  `json:"releaseVersion,omitempty"`
}

// aboutStatus is the admin-only product projection of real runtime and
// maintenance facts. Unknown values stay empty at the transport boundary and
// are rendered explicitly as Unknown by the UI rather than guessed healthy.
type aboutMaintenance struct {
	Active     bool   `json:"active"`
	Reason     string `json:"reason,omitempty"`
	RowVersion int64  `json:"rowVersion"`
}

type aboutStatus struct {
	ReleaseVersion string           `json:"releaseVersion"`
	Maintenance    aboutMaintenance `json:"maintenance"`
	Components     []runtimeSlot    `json:"components"`
}

// runtimeRelayCredentials builds the Runtime gRPC server's mTLS transport
// credentials: the quoin server identity plus mandatory client certificates
// verified against the deployment CA (ADR-0009).
func runtimeRelayCredentials(config contract.QuoinConfig) (credentials.TransportCredentials, error) {
	serverIdentity, err := tls.LoadX509KeyPair(config.RuntimeTLSCertificateFile, config.RuntimeTLSPrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load Runtime TLS identity: %w", err)
	}
	clientCA, err := os.ReadFile(config.RuntimeClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read Runtime client CA: %w", err)
	}
	clientPool := x509.NewCertPool()
	if !clientPool.AppendCertsFromPEM(clientCA) {
		return nil, fmt.Errorf("Runtime client CA is not valid PEM")
	}
	// grpc-go negotiates ALPN "h2" itself; MinVersion and the client-cert
	// policy are the deployment's transport authority.
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverIdentity}, MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientPool,
	}), nil
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
	retentionMonths := 6
	if config.Audit != nil {
		retentionMonths = config.Audit.RetentionMonths
	}
	if err := prepareAuthenticationBootstrap(ctx, authService, config.DataDirectory, retentionMonths); err != nil {
		return fmt.Errorf("initialize authentication: %w", err)
	}
	var maintenanceActive int
	var maintenanceReason sql.NullString
	if err := database.SQL.QueryRowContext(ctx, `SELECT active,reason FROM maintenance_state WHERE id=1`).Scan(&maintenanceActive, &maintenanceReason); err != nil {
		return fmt.Errorf("read maintenance state: %w", err)
	}
	if maintenanceActive == 1 {
		application := NewMaintenanceAPIServer(authService, database.SQL, config.RootKeyFile)
		if err := application.configureReadOnly(database.Reader); err != nil {
			return err
		}
		application.dataDirectory = config.DataDirectory
		if err := application.configureLoginProviders(config.Authentication); err != nil {
			return fmt.Errorf("configure login providers: %w", err)
		}
		serverSet, err := newMaintenanceServers(application, config, maintenanceReason.String)
		if err != nil {
			return err
		}
		serverSet.ops.SetMaintenanceReason(maintenanceReason.String, true)
		if maintenanceReason.String == "Upgrade" {
			// A restart inside Upgrade maintenance keeps converging durably:
			// the reconciler projects the frozen checklist and runs the
			// pre-upgrade backup, while a dispatch-free lease sweeper closes
			// attempts whose runtime disappeared with the previous process.
			if err := application.startUpgradeMaintenanceRuntime(ctx, config, serverSet); err != nil {
				return err
			}
			// Exiting the aborted upgrade stops this maintenance-shaped process;
			// the deployment's restart policy boots the normal surface.
			stop := make(chan struct{})
			application.onUpgradeMaintenanceExit = func() { close(stop) }
			return serverSet.runMaintenance(ctx, stop)
		}
		return serverSet.runMaintenance(ctx, nil)
	}
	application := NewAPIServer(authService, database.SQL, config.RootKeyFile)
	if err := application.configureReadOnly(database.Reader); err != nil {
		return err
	}
	application.dataDirectory = config.DataDirectory
	if err := application.configureLoginProviders(config.Authentication); err != nil {
		return fmt.Errorf("configure login providers: %w", err)
	}
	StartAuditCleanup(ctx, database.SQL)
	application.ConfigureSourceObservation(config.EnabledPlugins)
	application.SetStelePublicURL(config.StelePublicURL)
	serverSet, err := application.newServers(config)
	if err != nil {
		return err
	}
	// ADR-0004: deployment YAML selects plugin enablement; the resolved
	// state feeds the management catalog, the frozen tool catalogs of every
	// agent slice and the fault-origin eligibility in one authoritative pass.
	if _, err := application.configurePlugins(config.EnabledPlugins); err != nil {
		return fmt.Errorf("configure plugins: %w", err)
	}
	// Plinth probes network partitions every 20 seconds. Accept those idle
	// HTTP/2 pings: the default gRPC server minimum is five minutes and sends
	// GOAWAY(too_many_pings), which otherwise prevents every runtime task.
	// TLS terminates inside gRPC (grpc.Creds) so every handler context carries
	// the verified client identity: component identity is the leaf certificate
	// CN (plinth/stele) verified against the deployment CA (ADR-0009).
	runtimeTLSCreds, err := runtimeRelayCredentials(config)
	if err != nil {
		return err
	}
	serverSet.relay = grpc.NewServer(grpc.KeepaliveEnforcementPolicy(runtimeRelayKeepalivePolicy()), grpc.Creds(runtimeTLSCreds))
	serverSet.beforeShutdown = application.runtime.CloseAll
	RegisterSteleRelay(serverSet.relay, NewSteleRelayServer(application.alerts))
	artifactStore, err := artifact.NewStore(database.SQL, filepath.Join(config.DataDirectory, "artifacts"))
	if err != nil {
		return fmt.Errorf("open artifact store: %w", err)
	}
	application.artifacts = artifactStore
	if err := artifactStore.SetReader(database.Reader); err != nil {
		return fmt.Errorf("wire artifact store reader: %w", err)
	}
	gcProjector, err := serverSet.ops.ArtifactGCSuccessProjector()
	if err != nil {
		return err
	}
	artifactStore.SetGCSuccessProjector(gcProjector)
	// Generated-artifact expiry is a real storage lifecycle, not merely an
	// Admin setting. The bounded in-process collector retains metadata while
	// making expired bodies unavailable.
	go artifactStore.RunGC(ctx)
	backupService, err := backup.NewService(database.SQL, backup.Config{
		DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory,
		ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts"), ArtifactStore: artifactStore,
		ScheduleAdmission: func() bool {
			state := serverSet.ops.Readiness()
			return state.Mode == "normal" && state.AcceptingWork && state.Reason == sharedops.Ready
		},
		AuthorizeActor: func(commandContext context.Context, conn execution.Executor, actorID int64) error {
			var maintenanceActive int
			if err := auth.VerifyExecutionSession(commandContext, conn, "admin"); err != nil {
				return backup.ErrActorUnauthorized
			}
			if err := conn.QueryRowContext(commandContext, `SELECT active FROM maintenance_state WHERE id=1`).Scan(&maintenanceActive); err != nil {
				return backup.ErrMaintenanceActive
			}
			if maintenanceActive != 0 {
				return backup.ErrMaintenanceActive
			}
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("open backup service: %w", err)
	}
	backupMetrics, err := serverSet.ops.BackupMetrics()
	if err != nil {
		return err
	}
	backupService.SetMetrics(backupMetrics)
	if err := backupService.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile backups: %w", err)
	}
	if err := application.SetBackupService(backupService); err != nil {
		return fmt.Errorf("attach backup service: %w", err)
	}
	go backupService.RunScheduler(ctx)
	// T36: the upgrade reconciler owns the drain checklist projection, the
	// verified pre-upgrade backup and the quoin_upgrade_prepared gauge.
	upgradeBackups := &backupUpgradeRunner{service: backupService}
	application.upgradeReconciler = upgrade.NewReconciler(database.SQL, upgradeBackups)
	if projector, projectorErr := serverSet.ops.UpgradePreparedProjector(); projectorErr == nil {
		application.upgradeReconciler.SetPrepared(projector)
	} else {
		return projectorErr
	}
	go application.upgradeReconciler.Run(ctx)
	// T14: investigation attachment staging streams through the same
	// content-addressed store; the deployment boundary (default 10 MiB,
	// HTTP-FILE-002) is environment-tunable.
	application.investigations.SetAttachmentStore(artifactStore, attachmentLimitBytes())
	application.inspections.SetArtifactWriter(artifactStore.MaterializeEvidenceTransaction)
	// The attempt ledger seals a tool call before its tool_result read
	// grant (the frozen grant closure requires the succeeded state); wire
	// the grant write into CompleteToolCall's transaction.
	application.analyses.Attempts().ToolResultGrants = artifactStore.InsertToolResultGrant
	application.investigations.Attempts().ToolResultGrants = artifactStore.InsertToolResultGrant
	controlService := NewRuntimeControl(application.runtime, buildinfo.Release, application.connections, application.db)
	controlService.PlatformFaults = application.platformFaults
	// The initial-analysis terminal transaction is the only current reachable
	// worker-launch failure authority. Project its fault inside the runner's
	// guarded transaction (before COMMIT) so a successful ResultAck never
	// outlives a missing platform-fault mutation. The structural interfaces
	// (analysis.TxWriter -> alerts.Transaction) keep both packages decoupled.
	application.analyses.ProjectTerminalOutcome = func(ctx context.Context, tx analysis.TxWriter, sequence int64, succeeded bool, termination string) error {
		return application.platformFaults.ObserveExecutionOutcomeOn(ctx, tx, sequence, succeeded, termination)
	}
	// Scheduling admission stops inside any maintenance revision: missed
	// boundaries record their durable runtime_unavailable tombstone instead
	// of creating dispatchable work (OPS-UPGRADE-003).
	controlService.MaintenanceBlocking = maintenanceAdmissionChecker(ctx, database.SQL)
	controlService.Inspections = application.inspections
	controlService.Analyses = application.analyses
	controlService.Investigations = application.investigations
	controlService.Knowledge = application.knowledgeService
	controlService.Artifacts = artifactStore
	application.probeDispatchFunc = controlService.dispatchAttempt
	// The semantic search channel embeds the query through the same real
	// dispatch path; the kick is best effort (failures leave the query
	// attempt Queued for the reconnect sweep and an honestly empty page).
	application.knowledgeService.Embeddings().SetDispatcher(controlService.dispatchEmbeddingAttemptForSearch)
	application.cancelDispatchFunc = controlService.dispatchCancelRouted
	application.analysisDispatchFunc = controlService.dispatchAnalysisAttempt
	application.knowledgeDispatchFunc = controlService.dispatchKnowledgeExtractionAttempt
	investigationRuntime := &appinvestigation.RuntimeSlice{
		Service: application.investigations,
		DB:      application.db,
		SlotView: func(ctx context.Context) (appinvestigation.PlinthView, error) {
			view, err := application.runtime.View(ctx, qruntime.SlotPlinth)
			if err != nil {
				return appinvestigation.PlinthView{}, err
			}
			slice := appinvestigation.PlinthView{Connected: view.Connected, BootID: view.BootID, ReleaseVersion: view.ReleaseVersion}
			if view.ConnectionEpoch != nil {
				slice.ConnectionEpoch = *view.ConnectionEpoch
			}
			return slice, nil
		},
		SendEnvelope: func(envelope *runtimev1.ControlEnvelope) error {
			return controlService.sendEnvelope(qruntime.SlotPlinth, envelope)
		},
		TerminationReason: terminationReasonOf,
	}
	controlService.InvestigationRuntime = investigationRuntime
	application.investigationDispatchFunc = investigationRuntime.Dispatch
	application.inspectionDispatchFunc = controlService.dispatchQueuedInspections
	controlService.Observations = application.observations
	application.inspectionCancelDispatchFunc = controlService.dispatchInspectionCancellation
	RegisterRuntimeControl(serverSet.relay, controlService)
	RegisterArtifactService(serverSet.relay, NewArtifactService(application.runtime, artifactStore))
	// T12: the periodic lease sweeper converges attempts whose runtime
	// disappeared without reconnecting (RUNTIME-TASK-006).
	go controlService.RunLeaseSweeper(ctx)
	go controlService.RunInspectionScheduler(ctx)
	// Current published declarations are admitted immediately and thereafter by
	// a process-owned poller. The domain command supplies durable interval,
	// current-pointer, active-run, and command-key fences; maintenance returns
	// above before this normal-runtime loop can start.
	go NewSourceObservationScheduler(application.observations, controlService.dispatchQueuedSourceObservationAttempts).Run(ctx, func(err error) {
		sharedops.LogEvent("quoin", "error", "source_observation.scheduler", err.Error())
	})
	application.upgradeGate = serverSet.upgradeGate
	application.setReadiness = serverSet.ops.SetReadiness
	application.setMaintenanceReason = serverSet.ops.SetMaintenanceReason
	application.onUpgradeMaintenanceEntered = func() { application.enterUpgradeMaintenance(config.PublicOrigin) }
	application.onUpgradeMaintenanceExit = application.exitUpgradeMaintenance
	return serverSet.run(ctx, config)
}

func newMaintenanceServers(application *apiServer, config contract.QuoinConfig, maintenanceReason string) (*servers, error) {
	public, err := newMaintenanceHandler(application, config.PublicOrigin, maintenanceReason)
	if err != nil {
		return nil, err
	}
	opsServer, err := sharedops.New("quoin", ":9090", sharedops.Maintenance)
	if err != nil {
		return nil, err
	}
	opsServer.SetReadiness(sharedops.Readiness{Component: "quoin", Release: buildinfo.Release, Mode: "maintenance", AcceptingWork: false, Reason: sharedops.Maintenance})
	return &servers{public: &http.Server{Addr: ":8080", Handler: public, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}, ops: opsServer}, nil
}

func (application *apiServer) newServers(config contract.QuoinConfig) (*servers, error) {
	public, err := NewHandler(application, config.PublicOrigin)
	if err != nil {
		return nil, err
	}
	// The live upgrade gate wraps the complete normal surface; entering
	// Upgrade maintenance swaps it to the maintenance allowlist without
	// touching the Runtime control plane or the sweeps.
	gate := newUpgradeGate(public)
	application.upgradeGate = gate
	opsServer, err := sharedops.New("quoin", ":9090", sharedops.Ready)
	if err != nil {
		return nil, err
	}
	return &servers{public: &http.Server{Addr: ":8080", Handler: gate, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}, ops: opsServer, upgradeGate: gate}, nil
}

// NewHandler builds Quoin's backend-only public surface. The independent
// frontend service owns pages and SPA fallback; the shared entry proxy keeps
// browser traffic same-origin before dispatching it here.
func NewHandler(application *apiServer, publicOrigin string) (http.Handler, error) {
	configureHumaErrorModel()
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
	accessRegistry, err := NormalAccessRegistry()
	if err != nil {
		return nil, err
	}
	admission, err := NewAccessAdmission(application, accessRegistry, nil)
	if err != nil {
		return nil, err
	}
	api.UseMiddleware(admission.HumaMiddleware())
	application.register(api)
	if err := accessRegistry.ValidateSurface(api); err != nil {
		return nil, err
	}
	alertStream, err := admission.Wrap("streamAlertEvents", http.HandlerFunc(newAlertEventStream(application).serve))
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /api/v1/alerts/events", alertStream)
	// The artifact download streams raw bytes with the frozen security
	// headers (HTTP-FILE-003), so it owns the response head directly.
	artifactDownload, err := admission.Wrap("downloadArtifactContent", http.HandlerFunc(application.downloadArtifactContent))
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /api/v1/artifacts/{artifactId}/content", artifactDownload)
	backupDownload, err := admission.Wrap("downloadBackup", http.HandlerFunc(application.downloadBackup))
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /api/v1/backups/{backupId}/download", backupDownload)
	// The attachment upload streams multipart parts into staging without
	// whole-body buffering (HTTP-FILE-001); it also owns its response head.
	attachmentUpload, err := admission.Wrap("uploadInvestigationAttachment", http.HandlerFunc(application.investigationUpload.ServeUpload))
	if err != nil {
		return nil, err
	}
	mux.Handle("POST /api/v1/investigation-attachments", attachmentUpload)
	// Every declared raw route must actually be wrapped: the Huma surface
	// check cannot see raw mux routes, so the admission proof ends here.
	if err := admission.AssertRawSurface(); err != nil {
		return nil, err
	}

	csrf := http.NewCrossOriginProtection()
	if err := csrf.AddTrustedOrigin(publicOrigin); err != nil {
		return nil, fmt.Errorf("configure public Origin: %w", err)
	}
	// Security headers also cover responses rejected by the bootstrap gate.
	gated, err := newBootstrapGate(application, accessRegistry, requireBrowserOrigin(csrf.Handler(mux)))
	if err != nil {
		return nil, err
	}
	return securityHeaders(gated), nil
}

func (application *apiServer) register(api huma.API) {
	application.registerAuthenticationFlows(api)
	application.registerAuthConfigRoutes(api)
	application.registerAuthContactRoutes(api)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/auth/me", OperationID: "getCurrentUser"}, application.me)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/auth/password", OperationID: "changeOwnPassword", DefaultStatus: http.StatusNoContent}, application.changePassword)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/logout", OperationID: "logout", DefaultStatus: http.StatusNoContent}, application.logout)
	// T36: the maintenance projection and the coordinated-upgrade entry.
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/maintenance", OperationID: "getMaintenanceState"}, application.getMaintenanceState)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/maintenance/upgrade/prepare", OperationID: "prepareUpgrade"}, application.prepareUpgrade)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/admin/about", OperationID: "getAdminAbout"}, application.aboutPlatform)
	application.registerBusinessContextRoute(api)
	application.registerAlertRoutes(api)
	application.registerAdminUserRoutes(api)
	application.registerConnectionRoutes(api)
	application.registerPluginRoutes(api)
	application.registerAnalysisRoutes(api)
	application.registerEvidenceRoutes(api)
	application.registerBackupRoutes(api)
	investigationHandler := &appinvestigation.Handler{
		Service: application.investigations,
		Authenticate: func(ctx context.Context, cookie string) (int64, error) {
			session, err := application.authenticateFull(ctx, cookie, "使用调查")
			if err != nil {
				return 0, err
			}
			return session.User.ID, nil
		},
		// The established stream re-checks the session every tick; any
		// revocation/expiry closes it silently (Q214, SEC-SESSION-002).
		SessionValid: func(ctx context.Context, cookie string) bool {
			_, err := application.authenticateFull(ctx, cookie, "使用调查")
			return err == nil
		},
		// Resolved at request time like the analysis dispatch funcs: the
		// runtime slice is wired in Run() after the servers are built, so
		// a register-time capture would stay nil forever.
		Dispatch: func(ctx context.Context, attemptID int64) error {
			if application.investigationDispatchFunc == nil {
				return errors.New("investigation dispatch not wired")
			}
			return application.investigationDispatchFunc(ctx, attemptID)
		},
		// The committed stop/undo fence travels the same routed cancel
		// dispatcher as the analysis cancel (RUNTIME-CANCEL-001).
		CancelDispatch: func(ctx context.Context, attemptID int64) error {
			if application.cancelDispatchFunc == nil {
				return errors.New("cancel dispatch not wired")
			}
			return application.cancelDispatchFunc(ctx, attemptID)
		},
	}
	investigationHandler.Register(api)
	inspectionHandler := &appinspection.Handler{
		Inspections: application.inspections,
		Authenticate: func(ctx context.Context, cookie string) (int64, error) {
			session, err := application.authenticateAdmin(ctx, cookie, "使用巡检")
			if err != nil {
				return 0, err
			}
			return session.User.ID, nil
		},
		DispatchInspections: func(ctx context.Context) {
			if application.inspectionDispatchFunc != nil {
				application.inspectionDispatchFunc(ctx)
			}
		},
		CancelDispatch: func(ctx context.Context, attemptID int64) error {
			if application.inspectionCancelDispatchFunc == nil {
				return errors.New("inspection cancel dispatch not wired")
			}
			return application.inspectionCancelDispatchFunc(ctx, attemptID)
		},
	}
	inspectionHandler.Register(api)
	// 业务视图（ADR-0004）：可选组织能力，仅 Admin 可管理。
	viewHandler := &businessview.Handler{
		Views: application.views,
		Authenticate: func(ctx context.Context, cookie string) (int64, error) {
			session, err := application.authenticateAdmin(ctx, cookie, "管理业务视图")
			if err != nil {
				return 0, err
			}
			return session.User.ID, nil
		},
	}
	viewHandler.Register(api)
	knowledgeHandler := &appknowledge.Handler{
		Feedback:  application.feedbackService,
		Knowledge: application.knowledgeService,
		DispatchImport: func(ctx context.Context, attemptID int64) error {
			if application.knowledgeDispatchFunc == nil {
				return errors.New("knowledge import dispatch not wired")
			}
			return application.knowledgeDispatchFunc(ctx, attemptID)
		},
		DispatchCancel: func(ctx context.Context, attemptID int64) error {
			if application.cancelDispatchFunc == nil {
				return errors.New("knowledge cancellation dispatch not wired")
			}
			return application.cancelDispatchFunc(ctx, attemptID)
		},
		Authenticate: func(ctx context.Context, cookie string) (auth.Session, error) {
			return application.authenticateFull(ctx, cookie, "使用知识")
		},
	}
	knowledgeHandler.Register(api)
	// The raw multipart upload routes (NewHandler) reuse this handler's
	// authentication seams.
	application.investigationUpload = investigationHandler
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

type logoutOutput struct {
	SetCookie     []string `header:"Set-Cookie"`
	ClearSiteData string   `header:"Clear-Site-Data"`
	CacheControl  string   `header:"Cache-Control"`
	Pragma        string   `header:"Pragma"`
}

func (application *apiServer) logout(ctx context.Context, input *authInput) (*logoutOutput, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, authFailure(err, "完成登出")
	}
	application.reveals.InvalidateSession(secrets.SessionDigest(input.Session))
	if err := application.auth.Logout(ctx, session); err != nil {
		return nil, huma.Error500InternalServerError("无法完成登出", err)
	}
	return &logoutOutput{SetCookie: []string{sessionCookie("", -time.Hour)}, ClearSiteData: `"cache", "cookies", "storage"`, CacheControl: "no-store", Pragma: "no-cache"}, nil
}

func dereferenceString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// runtimeSlotProjection translates the shared Runtime authority without
// guessing connection-only facts for an offline component. Both status views
// must use this single projection to keep their privacy and unknown semantics
// identical.
func runtimeSlotProjection(view qruntime.SlotView) runtimeSlot {
	rendered := runtimeSlot{Slot: view.Slot, Connected: view.Connected}
	if view.Connected {
		rendered.BootID = view.BootID
		rendered.ConnectionEpoch = view.ConnectionEpoch
		rendered.LastSeenAt = dereferenceString(view.LastSeenAt)
		rendered.ReleaseVersion = view.ReleaseVersion
	}
	return rendered
}

// aboutPlatform exposes only real, non-secret platform facts and existing
// maintenance state to Admins. Runtime slot maintenance actions stay on their
// established protected commands; Operators cannot call this endpoint.
func (application *apiServer) aboutPlatform(ctx context.Context, input *authInput) (*struct {
	Body aboutStatus `json:"body"`
}, error,
) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "查看平台关于信息"); err != nil {
		return nil, err
	}
	maintenanceState, err := application.maintenance.State(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取维护状态", err)
	}
	output := aboutStatus{ReleaseVersion: buildinfo.Release, Maintenance: aboutMaintenance{Active: maintenanceState.Active, Reason: maintenanceState.Reason, RowVersion: maintenanceState.RowVersion}, Components: []runtimeSlot{}}
	// A disabled plugin's component is not part of this deployment and must
	// not read as a perpetually degraded slot on the About page; the
	// unresolved maintenance surface keeps every historical slot visible.
	components, err := application.runtimeSlotViews(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取组件状态", err)
	}
	output.Components = components
	return &struct {
		Body aboutStatus `json:"body"`
	}{Body: output}, nil
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

// runMaintenance starts only the HTTP maintenance allowlist and ops readiness
// surface. Restore containment is already committed before Quoin is started;
// Runtime, browser and task control streams stay unavailable until maintenance
// exits, so no recovered identity can resume work. An Upgrade boot passes a
// stop channel: a successful exitMaintenance ends the process so the
// deployment's restart policy boots the normal surface.
func (serverSet *servers) runMaintenance(ctx context.Context, stop <-chan struct{}) error {
	errCh := make(chan error, 1)
	go func() { errCh <- serverSet.public.ListenAndServe() }()
	opsDone := make(chan error, 1)
	go func() { opsDone <- serverSet.ops.Run(ctx) }()
	select {
	case <-ctx.Done():
	case <-stop:
		sharedops.LogEvent("quoin", "info", "maintenance.exited", "upgrade maintenance exited by administrator; restarting into normal mode")
	}
	opsErr := <-opsDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = serverSet.public.Shutdown(shutdownCtx)
	return opsErr
}

func (serverSet *servers) run(ctx context.Context, config contract.QuoinConfig) error {
	runtimeListener, err := net.Listen("tcp", ":8443")
	if err != nil {
		return fmt.Errorf("listen Runtime gRPC: %w", err)
	}
	errCh := make(chan error, 4)
	go func() { errCh <- serverSet.public.ListenAndServe() }()
	// TLS itself terminates inside gRPC (the server was built with
	// grpc.Creds): Serve takes the raw listener so every handler context
	// carries the verified mTLS client identity (ADR-0009).
	go func() { errCh <- serverSet.relay.Serve(runtimeListener) }()
	opsDone := make(chan error, 1)
	go func() { opsDone <- serverSet.ops.Run(ctx) }()
	select {
	case <-ctx.Done():
		// OPS-SHUTDOWN-001/OPS-HEALTH-006: the ops listener owns the drained
		// readiness window after SIGTERM; the process must not exit before
		// that surface has closed, or /readyz draining and /livez 200 would
		// never be observable.
		opsDrained := <-opsDone
		// OPS-SHUTDOWN-001: 60s total grace; the ops drain happened above, so
		// this closing budget stays inside the reserved >=15s
		// connection-close window.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if serverSet.beforeShutdown != nil {
			serverSet.beforeShutdown()
		}
		serverSet.relay.GracefulStop()
		_ = serverSet.public.Shutdown(shutdownCtx)
		return opsDrained
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, grpc.ErrServerStopped) {
			return err
		}
	}
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
