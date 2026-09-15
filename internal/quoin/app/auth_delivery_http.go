package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/secrets"
	"github.com/danielgtaylor/huma/v2"
)

type authDeliveryOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Configuration authDeliveryConfiguration `json:"configuration"`
		RowVersion    int64                     `json:"rowVersion"`
		Source        string                    `json:"source"`
		Configured    bool                      `json:"configured"`
	}
}

type authDeliveryInput struct {
	Flow    string `cookie:"__Host-quoin-flow"`
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		Configuration      authDeliveryConfiguration `json:"configuration"`
		Secrets            map[string]string         `json:"secrets,omitempty"`
		ExpectedRowVersion int64                     `json:"expectedRowVersion" minimum:"0"`
	}
}

func (application *apiServer) registerAuthDeliveryRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/auth/flow/delivery", OperationID: "readAuthDelivery"}, application.readAuthDelivery)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/auth/flow/delivery", OperationID: "configureAuthDelivery"}, application.configureAuthDelivery)
}

func (application *apiServer) deliveryActor(ctx context.Context, flow, session string) (context.Context, int64, error) {
	// The flow credential wins whenever it is present: during administrator
	// initialization no session can exist, and a stale session cookie left in
	// the browser (cookies are scoped by host, not port) must not lock the
	// delivery pane out of a perfectly valid initialization flow. Only the
	// admin_initialize flow may configure delivery — recovery is CLI-only and
	// re-enters through this same initialization flow.
	if flow != "" {
		state, err := application.auth.ReadFlow(ctx, flow)
		if err != nil || state.User.Role != "admin" || string(state.Type) != "admin_initialize" {
			return ctx, 0, problem(http.StatusForbidden, "forbidden", "只有管理员初始化流程可以配置验证投递。")
		}
		return ctx, state.User.ID, nil
	}
	if session != "" {
		actor, err := application.authenticateAdmin(ctx, session, "管理认证投递")
		if err != nil {
			return ctx, 0, err
		}
		return ctx, actor.User.ID, nil
	}
	return ctx, 0, problem(http.StatusForbidden, "forbidden", "只有管理员初始化流程可以配置验证投递。")
}

func (application *apiServer) readAuthDelivery(ctx context.Context, input *struct {
	Flow    string `cookie:"__Host-quoin-flow"`
	Session string `cookie:"__Host-quoin-session"`
},
) (*authDeliveryOutput, error) {
	if _, _, err := application.deliveryActor(ctx, input.Flow, input.Session); err != nil {
		return nil, err
	}
	config, _, version, source, err := application.loadAuthDelivery(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, authenticationError(err)
	}
	out := &authDeliveryOutput{CacheControl: "no-store"}
	out.Body.Configuration = config
	out.Body.RowVersion = version
	out.Body.Source = source
	out.Body.Configured = err == nil
	return out, nil
}

func (application *apiServer) configureAuthDelivery(ctx context.Context, input *authDeliveryInput) (*authDeliveryOutput, error) {
	ctx, actorID, err := application.deliveryActor(ctx, input.Flow, input.Session)
	if err != nil {
		return nil, err
	}
	_, previous, version, source, err := application.loadAuthDelivery(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, authenticationError(err)
	}
	if source == "deployment" {
		return nil, problem(http.StatusConflict, "deployment_owned", "投递配置由部署文件管理。")
	}
	if version != input.Body.ExpectedRowVersion {
		return nil, problem(http.StatusConflict, "row_version_conflict", "配置已变化，请刷新后重试。")
	}
	if previous == nil {
		previous = map[string]string{}
	}
	for name, value := range input.Body.Secrets {
		previous[name] = value
	}
	if err := validateAuthDelivery(input.Body.Configuration, previous); err != nil {
		return nil, problemUnprocessable("投递配置或秘密引用无效，请检查字段。")
	}
	key, err := application.rootKey()
	if err != nil {
		return nil, authenticationError(err)
	}
	var binding int
	if err := application.readAuthority().QueryRowContext(ctx, `SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&binding); err != nil {
		return nil, authenticationError(err)
	}
	encoded, err := json.Marshal(input.Body.Configuration)
	if err != nil {
		return nil, authenticationError(err)
	}
	secretBytes, err := json.Marshal(previous)
	if err != nil {
		return nil, authenticationError(err)
	}
	envelope, err := secrets.SealSetting(key, "auth.delivery", version+1, binding, secretBytes)
	if err != nil {
		return nil, authenticationError(err)
	}
	registry := execution.NewRegistry()
	op, err := registry.Register(execution.Operation{Name: "auth.delivery.configure", Class: execution.ClassWrite, ObjectType: "auth_delivery_settings", Authorize: func(ctx context.Context, tx *execution.Tx) error {
		var enabled int
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT enabled,role FROM users WHERE id=?`, actorID).Scan(&enabled, &role); err != nil {
			return err
		}
		if enabled != 1 || role != "admin" {
			return errors.New("administrator unavailable")
		}
		// Same credential precedence as deliveryActor: during administrator
		// initialization only the flow exists, and a stale session cookie
		// must not fail the in-transaction re-proof. The credential digest
		// must belong to the same administrator as the actor — a mismatched
		// or foreign credential fails closed.
		credential := input.Flow
		query := `SELECT COUNT(*) FROM auth_flows f JOIN users u ON u.id=f.user_id WHERE f.user_id=? AND f.flow_token_digest=? AND f.status='pending' AND f.flow_type='admin_initialize' AND f.auth_revision_at_issue=u.auth_revision AND julianday(f.expires_at)>julianday(?) AND julianday(f.expires_at)>julianday(?)`
		if credential == "" {
			credential = input.Session
			query = `SELECT COUNT(*) FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.user_id=? AND s.session_token_digest=? AND s.revoked_at IS NULL AND s.auth_revision_at_issue=u.auth_revision AND u.initialized=1 AND julianday(s.idle_expires_at)>julianday(?) AND julianday(s.absolute_expires_at)>julianday(?)`
		}
		raw, err := base64.RawURLEncoding.DecodeString(credential)
		if err != nil || len(raw) != 32 {
			return errors.New("invalid authentication credential")
		}
		digest := sha256.Sum256(raw)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		var valid int
		if err := tx.QueryRowContext(ctx, query, actorID, digest[:], now, now).Scan(&valid); err != nil {
			return err
		}
		if valid != 1 {
			return errors.New("authentication credential expired or revoked")
		}
		return nil
	}})
	if err != nil {
		return nil, authenticationError(err)
	}
	_, err = execution.Execute(ctx, execution.NewRunner(application.db, registry, audit.NewWriter()), op, func(tx *execution.Tx) (struct{ ID int64 }, error) {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		result, err := tx.ExecContext(ctx, `INSERT INTO auth_delivery_settings(id,source,configuration_json,secret_nonce,secret_ciphertext,root_binding_revision,row_version,updated_at) VALUES(1,'administrator',?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET configuration_json=excluded.configuration_json,secret_nonce=excluded.secret_nonce,secret_ciphertext=excluded.secret_ciphertext,root_binding_revision=excluded.root_binding_revision,row_version=excluded.row_version,updated_at=excluded.updated_at WHERE auth_delivery_settings.row_version=? AND auth_delivery_settings.source='administrator'`, string(encoded), envelope.Nonce, envelope.Ciphertext, binding, version+1, now, version)
		if err != nil {
			return struct{ ID int64 }{}, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return struct{ ID int64 }{}, err
		}
		if n != 1 {
			return struct{ ID int64 }{}, &execution.Rejection{Code: "row_version_conflict", ObjectID: 1}
		}
		return struct{ ID int64 }{1}, nil
	}, func(v struct{ ID int64 }) int64 { return v.ID })
	if err != nil {
		return nil, authenticationError(err)
	}
	out := &authDeliveryOutput{CacheControl: "no-store"}
	out.Body.Configuration = input.Body.Configuration
	out.Body.RowVersion = version + 1
	out.Body.Source = "administrator"
	out.Body.Configured = true
	return out, nil
}
