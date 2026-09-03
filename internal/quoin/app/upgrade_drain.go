package app

import (
	"context"
	"errors"

	appconfig "github.com/Suknna/quoin/internal/quoin/app/config"
	appinspection "github.com/Suknna/quoin/internal/quoin/app/inspection"
	appinvestigation "github.com/Suknna/quoin/internal/quoin/app/investigation"
	appknowledge "github.com/Suknna/quoin/internal/quoin/app/knowledge"
	"github.com/Suknna/quoin/internal/quoin/auth"
)

// The upgrade-drain handler builders mirror register()'s durable wiring for
// exactly the seven frozen cancel operations. Dispatch closures resolve at
// request time: the live normal-mode process keeps its runtime dispatchers
// (a committed cancel fence must still reach the Runtime), while a
// maintenance-mode boot stays durable-only and every CancelDispatch stays
// nil-safe.

func (application *apiServer) upgradeDrainInvestigations() *appinvestigation.Handler {
	return &appinvestigation.Handler{
		Service: application.investigations,
		Authenticate: func(ctx context.Context, cookie string) (int64, error) {
			session, err := application.authenticateFull(ctx, cookie, "停止调查")
			if err != nil {
				return 0, err
			}
			return session.User.ID, nil
		},
		CancelDispatch: func(ctx context.Context, attemptID int64) error {
			if application.cancelDispatchFunc == nil {
				return errors.New("cancel dispatch not wired")
			}
			return application.cancelDispatchFunc(ctx, attemptID)
		},
	}
}

func (application *apiServer) upgradeDrainInspections() *appinspection.Handler {
	return &appinspection.Handler{
		Inspections: application.inspections,
		Authenticate: func(ctx context.Context, cookie string) (int64, error) {
			session, err := application.authenticateFull(ctx, cookie, "取消巡检")
			if err != nil {
				return 0, err
			}
			return session.User.ID, nil
		},
		CancelDispatch: func(ctx context.Context, attemptID int64) error {
			if application.inspectionCancelDispatchFunc == nil {
				return errors.New("inspection cancel dispatch not wired")
			}
			return application.inspectionCancelDispatchFunc(ctx, attemptID)
		},
	}
}

func (application *apiServer) upgradeDrainKnowledge() *appknowledge.Handler {
	return &appknowledge.Handler{
		Feedback:  application.feedbackService,
		Knowledge: application.knowledgeService,
		DispatchCancel: func(ctx context.Context, attemptID int64) error {
			if application.cancelDispatchFunc == nil {
				return errors.New("cancel dispatch not wired")
			}
			return application.cancelDispatchFunc(ctx, attemptID)
		},
		Authenticate: func(ctx context.Context, cookie string) (auth.Session, error) {
			return application.authenticateFull(ctx, cookie, "取消知识导入")
		},
	}
}

func (application *apiServer) upgradeDrainConfig() *appconfig.Handler {
	return &appconfig.Handler{
		Systems:   application.systems,
		Contracts: application.contracts,
		Authenticate: func(ctx context.Context, cookie string) (int64, error) {
			session, err := application.authenticateFull(ctx, cookie, "取消配置验证")
			if err != nil {
				return 0, err
			}
			return session.User.ID, nil
		},
		AuthenticateAdmin: func(ctx context.Context, cookie string) (int64, error) {
			session, err := application.authenticateAdmin(ctx, cookie, "取消配置验证")
			if err != nil {
				return 0, err
			}
			return session.User.ID, nil
		},
		CancelDispatch: func(ctx context.Context, attemptID int64) error {
			if application.cancelDispatchFunc == nil {
				return errors.New("cancel dispatch not wired")
			}
			return application.cancelDispatchFunc(ctx, attemptID)
		},
	}
}
