package app

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
	"github.com/Suknna/quoin/internal/quoin/upgrade"
	"github.com/danielgtaylor/huma/v2"
)

// upgradeGate swaps the live public HTTP surface between the normal handler
// and the Upgrade maintenance allowlist (HTTP-MAINT-001). It is installed
// only in the normal-mode process: entering Upgrade maintenance mid-flight
// keeps the Runtime control plane and the T12 sweeps alive so Cancelling
// work can still converge, while every non-allowlisted HTTP operation
// starts answering 503. Established read-only SSE streams end when the
// process stops; the drain window is operator-supervised.
type upgradeGate struct {
	normal  atomic.Pointer[http.Handler]
	current atomic.Pointer[http.Handler]
}

func newUpgradeGate(normalHandler http.Handler) *upgradeGate {
	gate := &upgradeGate{}
	gate.normal.Store(&normalHandler)
	gate.current.Store(&normalHandler)
	return gate
}

// SetMaintenance installs the allowlist and atomically swaps admission.
func (gate *upgradeGate) SetMaintenance(handler http.Handler) {
	gate.current.Store(&handler)
}

// Exit restores the normal surface after a successful exitMaintenance.
func (gate *upgradeGate) Exit() {
	if normal := gate.normal.Load(); normal != nil {
		gate.current.Store(normal)
	}
}

func (gate *upgradeGate) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler := gate.current.Load()
	(*handler).ServeHTTP(writer, request)
}

type prepareUpgradeInput struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}

// prepareUpgrade is the Admin's idempotent versioned entry into Upgrade
// maintenance (HTTP-MAINT-005). In the normal-mode process the committed
// entry immediately swaps the public surface to the maintenance allowlist;
// inside maintenance the same command continues the frozen revision and
// re-arms the pre-upgrade backup after a failure.
func (application *apiServer) prepareUpgrade(ctx context.Context, input *prepareUpgradeInput) (*maintenanceUpgradeOutput, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "准备升级维护")
	if err != nil {
		return nil, err
	}
	state, err := application.upgradeService.Prepare(ctx, upgrade.PrepareRequest{ActorID: session.User.ID, ClientCommandID: input.Body.ClientCommandID, ExpectedRowVersion: input.Body.ExpectedRowVersion})
	if err != nil {
		if errors.Is(err, upgrade.ErrConflict) || errors.Is(err, upgrade.ErrCommandReused) || errors.Is(err, maintenance.ErrConflict) || errors.Is(err, maintenance.ErrCommandReused) {
			return nil, problem(http.StatusConflict, "maintenance_conflict", "维护状态或安全清单已变化，请刷新后重试。")
		}
		return nil, huma.Error500InternalServerError("暂时无法进入升级维护，请重试。", err)
	}
	if entered := application.onUpgradeMaintenanceEntered; entered != nil {
		entered()
	}
	return &maintenanceUpgradeOutput{Status: http.StatusAccepted, CacheControl: "no-store", Body: maintenanceResponse(state)}, nil
}

type maintenanceUpgradeOutput struct {
	Status       int                      `exclude:"true"`
	CacheControl string                   `header:"Cache-Control"`
	Body         maintenanceStateResponse `json:"body"`
}

// enterUpgradeMaintenance converges the live process onto the maintenance
// surface: readiness flips to maintenance mode, established browser tunnels
// close, and the reconciler wakes to publish the checklist.
func (application *apiServer) enterUpgradeMaintenance(publicOrigin string) {
	handler, err := newMaintenanceHandler(application, publicOrigin, "Upgrade")
	if err != nil {
		sharedops.LogEvent("quoin", "error", "upgrade.maintenance_surface_invalid", err.Error())
		return
	}
	if application.upgradeGate != nil {
		application.upgradeGate.SetMaintenance(handler)
	}
	if application.setReadiness != nil {
		application.setReadiness(sharedops.Readiness{Component: "quoin", Mode: "maintenance", AcceptingWork: false, Reason: sharedops.Maintenance})
	}
	if application.setMaintenanceReason != nil {
		application.setMaintenanceReason("Upgrade", true)
	}
	application.closeBrowserSessions(context.Background())
	if application.upgradeReconciler != nil {
		application.upgradeReconciler.Notify()
	}
}

// exitUpgradeMaintenance restores the normal surface and readiness after a
// successful Admin abort of the upgrade.
func (application *apiServer) exitUpgradeMaintenance() {
	if application.upgradeGate != nil {
		application.upgradeGate.Exit()
	}
	if application.setReadiness != nil {
		application.setReadiness(sharedops.Readiness{Component: "quoin", Mode: "normal", AcceptingWork: true, Reason: sharedops.Ready})
	}
	if application.setMaintenanceReason != nil {
		application.setMaintenanceReason("", false)
	}
	if application.upgradeReconciler != nil {
		application.upgradeReconciler.Notify()
	}
}

// registerUpgradeDrainRoutes is intentionally an explicit projection of the
// frozen upgrade-drain allowlist (openapi x-quoin-maintenance-access:
// upgrade-drain). Do not reuse the normal route registrars: they contain
// ordinary work-creation operations which stay closed throughout
// maintenance.
func (application *apiServer) registerUpgradeDrainRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}/cancel", OperationID: "cancelInitialAnalysis"}, application.cancelInitialAnalysis)
	application.upgradeDrainInvestigations().RegisterUpgradeDrain(api)
	application.upgradeDrainInspections().RegisterUpgradeDrain(api)
	application.upgradeDrainKnowledge().RegisterUpgradeDrain(api)
	application.upgradeDrainConfig().RegisterUpgradeDrain(api)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/browser-login/{systemKey}/operations/{browserOperationId}/cancel", OperationID: "cancelBrowserLoginOperation"}, application.cancelBrowserLoginOperation)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/connections/{connectionName}/probe-attempts/{attemptId}/cancel", OperationID: "cancelConnectionProbeAttempt"}, application.cancelConnectionProbeAttempt)
}
