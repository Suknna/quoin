package stele

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"time"

	"google.golang.org/grpc/status"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/prometheus/client_golang/prometheus"
)

// Webhook is the Alertmanager HTTP intake handler.
type Webhook struct {
	relay      *Relay
	deliveries *prometheus.CounterVec
	ready      prometheus.Gauge
	available  prometheus.Gauge
	requests   *prometheus.CounterVec
}

// NewWebhook wires the handler with the frozen metrics families
// (metrics.yaml: stele_ready, stele_quoin_available, stele_deliveries_total,
// stele_grpc_client_requests_total).
func NewWebhook(relay *Relay, registry *prometheus.Registry) *Webhook {
	deliveries := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stele_deliveries_total",
		Help: "Stele delivery attempts grouped by the authoritative relay DeliveryStatus.",
	}, []string{"delivery_status"})
	for _, status := range []string{"accepted", "rejected", "unavailable"} {
		deliveries.WithLabelValues(status).Add(0)
	}
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stele_grpc_client_requests_total",
		Help: "Stele gRPC client requests grouped by service and canonical status.",
	}, []string{"rpc_group", "grpc_status"})
	for _, status := range []string{"OK", "Unavailable", "DeadlineExceeded", "ResourceExhausted", "Unauthenticated", "Unknown"} {
		requests.WithLabelValues("runtime.v1.SteleRelay", status).Add(0)
	}
	ready := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stele_ready",
		Help: "Whether Stele can authenticate and relay Alertmanager deliveries.",
	})
	available := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stele_quoin_available",
		Help: "Whether Stele can currently reach an accepted same-version Quoin relay service.",
	})
	registry.MustRegister(deliveries, requests, ready, available)
	webhook := &Webhook{relay: relay, deliveries: deliveries, ready: ready, available: available, requests: requests}
	if relay.Ready() {
		ready.Set(1)
	}
	return webhook
}

// ServeHTTP authenticates the bearer against the cached snapshot and relays
// the exact body (CONTEXT「Stele」): 204 only after Quoin commits; 4xx for
// permanent rejection; 5xx for retryable failures; 503 before the snapshot
// loads.
func (webhook *Webhook) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !webhook.relay.Ready() {
		http.Error(writer, "credential snapshot not loaded", http.StatusServiceUnavailable)
		webhook.deliveries.WithLabelValues("unavailable").Inc()
		return
	}
	webhook.ready.Set(1)
	webhook.available.Set(1)
	authHeader := request.Header.Get("Authorization")
	if len(authHeader) < 8 || authHeader[:7] != "Bearer " {
		http.Error(writer, "missing bearer", http.StatusUnauthorized)
		webhook.deliveries.WithLabelValues("rejected").Inc()
		return
	}
	bearer := authHeader[7:]
	sourceID, credentialID, snapshotVersion, ok := webhook.relay.Credential(bearer)
	if !ok {
		http.Error(writer, "unknown credential", http.StatusUnauthorized)
		webhook.deliveries.WithLabelValues("rejected").Inc()
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxWebhookBody+1))
	if err != nil {
		http.Error(writer, "read body failed", http.StatusBadRequest)
		webhook.deliveries.WithLabelValues("rejected").Inc()
		return
	}
	if len(body) > maxWebhookBody {
		http.Error(writer, "body too large", http.StatusRequestEntityTooLarge)
		webhook.deliveries.WithLabelValues("rejected").Inc()
		return
	}
	relayID, err := randomRelayID()
	if err != nil {
		http.Error(writer, "relay id unavailable", http.StatusServiceUnavailable)
		webhook.deliveries.WithLabelValues("unavailable").Inc()
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 40*time.Second)
	defer cancel()
	status, relayErr := webhook.relay.Deliver(ctx, relayID, sourceID, credentialID, snapshotVersion, body, time.Now().UTC())
	if relayErr != nil {
		http.Error(writer, "relay failed", http.StatusServiceUnavailable)
		webhook.deliveries.WithLabelValues("unavailable").Inc()
		webhook.requests.WithLabelValues("runtime.v1.SteleRelay", statusName(relayErr)).Inc()
		return
	}
	switch status {
	case runtimev1.DeliveryStatus_DELIVERY_STATUS_ACCEPTED:
		writer.WriteHeader(http.StatusNoContent)
		webhook.deliveries.WithLabelValues("accepted").Inc()
		webhook.requests.WithLabelValues("runtime.v1.SteleRelay", "OK").Inc()
	case runtimev1.DeliveryStatus_DELIVERY_STATUS_REJECTED:
		http.Error(writer, "delivery rejected", http.StatusBadRequest)
		webhook.deliveries.WithLabelValues("rejected").Inc()
		webhook.requests.WithLabelValues("runtime.v1.SteleRelay", "Unknown").Inc()
	default:
		http.Error(writer, "relay unavailable", http.StatusServiceUnavailable)
		webhook.deliveries.WithLabelValues("unavailable").Inc()
		webhook.requests.WithLabelValues("runtime.v1.SteleRelay", "Unavailable").Inc()
	}
}

func statusName(err error) string {
	return status.Code(err).String()
}

func randomRelayID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
