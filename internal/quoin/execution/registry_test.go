package execution

import (
	"context"
	"strings"
	"testing"
)

func TestRegistryRegisterValidatesDeclarations(t *testing.T) {
	registry := NewRegistry()
	cases := []struct {
		name string
		op   Operation
		want string
	}{
		{"missing name", Operation{Class: ClassWrite, ObjectType: "quoin_item"}, "operation name is required"},
		{"invalid class", Operation{Name: "op.a", Class: "upsert", ObjectType: "quoin_item"}, "class read or write"},
		{"missing object type", Operation{Name: "op.b", Class: ClassWrite}, "object type"},
	}
	for _, testCase := range cases {
		if _, err := registry.Register(testCase.op); err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: err=%v, want rejection mentioning %q", testCase.name, err, testCase.want)
		}
	}
}

func TestRegistryRejectsDuplicateNamesAndReturnsCanonicalPointer(t *testing.T) {
	registry := NewRegistry()
	first, err := registry.Register(Operation{
		Name:       "item.create",
		Class:      ClassWrite,
		ObjectType: "quoin_item",
		Authorize:  func(ctx context.Context, tx *Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(Operation{Name: "item.create", Class: ClassWrite, ObjectType: "other"}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("err=%v, want duplicate rejection", err)
	}
	lookup, ok := registry.Lookup("item.create")
	if !ok || lookup != first {
		t.Fatalf("lookup=%v ok=%v, want the registered pointer", lookup, ok)
	}
	if _, ok := registry.Lookup("missing.op"); ok {
		t.Fatal("unknown operation must not resolve")
	}
}
