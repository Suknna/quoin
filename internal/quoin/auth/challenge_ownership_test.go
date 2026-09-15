package auth_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

func TestChallengeDispatchRejectsForeignContactBeforeDelivery(t *testing.T) {
	service, sender, db := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	_, session, _ := flowLogin(t, service, sender, "admin", fixtureAdminPassword)
	if _, _, err := service.CreateUser(ctx, session, auth.CreateUserInput{
		ClientCommandID: "foreign-contact-test", Digest: auth.DigestCommand("user.create", map[string]any{"username": "op1"}),
		Username: "op1", DisplayName: "Operator", Role: "operator", Password: fixtureOperatorTempPass,
		Contacts: []auth.ContactInput{{Channel: "email", Target: fixtureOperatorEmail}},
	}); err != nil {
		t.Fatal(err)
	}
	flow, _, err := service.StartOperatorInitialization(ctx, "op1", fixtureOperatorTempPass)
	if err != nil {
		t.Fatal(err)
	}
	var foreignID int64
	if err := db.QueryRow(`SELECT id FROM user_contacts WHERE user_id=?`, session.User.ID).Scan(&foreignID); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	sent := sender.count()
	masked, _, err := service.SendFlowChallenge(ctx, flow.Bearer, fmt.Sprint(foreignID))
	if err == nil || masked.MaskedTarget != "" {
		t.Fatalf("foreign contact accepted or disclosed: %+v, %v", masked, err)
	}
	if sender.count() != sent {
		t.Fatal("foreign contact triggered a delivery")
	}
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("foreign contact consumed persisted send budget")
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, flow.Contacts[0].Locator); err != nil {
		t.Fatalf("rejected foreign contact must not consume own send opportunity: %v", err)
	}
}
