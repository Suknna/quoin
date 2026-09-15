package app

import (
	"context"
	"database/sql"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

func prepareAuthenticationBootstrap(ctx context.Context, service *auth.Service, _ *sql.DB, _ string, retentionMonths ...int) error {
	_, err := service.EnsureBootstrapAdmin(ctx, retentionMonths...)
	return err
}
