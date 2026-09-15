package delivery

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	defaultWebhookTimeout         = 10 * time.Second
	defaultSMTPTimeout            = 15 * time.Second
	minTimeout                    = time.Second
	maxTimeout                    = 60 * time.Second
	defaultMaxResponseBytes int64 = 64 << 10 // 64 KiB
	minMaxResponseBytes     int64 = 1 << 10  // 1 KiB
	maxMaxResponseBytes     int64 = 1 << 20  // 1 MiB
	maxResponseHeaderBytes  int64 = 64 << 10
)

func buildTimeout(what string, value, fallback time.Duration) (time.Duration, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < minTimeout || value > maxTimeout {
		return 0, fmt.Errorf("%s must be between %s and %s", what, minTimeout, maxTimeout)
	}
	return value, nil
}

// buildTLSClientConfig assembles the shared TLS client configuration.
// Certificate validation is never optional; RootCAPEM is appended to the
// system pool so a private CA can be trusted without losing public
// validation.
func buildTLSClientConfig(serverName string, rootCAPEM []byte) (*tls.Config, error) {
	rootPool, err := x509.SystemCertPool()
	if err != nil || rootPool == nil {
		rootPool = x509.NewCertPool()
	}
	if len(rootCAPEM) > 0 {
		if !rootPool.AppendCertsFromPEM(rootCAPEM) {
			return nil, errors.New("root CA PEM contains no usable certificates")
		}
	}
	return &tls.Config{
		ServerName: serverName,
		RootCAs:    rootPool,
		MinVersion: tls.VersionTLS12,
	}, nil
}

func validateHeaderName(name string) error {
	if name == "" || len(name) > maxHeaderNameLength {
		return errors.New("header name is empty or longer than 128 bytes")
	}
	if strings.Trim(name, "!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") != "" {
		return fmt.Errorf("header name %q contains characters outside the HTTP token set", name)
	}
	return nil
}

// WebhookConfig is the fixed-destination outbound HTTPS webhook delivery
// configuration. Everything is validated and compiled once at
// NewWebhookSender time; Send only substitutes message values.
type WebhookConfig struct {
	// URL is the one fixed destination: HTTPS, no userinfo, no fragment,
	// no placeholders (the target must not become dynamic).
	URL string
	// Timeout bounds DNS, connect, TLS, request and response read for one
	// delivery. Default 10s, 1s..60s.
	Timeout time.Duration
	// MaxResponseBytes bounds how much of the response body is read.
	// Default 64 KiB, 1 KiB..1 MiB.
	MaxResponseBytes int64
	// Headers are static request headers sent with every delivery.
	Headers map[string]string
	// SecretHeaders maps a header name to a secret reference resolved via
	// SecretFunc at send time; values never appear in errors.
	SecretHeaders map[string]string
	// Encoding selects the payload encoding: "json" (default) or "form".
	Encoding string
	// JSONFields maps dotted JSON paths (maximum depth 4) to payload
	// templates; ignored unless Encoding is "json". Empty means the
	// documented default payload.
	JSONFields map[string]string
	// FormFields maps form keys to payload templates; ignored unless
	// Encoding is "form".
	FormFields map[string]string
	// MetadataKeys is the closed allowlist of Message.Metadata keys that
	// {metadata.KEY} placeholders may reference.
	MetadataKeys []string
	// SuccessField/SuccessValue optionally check one JSON field of a 2xx
	// response so receivers reporting business failure inside HTTP 200 are
	// treated as failed deliveries.
	SuccessField string
	// SuccessValue is the expected JSON literal (string, number, boolean).
	SuccessValue string
	// AllowPrivateCIDRs explicitly extends the dial policy to private
	// networks; link-local/metadata stays denied even when listed.
	AllowPrivateCIDRs []string
	// RootCAPEM optionally adds a private CA for the fixed destination.
	RootCAPEM []byte
}

// Payload encodings.
const (
	EncodingJSON = "json"
	EncodingForm = "form"
)

// SMTPConfig is the outbound SMTP delivery configuration. TLS is mandatory
// (implicit TLS or STARTTLS-required) and always validated: the payload
// carries verification codes.
type SMTPConfig struct {
	// Host is the SMTP server hostname; it anchors certificate validation
	// and SMTP PLAIN auth.
	Host string
	// Port is required, 1..65535.
	Port int
	// Username and PasswordRef enable SMTP AUTH PLAIN; PasswordRef is a
	// secret reference resolved via SecretFunc at send time.
	Username    string
	PasswordRef string
	// From is the envelope sender and From header address.
	From string
	// TLSMode is "starttls" (default; fails when the server does not offer
	// STARTTLS) or "implicit" (TLS from the first byte).
	TLSMode string
	// Subject and Body are message templates using the fixed placeholder
	// set; they default to a simple code message.
	Subject string
	Body    string
	// MetadataKeys is the closed allowlist for {metadata.KEY} placeholders.
	MetadataKeys []string
	// AllowPrivateCIDRs extends the dial policy like WebhookConfig.
	AllowPrivateCIDRs []string
	// RootCAPEM optionally adds a private CA.
	RootCAPEM []byte
	// Timeout bounds DNS, connect, TLS and the whole SMTP conversation.
	// Default 15s, 1s..60s.
	Timeout time.Duration
}

// SMTP TLS modes.
const (
	SMTPTLSStartTLS = "starttls"
	SMTPTLSImplicit = "implicit"
)

func compileMetadataKeys(keys []string) (map[string]bool, error) {
	set := make(map[string]bool, len(keys))
	for _, key := range keys {
		if err := validateSafeString("metadata allowlist key", key, maxIdentifierLength); err != nil {
			return nil, err
		}
		set[key] = true
	}
	return set, nil
}
