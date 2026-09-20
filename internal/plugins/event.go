package plugins

// Inbound event capability (ADR-0011): the plugin-side half of Stele's
// webhook layer. The gateway owns bearer/signature verification ("签名对不
// 吗") and hands the authenticated request to the plugin; the plugin owns the
// platform's wire protocol and normalizes it into Events. Business semantics
// (occurrence state machines, attribution, dedup policy) stay with Quoin.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// EventSource is the inbound capability one plugin may implement. Kind is the
// URL path segment AND the source protocol identity used by Quoin's source
// projections (e.g. "alertmanager").
type EventSource interface {
	// Kind is the unique source kind ([a-z][a-z0-9-]*); it maps
	// POST /webhook/{source} requests to this source.
	Kind() string
	// VerifyAndParse validates protocol-level integrity (payload shape,
	// source-specific signatures beyond the gateway's bearer check) and
	// normalizes the payload into one or more Events. Rejecting a request
	// here refuses it BEFORE the gateway enqueues it (HTTP 4xx). It performs
	// no business interpretation.
	VerifyAndParse(ctx context.Context, req InboundRequest) ([]Event, error)
}

// InboundRequest is one received external HTTP request after the gateway's
// authentication and body limits.
type InboundRequest struct {
	Header     http.Header
	Body       []byte
	ReceivedAt time.Time
}

// Event is one normalized inbound occurrence. The gateway stamps identity
// (event id) and delivery metadata; the plugin fills semantics only.
type Event struct {
	// Type is the normalized event type ("alerts.batch"); it routes the
	// event inside Quoin's consumer.
	Type string
	// OccurredAt is the source-declared occurrence time (zero when the
	// protocol carries none; the payload remains authoritative).
	OccurredAt time.Time
	// Payload is the normalized, source-kind-specific JSON document. It is
	// the contract between this plugin's VerifyAndParse and Quoin's consumer.
	Payload json.RawMessage
}
