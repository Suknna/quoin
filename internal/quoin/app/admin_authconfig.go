package app

// The read-only authentication configuration view (ADR-0010 / docs/
// authentication-design.md §8): administrators see the deployment-declared
// login channels with the secret reduced to a configured/not-configured
// fact. Editing stays with the deployment config file plus a restart — this
// endpoint never offers online mutation.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type adminAuthConfigEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type adminAuthConfigOutput struct {
	CacheControl string                 `header:"Cache-Control"`
	Body         adminAuthConfigEntries `json:"-"`
}

type adminAuthConfigEntries struct {
	Entries []adminAuthConfigEntry `json:"entries"`
}

func (application *apiServer) registerAdminAuthConfigRoute(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/admin/auth-config", OperationID: "getAdminAuthConfig"}, application.getAdminAuthConfig)
}

func (application *apiServer) getAdminAuthConfig(ctx context.Context, input *authInput) (*adminAuthConfigOutput, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "读取认证配置")
	if err != nil {
		return nil, err
	}
	_ = session
	config := application.authnConfig
	yes, no := "已启用", "已关闭"
	value := func(enabled bool) string {
		if enabled {
			return yes
		}
		return no
	}
	localEnabled := config.LocalEnabled()
	localVisible := config.LocalVisible()
	entries := []adminAuthConfigEntry{
		{Key: "本地登录（应急通道）", Value: value(localEnabled)},
		{Key: "登录页显示本地表单", Value: value(localVisible)},
		{Key: "OIDC 统一身份登录", Value: value(config.OIDCEnabled())},
	}
	if oidc := config.OIDC; oidc != nil {
		label := oidc.Label
		if label == "" {
			label = "（缺省：issuer 主机名）"
		}
		secretConfigured := "未配置"
		if config.SecretsFile != "" {
			secretConfigured = "已配置（来源 secretsFile，值不回显）"
		}
		entries = append(entries,
			adminAuthConfigEntry{Key: "OIDC Issuer", Value: oidc.Issuer},
			adminAuthConfigEntry{Key: "OIDC Client ID", Value: oidc.ClientID},
			adminAuthConfigEntry{Key: "OIDC 回调地址", Value: oidc.RedirectURL},
			adminAuthConfigEntry{Key: "SSO 按钮显示名", Value: label},
			adminAuthConfigEntry{Key: "OIDC Client Secret", Value: secretConfigured},
		)
	}
	output := &adminAuthConfigOutput{CacheControl: "no-store"}
	output.Body.Entries = entries
	return output, nil
}
