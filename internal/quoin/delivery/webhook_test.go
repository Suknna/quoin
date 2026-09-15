package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookSendDefaultPayloadMatchesDesignShape(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	server, caPEM := startTLSTestServer(t, handler)

	sender, err := NewWebhookSender(loopbackWebhookConfig(server, caPEM), staticSecrets(nil))
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}
	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := gotHeader.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var payload struct {
		DeliveryID string         `json:"delivery_id"`
		Channel    string         `json:"channel"`
		Recipient  string         `json:"recipient"`
		Template   string         `json:"template"`
		Variables  map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, gotBody)
	}
	if payload.DeliveryID != "delivery-1" || payload.Channel != "sms" ||
		payload.Recipient != "+8613800000000" || payload.Template != "login_verification" {
		t.Fatalf("unexpected envelope fields: %+v", payload)
	}
	if payload.Variables["code"] != "123456" {
		t.Fatalf("variables.code = %v, want string 123456", payload.Variables["code"])
	}
	// The documented default shape carries a numeric expires_in_seconds.
	if seconds, ok := payload.Variables["expires_in_seconds"].(float64); !ok || seconds != 300 {
		t.Fatalf("variables.expires_in_seconds = %#v, want number 300", payload.Variables["expires_in_seconds"])
	}
	if len(payload.Variables) != 2 {
		t.Fatalf("variables has unexpected extra entries: %v", payload.Variables)
	}
}

func TestWebhookFormEncoding(t *testing.T) {
	var gotContentType string
	var gotBody []byte
	server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	config := loopbackWebhookConfig(server, caPEM)
	config.Encoding = EncodingForm
	config.FormFields = map[string]string{
		"code":   "{code}",
		"target": "{recipient}",
		"kind":   "{channel}/{template}",
	}
	sender, err := NewWebhookSender(config, staticSecrets(nil))
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}
	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type = %q", gotContentType)
	}
	body := string(gotBody)
	for _, want := range []string{"code=123456", "target=%2B8613800000000", "kind=sms%2Flogin_verification"} {
		if !strings.Contains(body, want) {
			t.Fatalf("form body %q misses %q", body, want)
		}
	}
}

func TestWebhookSecretHeadersResolvedPerSend(t *testing.T) {
	var gotKey string
	server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	}))
	config := loopbackWebhookConfig(server, caPEM)
	config.Headers = map[string]string{"X-Quoin-Env": "test"}
	config.SecretHeaders = map[string]string{"X-Api-Key": "ref:api-key"}
	sender, err := NewWebhookSender(config, staticSecrets(map[string]string{"ref:api-key": "resolved-key-value"}))
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}
	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotKey != "resolved-key-value" {
		t.Fatalf("X-Api-Key = %q, want the resolved secret", gotKey)
	}
}

func TestWebhookSecretResolverFailureDoesNotLeak(t *testing.T) {
	server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	config := loopbackWebhookConfig(server, caPEM)
	config.SecretHeaders = map[string]string{"X-Api-Key": "ref:api-key"}
	sender, err := NewWebhookSender(config, func(_ context.Context, _ string) (string, error) {
		return "", errors.New("vault unavailable")
	})
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}
	err = sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send should fail when secret resolution fails")
	}
	if !strings.Contains(err.Error(), "X-Api-Key") || !strings.Contains(err.Error(), "vault unavailable") {
		t.Fatalf("error should name the header and the resolver failure: %v", err)
	}
}

func TestWebhookRedirectNotFollowed(t *testing.T) {
	var requests atomic.Int32
	server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	sender, err := NewWebhookSender(loopbackWebhookConfig(server, caPEM), staticSecrets(nil))
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}
	err = sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send must fail on redirect")
	}
	if !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("error should wrap ErrRedirectNotAllowed: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("redirect was followed %d extra times", got-1)
	}
}

func TestWebhookNon2xxSurfacesStatusOnly(t *testing.T) {
	const reflectedBody = "payload echo code=123456 must not appear in the error"
	server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, reflectedBody, http.StatusForbidden)
	}))
	sender, err := NewWebhookSender(loopbackWebhookConfig(server, caPEM), staticSecrets(nil))
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}
	err = sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send must fail on 403")
	}
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("error should wrap ErrRejected: %v", err)
	}
	if strings.Contains(err.Error(), reflectedBody) {
		t.Fatalf("error must not include the response body: %v", err)
	}
}

func TestWebhookResponseSizeCap(t *testing.T) {
	server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 200_000))
	}))
	sender, err := NewWebhookSender(loopbackWebhookConfig(server, caPEM), staticSecrets(nil))
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}
	err = sender.Send(context.Background(), testMessage())
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("Send should fail on oversized response, got %v", err)
	}
}

func TestWebhookSuccessFieldCheck(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		field     string
		want      string
		shouldErr bool
	}{
		{name: "matching bool passes", body: `{"ok":true,"id":1}`, field: "ok", want: "true"},
		{name: "false bool rejected", body: `{"ok":false}`, field: "ok", want: "true", shouldErr: true},
		{name: "nested string passes", body: `{"data":{"status":"accepted"}}`, field: "data.status", want: `"accepted"`},
		{name: "missing field rejected", body: `{"other":1}`, field: "data.status", want: `"accepted"`, shouldErr: true},
		{name: "non JSON body rejected", body: `plain`, field: "ok", want: "true", shouldErr: true},
		{name: "value mismatch rejected", body: `{"ok":"yes"}`, field: "ok", want: "true", shouldErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			config := loopbackWebhookConfig(server, caPEM)
			config.SuccessField = tc.field
			config.SuccessValue = tc.want
			sender, err := NewWebhookSender(config, staticSecrets(nil))
			if err != nil {
				t.Fatalf("NewWebhookSender: %v", err)
			}
			err = sender.Send(context.Background(), testMessage())
			if tc.shouldErr {
				if err == nil || !errors.Is(err, ErrRejected) {
					t.Fatalf("expected ErrRejected, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
		})
	}
}

func TestWebhookTimeoutBoundsDelivery(t *testing.T) {
	server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	config := loopbackWebhookConfig(server, caPEM)
	config.Timeout = time.Second
	sender, err := NewWebhookSender(config, staticSecrets(nil))
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}
	started := time.Now()
	err = sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send must fail when the receiver stalls")
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("Send took %s, deadline was not enforced", elapsed)
	}
}

func TestWebhookDestinationPolicy(t *testing.T) {
	t.Run("metadata endpoint denied even when allowlisted", func(t *testing.T) {
		config := WebhookConfig{
			URL:               "https://169.254.169.254/latest/meta-data/",
			AllowPrivateCIDRs: []string{"169.254.0.0/16"},
		}
		sender, err := NewWebhookSender(config, staticSecrets(nil))
		if err != nil {
			t.Fatalf("NewWebhookSender: %v", err)
		}
		err = sender.Send(context.Background(), testMessage())
		if err == nil || !strings.Contains(err.Error(), "link-local") {
			t.Fatalf("metadata endpoint must stay denied: %v", err)
		}
	})
	t.Run("loopback denied without explicit allow", func(t *testing.T) {
		server, _ := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		sender, err := NewWebhookSender(WebhookConfig{URL: server.URL}, staticSecrets(nil))
		if err != nil {
			t.Fatalf("NewWebhookSender: %v", err)
		}
		err = sender.Send(context.Background(), testMessage())
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("loopback must be denied without allow CIDR: %v", err)
		}
	})
}

func TestWebhookConfigValidation(t *testing.T) {
	secrets := staticSecrets(nil)
	tests := []struct {
		name    string
		mutate  func(*WebhookConfig)
		wantErr string
	}{
		{name: "http scheme rejected", mutate: func(c *WebhookConfig) { c.URL = "http://example.com/hook" }, wantErr: "https"},
		{name: "userinfo rejected", mutate: func(c *WebhookConfig) { c.URL = "https://user:pass@example.com/hook" }, wantErr: "userinfo"},
		{name: "dynamic URL rejected", mutate: func(c *WebhookConfig) { c.URL = "https://example.com/{recipient}" }, wantErr: "braces"},
		{name: "bad encoding", mutate: func(c *WebhookConfig) { c.Encoding = "xml" }, wantErr: "encoding"},
		{name: "form plus json fields", mutate: func(c *WebhookConfig) {
			c.Encoding = EncodingForm
			c.JSONFields = map[string]string{"a": "{code}"}
		}, wantErr: "JSON fields"},
		{name: "json plus form fields", mutate: func(c *WebhookConfig) {
			c.FormFields = map[string]string{"a": "{code}"}
		}, wantErr: "form fields"},
		{name: "unknown placeholder", mutate: func(c *WebhookConfig) {
			c.JSONFields = map[string]string{"a": "{exec(code)}"}
		}, wantErr: "unknown placeholder"},
		{name: "metadata key not allowlisted", mutate: func(c *WebhookConfig) {
			c.JSONFields = map[string]string{"a": "{metadata.tenant}"}
		}, wantErr: "allowlist"},
		{name: "metadata in URL mapping", mutate: func(c *WebhookConfig) {
			c.JSONFields = map[string]string{"{code}": "{code}"}
		}, wantErr: "path"},
		{name: "json path conflict", mutate: func(c *WebhookConfig) {
			c.JSONFields = map[string]string{"a": "{code}", "a.b": "{code}"}
		}, wantErr: "leaf and a parent"},
		{name: "success value not a literal", mutate: func(c *WebhookConfig) {
			c.SuccessField = "ok"
			c.SuccessValue = `{"deep":1}`
		}, wantErr: "string, number or boolean"},
		{name: "success field without value", mutate: func(c *WebhookConfig) { c.SuccessField = "ok" }, wantErr: "both"},
		{name: "timeout out of bounds", mutate: func(c *WebhookConfig) { c.Timeout = time.Millisecond }, wantErr: "timeout"},
		{name: "response cap out of bounds", mutate: func(c *WebhookConfig) { c.MaxResponseBytes = 10 << 20 }, wantErr: "response bytes"},
		{name: "invalid allow CIDR", mutate: func(c *WebhookConfig) { c.AllowPrivateCIDRs = []string{"10.0.0.0/"} }, wantErr: "CIDR"},
		{name: "invalid header name", mutate: func(c *WebhookConfig) { c.Headers = map[string]string{"X Bad": "v"} }, wantErr: "token set"},
		{name: "Host header controlled", mutate: func(c *WebhookConfig) { c.Headers = map[string]string{"Host": "example.com"} }, wantErr: "transport"},
		{name: "empty secret reference", mutate: func(c *WebhookConfig) { c.SecretHeaders = map[string]string{"X-Api-Key": "  "} }, wantErr: "secret reference"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := WebhookConfig{URL: "https://gateway.example.com/hook"}
			tc.mutate(&config)
			_, err := NewWebhookSender(config, secrets)
			if err == nil {
				t.Fatalf("expected construction error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestWebhookMetadataAllowlist(t *testing.T) {
	var gotBody []byte
	server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	config := loopbackWebhookConfig(server, caPEM)
	config.MetadataKeys = []string{"username"}
	config.JSONFields = map[string]string{"user": "{metadata.username}", "code": "{code}"}
	sender, err := NewWebhookSender(config, staticSecrets(nil))
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}

	message := testMessage()
	message.Metadata = map[string]string{"username": "ops-1"}
	if err := sender.Send(context.Background(), message); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(string(gotBody), `"user":"ops-1"`) {
		t.Fatalf("metadata placeholder was not substituted: %s", gotBody)
	}

	message.Metadata = nil
	err = sender.Send(context.Background(), message)
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("missing allowlisted metadata key must fail the send: %v", err)
	}
}

func TestWebhookSenderInterfaceDefaults(t *testing.T) {
	// A zero-value send context plus default configuration must not need
	// any tuning: this guards the "declarative defaults" contract.
	server, caPEM := startTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	sender, err := NewWebhookSender(loopbackWebhookConfig(server, caPEM), staticSecrets(nil))
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}
	var _ Sender = sender
	if sender.timeout != defaultWebhookTimeout {
		t.Fatalf("default timeout = %s, want %s", sender.timeout, defaultWebhookTimeout)
	}
	if sender.maxResponse != defaultMaxResponseBytes {
		t.Fatalf("default response cap = %d, want %d", sender.maxResponse, defaultMaxResponseBytes)
	}
}
