package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
)

// initialAdminPasswordFile is the 0600 bootstrap credential file written into
// the data directory (the same writable volume SQLite lives on, so read-only
// root filesystems need no extra mount). The startup log announces the path
// and deadline; the password itself never reaches any log.
const initialAdminPasswordFile = "initial-admin-password"

// prepareAuthenticationBootstrap seeds the pending built-in administrator on
// an empty database with a generated initial password whose 24-hour deadline
// lives on the users row. The file is written before the seed transaction: a
// rolled-back seed leaves an orphaned file, but the next boot finds the users
// table still empty and regenerates both — the pair can never drift apart
// for longer than one failed boot.
func prepareAuthenticationBootstrap(ctx context.Context, service *auth.Service, dataDirectory string, retentionMonths ...int) error {
	seeded, err := service.HasUsers(ctx)
	if err != nil {
		return err
	}
	if seeded {
		// Idempotent restart path: an existing deployment never re-arms a
		// credential. A completed initialization also clears any lingering
		// file from the (already dead) initial password on the next boot.
		pending, err := service.HasPendingBootstrapAdmin(ctx)
		if err != nil {
			return err
		}
		if !pending {
			removeInitialPasswordFile(dataDirectory)
		}
		return nil
	}
	password, err := auth.GenerateInitialPassword()
	if err != nil {
		return err
	}
	if err := writeInitialPasswordFile(dataDirectory, password); err != nil {
		return err
	}
	created, err := service.EnsureBootstrapAdmin(ctx, password, time.Now().UTC().Add(auth.InitialPasswordLifetime), retentionMonths...)
	if err != nil {
		return err
	}
	if created {
		slog.Info("initial administrator password generated",
			"code", "auth.bootstrap.initial_password",
			"path", initialPasswordPath(dataDirectory),
			"validFor", auth.InitialPasswordLifetime.String())
	}
	return nil
}

func initialPasswordPath(dataDirectory string) string {
	return filepath.Join(dataDirectory, initialAdminPasswordFile)
}

func writeInitialPasswordFile(dataDirectory, password string) error {
	path := initialPasswordPath(dataDirectory)
	// Write-then-rename keeps the file either absent or complete.
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(password+"\n"), 0o600); err != nil {
		return fmt.Errorf("write initial admin password file: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish initial admin password file: %w", err)
	}
	return nil
}

// removeInitialPasswordFile clears the bootstrap credential file after the
// password became irrelevant (deployment already seeded or the administrator
// completed the forced change); a missing file is not an error.
func removeInitialPasswordFile(dataDirectory string) {
	_ = os.Remove(initialPasswordPath(dataDirectory))
}

// configureLoginProviders assembles the provider registry from the
// deployment config (ADR-0010). The double-false combination is a deployment
// error and fails fast; the local channel always registers (even hidden) so
// /api/v1/auth/config projects a stable shape.
func (application *apiServer) configureLoginProviders(config *contract.QuoinAuthenticationConfig) error {
	application.authnConfig = config
	localEnabled := config.LocalEnabled()
	oidcEnabled := config.OIDCEnabled()
	if !localEnabled && !oidcEnabled {
		return fmt.Errorf("authentication.local.enabled and authentication.oidc.enabled are both false: at least one login channel must be enabled")
	}
	registry := auth.NewRegistry()
	if err := registry.Register(auth.LocalProvider{
		Service: application.auth,
		Enabled: localEnabled,
		Visible: config.LocalVisible(),
	}); err != nil {
		return err
	}
	if oidcEnabled {
		if application.rootKey == nil {
			return fmt.Errorf("the oidc login channel requires the deployment root key")
		}
		secrets, err := readAuthSecretsFile(config.SecretsFile)
		if err != nil {
			return err
		}
		clientSecret := secrets["oidcClientSecret"]
		if clientSecret == "" {
			return fmt.Errorf("authentication.secretsFile must provide the oidcClientSecret reference")
		}
		rootKey, err := application.rootKey()
		if err != nil {
			return fmt.Errorf("read root key for the oidc state signing key: %w", err)
		}
		mac := hmac.New(sha256.New, rootKey)
		mac.Write([]byte("quoin:authentication:oidc:v1"))
		if err := registry.Register(&auth.OIDCProvider{
			Service:      application.auth,
			Issuer:       config.OIDC.Issuer,
			ClientID:     config.OIDC.ClientID,
			ClientSecret: clientSecret,
			RedirectURL:  config.OIDC.RedirectURL,
			Label:        config.OIDC.Label,
			IconURL:      config.OIDC.IconURL,
			StateKey:     mac.Sum(nil),
		}); err != nil {
			return err
		}
	}
	application.providers = registry
	return nil
}
