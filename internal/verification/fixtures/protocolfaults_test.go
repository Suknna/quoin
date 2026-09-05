package fixtures

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCorpusLoadsAndValidates(t *testing.T) {
	corpus, err := LoadCorpus("../../../testdata/external-protocol-faults")
	if err != nil {
		t.Fatal(err)
	}
	// The frozen catalog references the corpus as one digest-pinned
	// fixture; every closed-vocabulary behavior must be represented so
	// the monitoring-stack scenario cannot silently lose a class.
	represented := map[string]bool{}
	for _, fault := range corpus {
		represented[fault.Behavior] = true
	}
	for behavior := range behaviors {
		if !represented[behavior] {
			t.Fatalf("corpus does not represent behavior %q", behavior)
		}
	}
}

func TestCorpusRejectsUnknownVocabularyAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("bad-protocol.json", `{"id":"x","protocol":"grpc","behavior":"error_status","status":500}`)
	if _, err := LoadCorpus(dir); err == nil {
		t.Fatal("unknown protocol accepted")
	}
	write("bad-protocol.json", `{"id":"x","protocol":"http","behavior":"teapot","status":500}`)
	if _, err := LoadCorpus(dir); err == nil {
		t.Fatal("unknown behavior accepted")
	}
	write("bad-protocol.json", `{"id":"x","protocol":"http","behavior":"error_status","status":200}`)
	if _, err := LoadCorpus(dir); err == nil {
		t.Fatal("non-error status accepted for error_status")
	}
	write("bad-protocol.json", `{"id":"x","protocol":"http","behavior":"error_status","status":500}`)
	write("duplicate.json", `{"id":"x","protocol":"http","behavior":"app_timeout"}`)
	if _, err := LoadCorpus(dir); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if _, err := LoadCorpus(t.TempDir()); err == nil {
		t.Fatal("empty corpus accepted")
	}
}

// TestReplayObservedClasses proves every replayed fault yields exactly
// its closed client-side class through a real HTTP client.
func TestReplayObservedClasses(t *testing.T) {
	corpus, err := LoadCorpus("../../../testdata/external-protocol-faults")
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range corpus {
		fault := fault
		t.Run(fault.ID, func(t *testing.T) {
			server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				fault.Serve(writer, request)
			})}
			listener := newLocalListener(t, server)
			defer func() { _ = server.Close() }()
			client := &http.Client{}
			observation := Observe(client, "http://"+listener.Addr().String()+"/probe", fault, 2*time.Second)
			want := fault.Behavior + "_observed"
			if observation.ClientClass != want {
				t.Fatalf("class %q, want %q: %+v", observation.ClientClass, want, observation)
			}
		})
	}
}

func TestMalformedBodyObservationCarriesInvalidJSONFlag(t *testing.T) {
	fault := ProtocolFault{ID: "malformed", Protocol: "http", Behavior: "malformed_body", Body: "{broken"}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fault.Serve(writer, request)
	})}
	listener := newLocalListener(t, server)
	defer func() { _ = server.Close() }()
	observation := Observe(&http.Client{}, "http://"+listener.Addr().String()+"/probe", fault, 2*time.Second)
	if !observation.BodyInvalid {
		t.Fatalf("malformed body must be flagged invalid: %+v", observation)
	}
	if !strings.Contains(observation.BodyPrefix, "broken") {
		t.Fatalf("body prefix not retained: %+v", observation)
	}
}

func newLocalListener(t *testing.T, server *http.Server) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	return listener
}
