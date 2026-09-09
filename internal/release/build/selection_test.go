package main

import (
	"bytes"
	"io"
	"os"
	"reflect"
	"testing"
)

func TestSelectBuildSubjects(t *testing.T) {
	tests := []struct {
		name    string
		changed []string
		want    []string
	}{
		{
			name:    "any authoritative proto rebuilds every application",
			changed: []string{"docs/specs/quoin-v1/contracts/runtime.proto"},
			want:    []string{"frontend", "lintel", "plinth", "quoin", "stele"},
		},
		{
			name:    "worker proto rebuilds every application",
			changed: []string{"docs/specs/quoin-v1/contracts/quoin/plinth/worker/v1/agent_worker.proto"},
			want:    []string{"frontend", "lintel", "plinth", "quoin", "stele"},
		},
		{
			name:    "openapi rebuilds API producer and consumer",
			changed: []string{"docs/specs/quoin-v1/contracts/openapi.yaml"},
			want:    []string{"frontend", "quoin"},
		},
		{
			name:    "component implementation is independently buildable",
			changed: []string{"cmd/stele/main.go", "internal/stele/relay.go"},
			want:    []string{"stele"},
		},
		{
			name:    "frontend implementation is independently buildable",
			changed: []string{"web/src/App.tsx"},
			want:    []string{"frontend"},
		},
		{
			name:    "frontend image inputs rebuild only the frontend",
			changed: []string{"deploy/images/frontend/web-caddy.yaml"},
			want:    []string{"frontend"},
		},
		{
			name:    "frontend Dockerfile rebuilds only the frontend",
			changed: []string{"deploy/images/frontend/Dockerfile"},
			want:    []string{"frontend"},
		},
		{
			name:    "backend Dockerfile rebuilds its component",
			changed: []string{"deploy/images/lintel/Dockerfile"},
			want:    []string{"lintel"},
		},
		{
			name:    "shared image build script rebuilds every application",
			changed: []string{"deploy/images/build.sh"},
			want:    []string{"frontend", "lintel", "plinth", "quoin", "stele"},
		},
		{
			name:    "workspace metadata rebuilds frontend and lintel",
			changed: []string{"pnpm-lock.yaml"},
			want:    []string{"frontend", "lintel"},
		},
		{
			name:    "lintel catalog rebuilds its Quoin consumer",
			changed: []string{"internal/lintel/catalog/catalog.go"},
			want:    []string{"lintel", "quoin"},
		},
		{
			name:    "unclassified internal source conservatively rebuilds backends",
			changed: []string{"internal/example/shared.go"},
			want:    []string{"lintel", "plinth", "quoin", "stele"},
		},
		{
			name:    "shared generated contracts rebuild every backend",
			changed: []string{"internal/gen/contracts/release-inputs.yaml"},
			want:    []string{"lintel", "plinth", "quoin", "stele"},
		},
		{
			name:    "stock Caddy is never a build subject",
			changed: []string{"deploy/caddy/Caddyfile"},
			want:    nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := selectBuildSubjects(test.changed); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("selectBuildSubjects(%v) = %v, want %v", test.changed, got, test.want)
			}
		})
	}
}

func TestSelectModeOutputsPolicySelection(t *testing.T) {
	for _, test := range []struct {
		changed string
		want    []byte
	}{
		{"deploy/caddy/Caddyfile", []byte(`"components":[]`)},
		{"deploy/images/frontend/web-caddy.yaml", []byte(`"components":["frontend"]`)},
		{"deploy/images/build.sh", []byte(`"components":["frontend","lintel","plinth","quoin","stele"]`)},
		{"pnpm-lock.yaml", []byte(`"components":["frontend","lintel"]`)},
		{"internal/lintel/catalog/catalog.go", []byte(`"components":["lintel","quoin"]`)},
	} {
		t.Run(test.changed, func(t *testing.T) {
			original := os.Stdout
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = write
			err = selectMode([]string{"-changed", test.changed})
			write.Close()
			os.Stdout = original
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(read)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(body, test.want) {
				t.Fatalf("output %s must contain %s", body, test.want)
			}
		})
	}
}
