// Package ops owns the shared, ops-network-only health and metrics surface.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Reason string

const (
	Ready                 Reason = "ready"
	Starting              Reason = "starting"
	DependencyUnavailable Reason = "dependency_unavailable"
	StorageUnavailable    Reason = "storage_unavailable"
	RuntimeUnregistered   Reason = "runtime_unregistered"
	Maintenance           Reason = "maintenance"
	Draining              Reason = "draining"
)

type Readiness struct {
	Component     string `json:"component"`
	Release       string `json:"release"`
	Mode          string `json:"mode"`
	AcceptingWork bool   `json:"acceptingWork"`
	Reason        Reason `json:"reason"`
}

type Server struct {
	httpServer *http.Server
	mu         sync.RWMutex
	state      Readiness
	registry   *prometheus.Registry
	readyGauge prometheus.Gauge
	accepting  prometheus.Gauge
}

func New(component, address string, reason Reason) (*Server, error) {
	if component != "quoin" && component != "plinth" && component != "lintel" && component != "stele" {
		return nil, fmt.Errorf("unsupported component %q", component)
	}
	ready := reason == Ready
	state := Readiness{
		Component: component, Release: buildinfo.Release, Mode: "normal",
		AcceptingWork: ready, Reason: reason,
	}
	registry := prometheus.NewRegistry()
	server := &Server{state: state, registry: registry}
	registerComponentMetrics(component, registry, ready)
	server.readyGauge = registryGauge(registry, component+"_ready", readinessHelp(component), boolValue(ready))
	if component == "quoin" {
		server.accepting = registryGauge(registry, "quoin_accepting_work", "Whether Quoin is accepting new domain work.", boolValue(ready))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		current := server.Readiness()
		if current.Reason != Ready && current.Mode != "maintenance" {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(writer).Encode(current)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	server.httpServer = &http.Server{
		Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	return server, nil
}

// registerComponentMetrics preinitializes the T01-applicable gauge families from
// contracts/metrics.yaml. Label values stay inside the closed catalog sets; the
// acceptance test asserts exported families and label values against the catalog.
func registerComponentMetrics(component string, registry *prometheus.Registry, ready bool) {
	switch component {
	case "quoin":
		labeledGauge(registry, "quoin_maintenance", "Whether Quoin maintenance is active for each closed reason.", "maintenance_reason",
			[]string{"Restore", "Upgrade", "RootKeyRebind", "LintelRecovery"}, 0)
		registryGauge(registry, "quoin_upgrade_prepared", "Whether the current Upgrade maintenance revision is fully safe and has a succeeded pre-upgrade backup.", 0)
		labeledGauge(registry, "quoin_storage_writable", "Whether each Quoin persistent storage target passed the durability probe.", "quoin_storage",
			[]string{"data", "backup"}, 1)
	case "plinth":
		labeledGauge(registry, "plinth_storage_writable", "Whether each Plinth local storage target passed its write probe.", "plinth_storage",
			[]string{"state", "workspace"}, 1)
	case "lintel":
		labeledGauge(registry, "lintel_storage_writable", "Whether each Lintel state or shared-memory target passed its probe.", "lintel_storage",
			[]string{"state", "shm"}, 1)
	case "stele":
		registryGauge(registry, "stele_quoin_available", "Whether Stele can currently reach an accepted same-version Quoin relay service.", 0)
	}
}

func registryGauge(registry *prometheus.Registry, name, help string, value int) prometheus.Gauge {
	metric := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	metric.Set(float64(value))
	registry.MustRegister(metric)
	return metric
}

func boolValue(value bool) int {
	if value {
		return 1
	}
	return 0
}

func labeledGauge(registry *prometheus.Registry, name, help, label string, values []string, value int) {
	metric := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, []string{label})
	for _, candidate := range values {
		metric.WithLabelValues(candidate).Set(float64(value))
	}
	registry.MustRegister(metric)
}

func (server *Server) Run(ctx context.Context) error {
	LogEvent(server.state.Component, "info", "ops.listener_started", server.httpServer.Addr)
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		current := server.Readiness()
		LogEvent(current.Component, "info", "component.draining", "SIGTERM received; new admissions stopped")
		server.SetReadiness(Readiness{Component: current.Component, Release: buildinfo.Release, Mode: "normal", AcceptingWork: false, Reason: Draining})
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		<-timer.C
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
		defer cancel()
		err := server.httpServer.Shutdown(shutdownCtx)
		LogEvent(current.Component, "info", "component.stopped", "ops listener closed")
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		LogEvent(server.state.Component, "error", "ops.listener_failed", err.Error())
		return err
	}
}

func (server *Server) Handler() http.Handler {
	return server.httpServer.Handler
}

func (server *Server) Readiness() Readiness {
	server.mu.RLock()
	defer server.mu.RUnlock()
	return server.state
}

func (server *Server) SetReadiness(state Readiness) {
	server.mu.Lock()
	previous := server.state
	server.state = state
	server.mu.Unlock()
	if state.Reason != previous.Reason {
		LogEvent(state.Component, "info", "readiness.changed", "reason="+string(state.Reason))
	}
	if state.Reason == Ready || state.Mode == "maintenance" {
		server.readyGauge.Set(1)
	} else {
		server.readyGauge.Set(0)
	}
	if server.accepting != nil {
		if state.AcceptingWork {
			server.accepting.Set(1)
		} else {
			server.accepting.Set(0)
		}
	}
}

func readinessHelp(component string) string {
	switch component {
	case "quoin":
		return "Whether Quoin can serve its current ready-mode responsibilities."
	case "plinth":
		return "Whether Plinth can accept work from Quoin."
	case "lintel":
		return "Whether Lintel can accept browser work from Quoin."
	default:
		return "Whether Stele can authenticate and relay Alertmanager deliveries."
	}
}
