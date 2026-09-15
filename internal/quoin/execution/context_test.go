package execution

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWithMetadataAndRequireRoundTrip(t *testing.T) {
	meta := Metadata{
		CorrelationID: "corr-round-trip",
		Actor:         Principal{Kind: PrincipalUser, ID: 7},
		Source:        Source{Kind: SourceHTTP, RequestID: "req-9"},
	}
	ctx, err := WithMetadata(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("metadata missing after WithMetadata")
	}
	if got.CorrelationID != "corr-round-trip" || got.Actor != meta.Actor || got.Source != meta.Source {
		t.Fatalf("metadata=%+v", got)
	}
	// A missing initiator defaults to the actor.
	if got.Initiator != meta.Actor {
		t.Fatalf("initiator=%+v, want actor", got.Initiator)
	}
	required, err := Require(ctx)
	if err != nil || required.CorrelationID != "corr-round-trip" {
		t.Fatalf("require=%+v err=%v", required, err)
	}
}

func TestRequireWithoutMetadataFailsClosed(t *testing.T) {
	if _, err := Require(context.Background()); !errors.Is(err, ErrMissingContext) {
		t.Fatalf("err=%v, want ErrMissingContext", err)
	}
	// A nil context must not panic.
	if _, ok := FromContext(nil); ok {
		t.Fatal("nil context must not report metadata")
	}
}

func TestWithMetadataValidationRejectsInvalidMetadata(t *testing.T) {
	validActor := Principal{Kind: PrincipalUser, ID: 7}
	cases := []struct {
		name   string
		meta   Metadata
		reject string
	}{
		{"empty correlation", Metadata{Actor: validActor, Source: Source{Kind: SourceHTTP}}, "correlation id"},
		{"non-ascii correlation", Metadata{CorrelationID: "корреляция", Actor: validActor, Source: Source{Kind: SourceHTTP}}, "correlation id"},
		{"control character correlation", Metadata{CorrelationID: "corr\nid", Actor: validActor, Source: Source{Kind: SourceHTTP}}, "correlation id"},
		{"oversized correlation", Metadata{CorrelationID: strings.Repeat("a", 129), Actor: validActor, Source: Source{Kind: SourceHTTP}}, "correlation id"},
		{"invalid actor kind", Metadata{CorrelationID: "corr", Actor: Principal{Kind: "robot", ID: 1}, Source: Source{Kind: SourceHTTP}}, "actor kind"},
		{"user actor without id", Metadata{CorrelationID: "corr", Actor: Principal{Kind: PrincipalUser, ID: 0}, Source: Source{Kind: SourceHTTP}}, "positive principal id"},
		{"system actor with row id", Metadata{CorrelationID: "corr", Actor: Principal{Kind: PrincipalSystem, ID: 4}, Source: Source{Kind: SourceHTTP}}, "id 0"},
		{"invalid initiator id", Metadata{CorrelationID: "corr", Actor: validActor, Initiator: Principal{Kind: PrincipalUser, ID: 0}, Source: Source{Kind: SourceHTTP}}, "initiator"},
		{"invalid source kind", Metadata{CorrelationID: "corr", Actor: validActor, Source: Source{Kind: "cron"}}, "source kind"},
		{"control character request id", Metadata{CorrelationID: "corr", Actor: validActor, Source: Source{Kind: SourceHTTP, RequestID: "req\t1"}}, "request id"},
	}
	for _, testCase := range cases {
		if _, err := WithMetadata(context.Background(), testCase.meta); err == nil || !strings.Contains(err.Error(), testCase.reject) {
			t.Fatalf("%s: err=%v, want rejection mentioning %q", testCase.name, err, testCase.reject)
		}
	}
}

func TestWithMetadataRejectsCorrelationReplacement(t *testing.T) {
	ctx, err := WithMetadata(context.Background(), Metadata{
		CorrelationID: "corr-root",
		Actor:         Principal{Kind: PrincipalUser, ID: 7},
		Source:        Source{Kind: SourceHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WithMetadata(ctx, Metadata{
		CorrelationID: "corr-other",
		Actor:         Principal{Kind: PrincipalUser, ID: 7},
		Source:        Source{Kind: SourceHTTP},
	}); err == nil || !strings.Contains(err.Error(), "already carries execution metadata") {
		t.Fatalf("err=%v, want replacement rejection", err)
	}
	if got, _ := FromContext(ctx); got.CorrelationID != "corr-root" {
		t.Fatalf("correlation=%q after rejected replacement", got.CorrelationID)
	}
}

func TestReplaceMetadataRerootsDeliberately(t *testing.T) {
	// Restore flow: persisted task metadata reattached to a fresh task
	// context, even when that context somehow already carries one.
	fresh, err := WithMetadata(context.Background(), Metadata{
		CorrelationID: "corr-stale",
		Actor:         Principal{Kind: PrincipalService, ID: 2},
		Source:        Source{Kind: SourceTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ReplaceMetadata(fresh, Metadata{
		CorrelationID: "corr-persisted-task",
		Actor:         Principal{Kind: PrincipalSystem, ID: 0},
		Initiator:     Principal{Kind: PrincipalUser, ID: 7},
		Source:        Source{Kind: SourceTask, RequestID: "task-42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := FromContext(restored)
	if !ok || got.CorrelationID != "corr-persisted-task" || got.Actor.Kind != PrincipalSystem || got.Initiator.ID != 7 {
		t.Fatalf("restored=%+v ok=%v", got, ok)
	}
	if _, err := ReplaceMetadata(nil, Metadata{}); err == nil {
		t.Fatal("nil context must be rejected")
	}
}

func TestDelegatePreservesCorrelationAndInitiator(t *testing.T) {
	ctx, err := WithMetadata(context.Background(), Metadata{
		CorrelationID: "corr-delegate",
		Actor:         Principal{Kind: PrincipalUser, ID: 7},
		Source:        Source{Kind: SourceHTTP, RequestID: "req-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	delegated, err := Delegate(ctx, Principal{Kind: PrincipalService, ID: 3})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := FromContext(delegated)
	if !ok {
		t.Fatal("delegated metadata missing")
	}
	if got.CorrelationID != "corr-delegate" || got.Initiator != (Principal{Kind: PrincipalUser, ID: 7}) {
		t.Fatalf("delegated=%+v, want preserved correlation and initiator", got)
	}
	if got.Actor != (Principal{Kind: PrincipalService, ID: 3}) {
		t.Fatalf("delegated actor=%+v, want executing principal", got.Actor)
	}
	if _, err := Delegate(context.Background(), Principal{Kind: PrincipalSystem, ID: 0}); !errors.Is(err, ErrMissingContext) {
		t.Fatalf("err=%v, want ErrMissingContext", err)
	}
}

func TestSessionReferenceValidationAndDelegation(t *testing.T) {
	userActor := Principal{Kind: PrincipalUser, ID: 7}
	base := Metadata{
		CorrelationID: "corr-session",
		Actor:         userActor,
		Source:        Source{Kind: SourceHTTP, RequestID: "req-5"},
	}

	// Both zero: flows and background execution without a session.
	if _, err := WithMetadata(context.Background(), base); err != nil {
		t.Fatalf("sessionless metadata rejected: %v", err)
	}
	// Both positive: a user-origin command carrying its session proof.
	withSession := base
	withSession.Session = SessionRef{ID: 42, AuthRevision: 3}
	ctx, err := WithMetadata(context.Background(), withSession)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := FromContext(ctx); got.Session != (SessionRef{ID: 42, AuthRevision: 3}) {
		t.Fatalf("session=%+v, want the attached reference", got.Session)
	}

	cases := []struct {
		name   string
		meta   Metadata
		reject string
	}{
		{"session id without revision", func() Metadata { m := base; m.Session = SessionRef{ID: 42}; return m }(), "both session id and auth revision"},
		{"revision without session id", func() Metadata { m := base; m.Session = SessionRef{AuthRevision: 3}; return m }(), "both session id and auth revision"},
		{"negative session id", func() Metadata { m := base; m.Session = SessionRef{ID: -1, AuthRevision: 3}; return m }(), "both session id and auth revision"},
		{"session on service actor", func() Metadata {
			m := base
			m.Actor = Principal{Kind: PrincipalService, ID: 2}
			m.Session = SessionRef{ID: 42, AuthRevision: 3}
			return m
		}(), "cannot carry a session proof"},
		{"session on system actor", func() Metadata {
			m := base
			m.Actor = Principal{Kind: PrincipalSystem, ID: 0}
			m.Session = SessionRef{ID: 42, AuthRevision: 3}
			return m
		}(), "cannot carry a session proof"},
	}
	for _, testCase := range cases {
		if _, err := WithMetadata(context.Background(), testCase.meta); err == nil || !strings.Contains(err.Error(), testCase.reject) {
			t.Fatalf("%s: err=%v, want rejection mentioning %q", testCase.name, err, testCase.reject)
		}
	}

	// Delegation clears the session proof (the background principal
	// authorizes by its own identity) while retaining initiator and
	// correlation.
	delegated, err := Delegate(ctx, Principal{Kind: PrincipalService, ID: 3})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := FromContext(delegated)
	if !ok {
		t.Fatal("delegated metadata missing")
	}
	if got.Session != (SessionRef{}) {
		t.Fatalf("delegated session=%+v, want cleared", got.Session)
	}
	if got.Initiator != userActor || got.CorrelationID != "corr-session" {
		t.Fatalf("delegated initiator=%+v correlation=%q, want preserved", got.Initiator, got.CorrelationID)
	}

	// A service actor delegation that tries to reattach a session proof is
	// rejected by validation.
	reattached := Metadata{
		CorrelationID: "corr-session",
		Actor:         Principal{Kind: PrincipalService, ID: 3},
		Initiator:     userActor,
		Source:        Source{Kind: SourceTask},
		Session:       SessionRef{ID: 42, AuthRevision: 3},
	}
	if _, err := ReplaceMetadata(context.Background(), reattached); err == nil || !strings.Contains(err.Error(), "cannot carry a session proof") {
		t.Fatalf("err=%v, want session proof rejection on service actor", err)
	}
}

func TestNewCorrelationIDIsBoundedUniqueASCII(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewCorrelationID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 32 || seen[id] {
			t.Fatalf("id=%q len=%d duplicate=%v", id, len(id), seen[id])
		}
		seen[id] = true
		if _, err := WithMetadata(context.Background(), Metadata{
			CorrelationID: id,
			Actor:         Principal{Kind: PrincipalSystem, ID: 0},
			Source:        Source{Kind: SourceInternal},
		}); err != nil {
			t.Fatalf("generated id rejected: %v", err)
		}
	}
}
