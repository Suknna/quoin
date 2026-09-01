// Package ops owns the shared, ops-network-only health and metrics surface.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
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
	httpServer      *http.Server
	mu              sync.RWMutex
	state           Readiness
	registry        *prometheus.Registry
	readyGauge      prometheus.Gauge
	accepting       prometheus.Gauge
	collectors      map[string]prometheus.Collector
	storageWritable map[string]bool
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
	// The deployment backup observer fences a metrics sampling window by the
	// standard process start timestamp. It is an upstream collector, not part
	// of the frozen custom metrics catalog.
	registry.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	server := &Server{state: state, registry: registry}
	collectors, err := registerCatalogMetrics(registry, component)
	if err != nil {
		return nil, fmt.Errorf("project frozen metrics catalog for %s: %w", component, err)
	}
	readyGauge, ok := collectors[component+"_ready"].(prometheus.Gauge)
	if !ok {
		return nil, fmt.Errorf("metrics catalog is missing the %s_ready gauge", component)
	}
	server.readyGauge = readyGauge
	server.readyGauge.Set(float64(boolValue(ready)))
	server.collectors = collectors
	if component == "quoin" {
		server.storageWritable = map[string]bool{"data": true, "backup": true}
	}
	if component == "quoin" {
		accepting, ok := collectors["quoin_accepting_work"].(prometheus.Gauge)
		if !ok {
			return nil, fmt.Errorf("metrics catalog is missing the quoin_accepting_work gauge")
		}
		server.accepting = accepting
		server.accepting.Set(float64(boolValue(ready)))
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
		if current.Reason == StorageUnavailable || (current.Reason != Ready && current.Mode != "maintenance") {
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

// ProjectedLabelValues exposes one resolved values_from enum projection for
// the OPS-METRIC-001 fixture: the machine-check must read exactly what the
// components export, including the handwritten mirror in this package.
func ProjectedLabelValues(t interface{ Helper() }, source string) []string {
	t.Helper()
	values := append([]string(nil), enumLabelValues[source]...)
	sort.Strings(values)
	return values
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
		// Hold the drained-but-alive state for a bounded window so the frozen
		// contract is observable (OPS-HEALTH-006): /readyz already answers
		// 503 draining, /livez keeps answering 200 while the component can
		// still close out work. The hold stays far inside the reserved 15s
		// connection-close window of the 60s stop grace.
		timer := time.NewTimer(5 * time.Second)
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

// BackupMetrics exposes only the already-registered Quoin backup collectors.
// It is intentionally a typed accessor, not a mutable registry escape hatch.
type BackupMetrics struct {
	Active, RunningSince, LastSuccess, LastOnlineManualSuccess, LastFailure, ScheduleOverdue, OldestActiveAge, RetentionCleanupHealthy, RetentionCleanupLastFailure prometheus.Gauge
	Failures                                                                                                                                                        prometheus.Counter
	Duration                                                                                                                                                        prometheus.Observer
	Storage                                                                                                                                                         *StorageHealth
}

// StorageHealth projects the two Quoin persistence targets and folds a failed
// target into the effective readiness state without overwriting maintenance or
// drain state. A later successful durable probe restores that prior state.
type StorageHealth struct {
	server *Server
	gauges map[string]prometheus.Gauge
}

func (health *StorageHealth) Set(target string, writable bool) {
	gauge, ok := health.gauges[target]
	if !ok {
		return
	}
	if writable {
		gauge.Set(1)
	} else {
		gauge.Set(0)
	}
	server := health.server
	server.mu.Lock()
	previous := server.effectiveReadinessLocked()
	server.storageWritable[target] = writable
	current := server.effectiveReadinessLocked()
	server.projectReadinessLocked(previous, current)
	server.mu.Unlock()
}

func (server *Server) BackupMetrics() (*BackupMetrics, error) {
	if server.state.Component != "quoin" {
		return nil, errors.New("backup metrics belong to quoin only")
	}
	gauge := func(name string) (prometheus.Gauge, error) {
		value, ok := server.collectors[name].(prometheus.Gauge)
		if !ok {
			return nil, fmt.Errorf("metrics catalog is missing gauge %s", name)
		}
		return value, nil
	}
	active, err := gauge("quoin_backup_active")
	if err != nil {
		return nil, err
	}
	running, err := gauge("quoin_backup_running_since_timestamp_seconds")
	if err != nil {
		return nil, err
	}
	success, err := gauge("quoin_backup_last_success_timestamp_seconds")
	if err != nil {
		return nil, err
	}
	manual, err := gauge("quoin_backup_last_online_manual_success_timestamp_seconds")
	if err != nil {
		return nil, err
	}
	failure, err := gauge("quoin_backup_last_failure_timestamp_seconds")
	if err != nil {
		return nil, err
	}
	retentionHealthy, err := gauge("quoin_backup_retention_cleanup_healthy")
	if err != nil {
		return nil, err
	}
	retentionFailure, err := gauge("quoin_backup_retention_cleanup_last_failure_timestamp_seconds")
	if err != nil {
		return nil, err
	}
	overdue, err := gauge("quoin_backup_schedule_overdue")
	if err != nil {
		return nil, err
	}
	oldest, err := gauge("quoin_backup_oldest_active_age_seconds")
	if err != nil {
		return nil, err
	}
	failures, ok := server.collectors["quoin_backup_failures_total"].(prometheus.Counter)
	if !ok {
		return nil, errors.New("metrics catalog is missing quoin_backup_failures_total")
	}
	duration, ok := server.collectors["quoin_backup_duration_seconds"].(prometheus.Observer)
	if !ok {
		return nil, errors.New("metrics catalog is missing quoin_backup_duration_seconds")
	}
	storage, ok := server.collectors["quoin_storage_writable"].(*prometheus.GaugeVec)
	if !ok {
		return nil, errors.New("metrics catalog is missing quoin_storage_writable")
	}
	return &BackupMetrics{
		Active: active, RunningSince: running, LastSuccess: success,
		LastOnlineManualSuccess: manual, LastFailure: failure, ScheduleOverdue: overdue,
		OldestActiveAge: oldest, RetentionCleanupHealthy: retentionHealthy, RetentionCleanupLastFailure: retentionFailure, Failures: failures, Duration: duration,
		Storage: &StorageHealth{server: server, gauges: map[string]prometheus.Gauge{
			"data": storage.WithLabelValues("data"), "backup": storage.WithLabelValues("backup"),
		}},
	}, nil
}

// ArtifactGCSuccessProjector exposes the catalog-owned GC health gauge without
// leaking the registry to artifact storage code.
func (server *Server) ArtifactGCSuccessProjector() (func(float64), error) {
	if server.state.Component != "quoin" {
		return nil, errors.New("artifact GC metrics belong to quoin only")
	}
	metric, ok := server.collectors["quoin_artifact_gc_last_success_timestamp_seconds"].(prometheus.Gauge)
	if !ok {
		return nil, errors.New("metrics catalog is missing quoin_artifact_gc_last_success_timestamp_seconds")
	}
	return metric.Set, nil
}

func (server *Server) Handler() http.Handler {
	return server.httpServer.Handler
}

func (server *Server) Readiness() Readiness {
	server.mu.RLock()
	defer server.mu.RUnlock()
	return server.effectiveReadinessLocked()
}

func (server *Server) effectiveReadinessLocked() Readiness {
	state := server.state
	if state.Component == "quoin" && (!server.storageWritable["data"] || !server.storageWritable["backup"]) {
		state.Reason = StorageUnavailable
		state.AcceptingWork = false
	}
	return state
}

func (server *Server) SetReadiness(state Readiness) {
	server.mu.Lock()
	previous := server.effectiveReadinessLocked()
	server.state = state
	current := server.effectiveReadinessLocked()
	server.projectReadinessLocked(previous, current)
	server.mu.Unlock()
}

func (server *Server) projectReadinessLocked(previous, state Readiness) {
	if state.Reason != previous.Reason {
		LogEvent(state.Component, "info", "readiness.changed", "reason="+string(state.Reason))
	}
	ready := state.Reason == Ready || (state.Mode == "maintenance" && state.Reason != StorageUnavailable)
	// On a transition to non-accepting, publish accepting=0 before exposing the
	// not-ready state. A scraper can therefore never observe accepting=1 after
	// the effective state became storage-unavailable. Restore in reverse order.
	if server.accepting != nil && !state.AcceptingWork {
		server.accepting.Set(0)
	}
	server.readyGauge.Set(float64(boolValue(ready)))
	if server.accepting != nil && state.AcceptingWork {
		server.accepting.Set(1)
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
