package plugins

// Post-commit fact declaration tests (ADR-0014): the subscription list is a
// compile-time, freeze-validated contract — unknown vocabulary entries,
// duplicate subscriptions and handler/subscription splits all fail
// registration instead of silently no-oping at runtime.

import (
	"context"
	"errors"
	"testing"
)

type stubHandler struct {
	calls int
	err   error
}

func (stub *stubHandler) HandlePostCommitFact(context.Context, PostCommitFact) error {
	stub.calls++
	return stub.err
}

func testPluginWithSubscriptions(id string, subscriptions []PostCommitSubscription, handler PostCommitHandler) Plugin {
	return Plugin{ID: id, Version: "v1-test", PostCommitSubscriptions: subscriptions, PostCommitHandler: handler}
}

func TestPostCommitDeclarationAcceptsVocabularyMember(t *testing.T) {
	registry := NewRegistry()
	err := registry.Register(testPluginWithSubscriptions("hooked", []PostCommitSubscription{
		{EventType: FactAlertObservationCommitted},
		{EventType: FactInspectionReportSealed},
	}, &stubHandler{}))
	if err != nil {
		t.Fatalf("valid declaration rejected: %v", err)
	}
	if len(registry.Plugins()) != 1 {
		t.Fatalf("valid declaration must freeze into the assembly")
	}
}

func TestPostCommitDeclarationRejectsUnknownFactType(t *testing.T) {
	registry := NewRegistry()
	err := registry.Register(testPluginWithSubscriptions("hooked", []PostCommitSubscription{
		{EventType: "quoin.unknown.fact"},
	}, &stubHandler{}))
	if !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("unknown fact type must fail registration, got %v", err)
	}
}

func TestPostCommitDeclarationRejectsDuplicateSubscription(t *testing.T) {
	registry := NewRegistry()
	err := registry.Register(testPluginWithSubscriptions("hooked", []PostCommitSubscription{
		{EventType: FactAlertObservationCommitted},
		{EventType: FactAlertObservationCommitted},
	}, &stubHandler{}))
	if !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("duplicate subscription must fail registration, got %v", err)
	}
}

func TestPostCommitDeclarationRejectsHandlerWithoutSubscriptions(t *testing.T) {
	registry := NewRegistry()
	err := registry.Register(testPluginWithSubscriptions("hooked", nil, &stubHandler{}))
	if !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("handler without subscriptions must fail registration, got %v", err)
	}
}

func TestPostCommitDeclarationRejectsSubscriptionsWithoutHandler(t *testing.T) {
	registry := NewRegistry()
	err := registry.Register(testPluginWithSubscriptions("hooked", []PostCommitSubscription{
		{EventType: FactAlertObservationCommitted},
	}, nil))
	if !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("subscriptions without handler must fail registration, got %v", err)
	}
}

func TestPostCommitDeclarationRejectsRegistrationAfterFreeze(t *testing.T) {
	registry := NewRegistry()
	if len(registry.Plugins()) != 0 {
		t.Fatal("fresh registry is empty")
	}
	err := registry.Register(testPluginWithSubscriptions("late", []PostCommitSubscription{
		{EventType: FactAlertObservationCommitted},
	}, &stubHandler{}))
	if !errors.Is(err, ErrRegistryFrozen) {
		t.Fatalf("registration after freeze must fail, got %v", err)
	}
}

func TestPostCommitSubscribersResolveEnabledInStableOrder(t *testing.T) {
	registry := NewRegistry()
	first := &stubHandler{}
	second := &stubHandler{}
	for _, plugin := range []Plugin{
		// One plugin subscribing to two fact types resolves under both.
		testPluginWithSubscriptions("alpha-sub", []PostCommitSubscription{
			{EventType: FactAlertObservationCommitted},
			{EventType: FactInspectionReportSealed},
		}, first),
		testPluginWithSubscriptions("beta-sub", []PostCommitSubscription{{EventType: FactAlertObservationCommitted}}, second),
		testPluginWithSubscriptions("gamma-sub", []PostCommitSubscription{{EventType: FactAlertObservationCommitted}}, &stubHandler{}),
	} {
		if err := registry.Register(plugin); err != nil {
			t.Fatalf("register %s: %v", plugin.ID, err)
		}
	}

	all := registry.PostCommitSubscribers(FactAlertObservationCommitted, []string{"alpha-sub", "beta-sub", "gamma-sub"})
	if len(all) != 3 {
		t.Fatalf("expected 3 enabled subscribers, got %d", len(all))
	}
	if all[0].PluginID != "alpha-sub" || all[1].PluginID != "beta-sub" || all[2].PluginID != "gamma-sub" {
		t.Fatalf("subscribers must resolve in stable plugin-ID order, got %v", []string{all[0].PluginID, all[1].PluginID, all[2].PluginID})
	}

	onlyGamma := registry.PostCommitSubscribers(FactAlertObservationCommitted, []string{"gamma-sub"})
	if len(onlyGamma) != 1 || onlyGamma[0].PluginID != "gamma-sub" {
		t.Fatalf("enablement must filter subscribers, got %v", onlyGamma)
	}
	if none := registry.PostCommitSubscribers(FactInspectionReportSealed, []string{"gamma-sub"}); len(none) != 0 {
		t.Fatalf("non-subscribed type must resolve empty, got %v", none)
	}
}

func TestValidPostCommitFactType(t *testing.T) {
	for _, factType := range []string{
		FactAlertObservationCommitted, FactInspectionDailyWindowDue,
		FactInspectionCheckEvidenceCommitted, FactInspectionReportSealed,
	} {
		if !ValidPostCommitFactType(factType) {
			t.Fatalf("%q is vocabulary", factType)
		}
	}
	for _, factType := range []string{"", "alert.committed", "quoin.made.up"} {
		if ValidPostCommitFactType(factType) {
			t.Fatalf("%q is outside the vocabulary", factType)
		}
	}
}
