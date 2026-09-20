package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/danielgtaylor/huma/v2"
)

// ErrUnauthenticated is the sentinel resolvers return for a credential that
// is cleanly absent, invalid or expired. Any other error means identity
// resolution itself failed (database, infrastructure) and admission must fail
// closed with 503 instead of pretending the caller is anonymous.
var ErrUnauthenticated = errors.New("operations: credential is invalid or expired")

// IsErrUnauthenticated reports whether err is (or wraps) the clean
// unauthenticated sentinel.
func IsErrUnauthenticated(err error) bool { return errors.Is(err, ErrUnauthenticated) }

// Subject is the resolved session principal. It carries exactly the facts the
// guard enforces, the audit records and the execution session reference —
// identifiers and flags, never credentials, cookies or tokens. Initialized
// projects the administrator initialization state; resolvers must not guess
// it from presence of other fields.
type Subject struct {
	UserID                 int64
	Role                   string
	Initialized            bool
	PasswordChangeRequired bool
	// SessionID/AuthRevision populate execution.Metadata.Session so
	// in-transaction re-authorization can verify the session. Flow-derived
	// identities carry the zero reference: a flow is not a session.
	SessionID    int64
	AuthRevision int64
}

// resolved reports whether the subject satisfies the beyond-restricted
// session rule (HTTP-AUTH-006): authenticated, past the forced password
// change and fully initialized.
func (s Subject) resolved() bool { return s.UserID > 0 }

func (s Subject) full() bool { return s.Initialized && !s.PasswordChangeRequired }

// SessionResolver resolves a session credential as a pure read. It returns
// ErrUnauthenticated for clean rejections; infrastructure failures are
// returned verbatim so admission can answer 503 instead of misattributing
// them as anonymous traffic.
type SessionResolver func(ctx context.Context, credential string) (Subject, error)

// Outcome states the access fact recorded for one request.
type Outcome string

const (
	// OutcomeGranted marks the pre-dispatch access-authorization record of an
	// authenticated request. It states that the principal passed admission
	// and dispatch proceeded — not that any business fact committed.
	OutcomeGranted Outcome = "granted"
	// OutcomeDenied marks an authenticated request rejected by the guard or
	// the handler's own authorization/validation (4xx).
	OutcomeDenied Outcome = "denied"
	// OutcomeRequestFailure marks an authenticated request that ended in an
	// infrastructure failure (5xx or an unknown status). It is recorded
	// separately so authorization denials are never inflated with server
	// faults.
	OutcomeRequestFailure Outcome = "request_failure"
)

// Sink receives automatic access facts. Implementations map them onto the
// shared audit writer (internal/quoin/audit, phase "access"); failures are
// contractually fatal for the request: no authenticated content is ever
// released without its access record.
type Sink interface {
	RecordAccess(ctx context.Context, fact AccessFact) error
}

// AccessFact is one automatic access record. Fields are a deliberate
// whitelist — request/response bodies, headers, cookies, credentials, OTPs
// and passwords never appear here.
type AccessFact struct {
	OperationID   string
	Method        string
	Path          string
	Level         Level
	Kind          Kind
	ObjectType    string
	CorrelationID string
	RequestID     string
	// ActorUserID is the resolved principal. Facts are only recorded for
	// claimed identities: an anonymous request is a bounded security-log
	// concern and is never emitted with a synthesized system actor.
	ActorUserID int64
	Outcome     Outcome
	// Status is the final response status; 0 for pre-dispatch records.
	Status int
}

// OnSinkError receives failures of records that cannot fail the request
// closed anymore (denials after rejection, telemetry). Wire it to the ops
// event log.
type OnSinkError func(fact AccessFact, err error)

// Admission is the constructed, validated admission guard of one surface.
// Build it with NewAdmission and install before any route registration:
// huma snapshots the middleware chain per operation at huma.Register time.
type Admission struct {
	registry *AccessRegistry
	sessions SessionResolver
	sink     Sink
	onError  OnSinkError

	// rawWrapMu guards rawWrapped: Wrap marks each raw declaration used so
	// AssertRawSurface can prove at construction time that every declared raw
	// route was actually wrapped (a declared-but-unwrapped route would
	// otherwise serve traffic without admission).
	rawWrapMu  sync.Mutex
	rawWrapped map[string]int
}

// AdmissionDeps wires the collaborators. Every dependency is required except
// OnSinkError; a missing sink or resolver must fail construction instead of
// silently passing unaudited content.
type AdmissionDeps struct {
	Registry *AccessRegistry
	// Sessions resolves session credentials for enforcement and attribution.
	Sessions SessionResolver
	// Sink persists the access facts.
	Sink Sink
	// OnSinkError is optional; the zero hook drops the error.
	OnSinkError OnSinkError
}

// NewAdmission validates the dependency wiring and returns the guard for one
// surface.
func NewAdmission(deps AdmissionDeps) (*Admission, error) {
	switch {
	case deps.Registry == nil:
		return nil, errors.New("operations: access registry is required")
	case deps.Sessions == nil:
		return nil, errors.New("operations: session resolver is required")
	case deps.Sink == nil:
		return nil, errors.New("operations: access sink is required")
	}
	return &Admission{
		registry:   deps.Registry,
		sessions:   deps.Sessions,
		sink:       deps.Sink,
		onError:    deps.OnSinkError,
		rawWrapped: map[string]int{},
	}, nil
}

// markWrapped records one Wrap of a raw declaration. A second wrap of the same
// declaration is a wiring mistake and fails instead of silently double-running
// the admission pass.
func (a *Admission) markWrapped(id string) error {
	a.rawWrapMu.Lock()
	defer a.rawWrapMu.Unlock()
	if count := a.rawWrapped[id]; count > 0 {
		return fmt.Errorf("operations: raw route %q is wrapped %d times", id, count+1)
	}
	a.rawWrapped[id] = 1
	return nil
}

// AssertRawSurface verifies at construction time that every Raw declaration of
// the registry was wrapped exactly once through Wrap. It closes the coverage
// gap the Huma surface check cannot see: a declared raw route whose wrapper
// was forgotten would otherwise serve traffic without admission.
func (a *Admission) AssertRawSurface() error {
	a.rawWrapMu.Lock()
	defer a.rawWrapMu.Unlock()
	var missing []string
	expected := 0
	for _, declaration := range a.registry.decls {
		if !declaration.Raw {
			continue
		}
		expected++
		if a.rawWrapped[declaration.ID] == 0 {
			missing = append(missing, declaration.ID)
		}
	}
	wrapped := 0
	for _, count := range a.rawWrapped {
		wrapped += count
	}
	if len(missing) == 0 && wrapped == expected {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("operations: raw surface is incomplete: unwrapped routes %v (expected %d wrapped raw routes, found %d)", missing, expected, wrapped)
}

// HumaMiddleware returns the middleware for api.UseMiddleware. It must be
// installed before any huma.Register call on the same API.
func (a *Admission) HumaMiddleware() func(ctx huma.Context, next func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		declaration, ok := a.registry.Lookup(ctx.Operation().OperationID)
		if !ok {
			// Construction validation makes this unreachable for registered
			// routes; fail closed rather than serve an undeclared operation.
			writeProblem(ctx, http.StatusServiceUnavailable, "unavailable", "该操作未注册访问声明。")
			return
		}
		decision := a.admit(ctx.Context(), declaration,
			cookieValue(ctx.Header("Cookie"), sessionCookieName))
		if decision.rejectStatus != 0 {
			if decision.subject != nil {
				// The denial of a claimed identity is recorded before the
				// rejection is released; a failing record cannot un-reject.
				a.recordDenial(ctx.Context(), decision)
			}
			writeProblem(ctx, decision.rejectStatus, decision.rejectCode, decision.rejectMessage)
			return
		}
		if decision.subject != nil {
			// Required access record: an authenticated request is never
			// dispatched when its access fact cannot be persisted (audit-design
			// §2: 普通必需访问审计也不能在写入失败时静默放行). No handler code
			// runs, so no content can leak.
			decision.fact.Outcome = OutcomeGranted
			if err := a.sink.RecordAccess(decision.ctx, *decision.fact); err != nil {
				a.report(*decision.fact, err)
				writeProblem(ctx, http.StatusServiceUnavailable, "unavailable", "暂时无法记录访问，请稍后重试。")
				return
			}
		}
		next(huma.WithContext(ctx, decision.ctx))
		// The huma context shares its status slot across WithContext copies,
		// so the final handler status is observable here. Infrastructure
		// failures are recorded as request failures, never as denials.
		if status := ctx.Status(); status >= http.StatusBadRequest && decision.subject != nil {
			outcome := OutcomeDenied
			if status >= http.StatusInternalServerError {
				outcome = OutcomeRequestFailure
			}
			a.recordOutcome(decision.ctx, *decision.fact, outcome, status)
		}
	}
}

// Wrap wraps one raw mux handler (SSE, download, upload, maintenance
// catch-all) with the same admission semantics. The wrapped writer forwards
// every write and flush unbuffered — streaming handlers keep full control of
// the response — while the final status stays observable for outcome facts.
func (a *Admission) Wrap(declarationID string, next http.Handler) (http.Handler, error) {
	declaration, ok := a.registry.Lookup(declarationID)
	if !ok {
		return nil, fmt.Errorf("operations: raw route %q has no access declaration", declarationID)
	}
	if err := a.markWrapped(declarationID); err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decision := a.admit(request.Context(), declaration,
			cookieFrom(request, sessionCookieName))
		if declaration.Method == "" {
			// Pattern declarations (any-method routes) record the actual method.
			decision.fact.Method = request.Method
		}
		if decision.rejectStatus != 0 {
			if decision.subject != nil {
				a.recordDenial(decision.finalize(), decision)
			}
			writeRawProblem(writer, decision.rejectStatus, decision.rejectCode, decision.rejectMessage)
			return
		}
		enriched := decision.finalize()
		if decision.subject != nil {
			decision.fact.Outcome = OutcomeGranted
			if err := a.sink.RecordAccess(enriched, *decision.fact); err != nil {
				a.report(*decision.fact, err)
				writeRawProblem(writer, http.StatusServiceUnavailable, "unavailable", "暂时无法记录访问，请稍后重试。")
				return
			}
		}
		recorder := &statusRecorder{ResponseWriter: writer}
		next.ServeHTTP(recorder, request.WithContext(enriched))
		if status := recorder.status; status >= http.StatusBadRequest && decision.subject != nil {
			outcome := OutcomeDenied
			if status >= http.StatusInternalServerError {
				outcome = OutcomeRequestFailure
			}
			a.recordOutcome(decision.ctx, *decision.fact, outcome, status)
		}
	}), nil
}

// admissionDecision carries the outcome of one admission pass.
type admissionDecision struct {
	fact    *AccessFact
	subject *Subject // claimed identity: audit duty exists when non-nil

	// Identity/correlation state finalized into the request context. It is
	// populated before any rejection or dispatch so denial records always
	// carry complete attribution.
	base          context.Context
	correlation   string
	actor         execution.Principal
	sessionRef    execution.SessionRef
	ctx           context.Context
	finalized     bool
	rejectStatus  int
	rejectCode    string
	rejectMessage string
}

func (d *admissionDecision) reject(status int, code, message string) {
	d.rejectStatus, d.rejectCode, d.rejectMessage = status, code, message
	if d.subject != nil {
		d.fact.ActorUserID = d.subject.UserID
	}
	if d.fact != nil && d.correlation != "" {
		d.fact.CorrelationID = d.correlation
	}
	d.finalize()
}

func (d admissionDecision) withReject(status int, code, message string) admissionDecision {
	d.reject(status, code, message)
	return d
}

// finalize builds the root execution metadata exactly once: one trusted
// correlation (resumed from the stored flow for flow operations), the actor,
// and the entry source. Handlers, executors and the audit sink inherit it.
func (d *admissionDecision) finalize() context.Context {
	if d.finalized {
		return d.ctx
	}
	d.finalized = true
	if d.fact != nil && d.correlation != "" {
		d.fact.CorrelationID = d.correlation
	}
	enriched, err := execution.WithMetadata(d.base, execution.Metadata{
		CorrelationID: d.correlation,
		Actor:         d.actor,
		Initiator:     d.actor,
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: d.fact.RequestID},
		Session:       d.sessionRef,
	})
	if err != nil {
		// Identity validation makes this unreachable; never dispatch on a
		// metadata failure.
		d.reject(http.StatusServiceUnavailable, "unavailable", "暂时无法处理请求，请稍后重试。")
		return d.base
	}
	d.ctx = enriched
	return d.ctx
}

// admit resolves the request identity, enforces the declared level and
// prepares the root execution metadata: one trusted correlation per request,
// resumed from the stored authentication flow for flow operations.
// Client-supplied values can never inject a correlation.
func (a *Admission) admit(ctx context.Context, declaration Declaration, sessionCredential string) admissionDecision {
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		a.report(AccessFact{OperationID: declaration.ID}, err)
		return admissionDecision{base: ctx, fact: &AccessFact{OperationID: declaration.ID}}.withReject(http.StatusServiceUnavailable, "unavailable", "暂时无法处理请求，请稍后重试。")
	}
	requestID, err := execution.NewCorrelationID()
	if err != nil {
		a.report(AccessFact{OperationID: declaration.ID}, err)
		return admissionDecision{base: ctx, fact: &AccessFact{OperationID: declaration.ID}}.withReject(http.StatusServiceUnavailable, "unavailable", "暂时无法处理请求，请稍后重试。")
	}
	decision := admissionDecision{
		base: ctx,
		fact: &AccessFact{
			OperationID:   declaration.ID,
			Method:        declaration.Method,
			Path:          declaration.Path,
			Level:         declaration.Level,
			Kind:          declaration.Kind,
			ObjectType:    declaration.ObjectType,
			CorrelationID: correlationID,
			RequestID:     requestID,
		},
		correlation: correlationID,
		actor:       execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
	}

	// Resolve the session credential (pure read). Resolution failures are
	// infrastructure faults and answer 503; only the sentinel means
	// "no valid session".
	var session *Subject
	if sessionCredential != "" {
		resolved, err := a.sessions(ctx, sessionCredential)
		switch {
		case err == nil:
			session = &resolved
		case IsErrUnauthenticated(err):
		default:
			a.report(*decision.fact, err)
			decision.reject(http.StatusServiceUnavailable, "unavailable", "暂时无法验证登录状态，请稍后重试。")
			return decision
		}
	}

	applySession := func(subject *Subject) {
		decision.subject = subject
		decision.sessionRef = execution.SessionRef{ID: subject.SessionID, AuthRevision: subject.AuthRevision}
		decision.actor = execution.Principal{Kind: execution.PrincipalUser, ID: subject.UserID}
	}

	switch declaration.Level {
	case LevelPublic:
		// The flow-start surface: no identity is required, anonymous traffic
		// stays out of the business audit table.

	case LevelSession:
		if session == nil {
			decision.reject(http.StatusUnauthorized, "unauthenticated", "请重新登录。")
			return decision
		}
		applySession(session)

	case LevelFull:
		if session == nil {
			decision.reject(http.StatusUnauthorized, "unauthenticated", "请重新登录。")
			return decision
		}
		if session.PasswordChangeRequired {
			applySession(session)
			decision.reject(http.StatusForbidden, "password_change_required", "请先完成临时密码修改后再使用该功能。")
			return decision
		}
		if !session.Initialized {
			applySession(session)
			decision.reject(http.StatusForbidden, "initialization_required", "请先完成账户初始化后再使用该功能。")
			return decision
		}
		applySession(session)

	case LevelAdmin:
		if session == nil {
			decision.reject(http.StatusUnauthorized, "unauthenticated", "请重新登录。")
			return decision
		}
		if !session.full() {
			applySession(session)
			decision.reject(http.StatusForbidden, "forbidden", "请先完成账户初始化后再使用该功能。")
			return decision
		}
		if session.Role != "admin" {
			applySession(session)
			decision.reject(http.StatusForbidden, "forbidden", "该操作需要管理员权限。")
			return decision
		}
		applySession(session)
	}

	if decision.subject != nil {
		decision.fact.ActorUserID = decision.subject.UserID
	}
	decision.finalize()
	return decision
}

func (a *Admission) recordDenial(ctx context.Context, decision admissionDecision) {
	denial := *decision.fact
	denial.Outcome = OutcomeDenied
	denial.Status = decision.rejectStatus
	if err := a.sink.RecordAccess(ctx, denial); err != nil {
		a.report(denial, err)
	}
}

func (a *Admission) recordOutcome(ctx context.Context, fact AccessFact, outcome Outcome, status int) {
	outcomeFact := fact
	outcomeFact.Outcome = outcome
	outcomeFact.Status = status
	if err := a.sink.RecordAccess(ctx, outcomeFact); err != nil {
		a.report(outcomeFact, err)
	}
}

func (a *Admission) report(fact AccessFact, err error) {
	if a.onError != nil {
		a.onError(fact, err)
	}
}

const (
	sessionCookieName = "__Host-quoin-session"
)

func cookieValue(cookieHeader, name string) string {
	if cookieHeader == "" {
		return ""
	}
	request := http.Request{Header: http.Header{"Cookie": []string{cookieHeader}}}
	if cookie, err := request.Cookie(name); err == nil {
		return cookie.Value
	}
	return ""
}

func cookieFrom(request *http.Request, name string) string {
	if cookie, err := request.Cookie(name); err == nil {
		return cookie.Value
	}
	return ""
}

// statusRecorder observes the response status without buffering anything:
// body writes, flushes, hijacks and sniffing behave exactly as the wrapped
// writer's, so SSE and download handlers keep their streaming contract.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(payload []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(payload)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

// problemBody mirrors the frozen problem envelope (HTTP-ERROR-001).
type problemBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func writeProblem(ctx huma.Context, status int, code, message string) {
	ctx.SetHeader("Content-Type", "application/problem+json")
	ctx.SetHeader("Cache-Control", "no-store")
	ctx.SetStatus(status)
	_ = json.NewEncoder(ctx.BodyWriter()).Encode(problemBody{Code: code, Message: message, Retryable: status >= 500 || status == 429})
}

func writeRawProblem(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(problemBody{Code: code, Message: message, Retryable: status >= 500 || status == 429})
}
