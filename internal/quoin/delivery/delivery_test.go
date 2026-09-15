package delivery

import (
	"strings"
	"testing"
	"time"
)

func TestMessageValidate(t *testing.T) {
	valid := testMessage()
	if err := valid.validate(); err != nil {
		t.Fatalf("valid message rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Message)
		wantErr string
	}{
		{name: "empty id", mutate: func(m *Message) { m.ID = "" }, wantErr: "message ID"},
		{name: "braces in id", mutate: func(m *Message) { m.ID = "{id}" }, wantErr: "braces"},
		{name: "unknown channel", mutate: func(m *Message) { m.Channel = "fax" }, wantErr: "channel"},
		{name: "empty recipient", mutate: func(m *Message) { m.Recipient = " " }, wantErr: "recipient"},
		{name: "recipient CRLF", mutate: func(m *Message) { m.Recipient = "+8613\r\n00" }, wantErr: "control"},
		{name: "invalid email", mutate: func(m *Message) { m.Channel = ChannelEmail; m.Recipient = "no-at-sign" }, wantErr: "@"},
		{name: "empty template", mutate: func(m *Message) { m.Template = "" }, wantErr: "template"},
		{name: "empty code", mutate: func(m *Message) { m.Code = "" }, wantErr: "code is empty"},
		{name: "zero expiry", mutate: func(m *Message) { m.ExpiresIn = 0 }, wantErr: "expiry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			message := testMessage()
			tc.mutate(&message)
			err := message.validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v does not mention %q", err, tc.wantErr)
			}
			if message.Code != "" && strings.Contains(err.Error(), message.Code) {
				t.Fatalf("validation error leaked the code: %v", err)
			}
		})
	}
}

func TestCompileTemplatePlaceholderRules(t *testing.T) {
	metadata := map[string]bool{"username": true}

	valid, err := compileTemplate("t", "id={delivery_id} user={metadata.username} {expires_in_seconds}", metadata)
	if err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
	rendered, err := valid.render(Message{
		ID:        "d-1",
		Channel:   ChannelSMS,
		Recipient: "+8613800000000",
		Template:  "login_verification",
		Code:      "123456",
		ExpiresIn: 5 * time.Minute,
		Metadata:  map[string]string{"username": "ops-1"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if rendered != "id=d-1 user=ops-1 300" {
		t.Fatalf("rendered = %q", rendered)
	}

	tests := []struct {
		name     string
		template string
		wantErr  string
	}{
		{name: "unknown token", template: "{boom}", wantErr: "unknown placeholder"},
		{name: "expression syntax", template: "{{.Code}}", wantErr: "unknown placeholder"},
		{name: "unterminated", template: "hi {code", wantErr: "unterminated"},
		{name: "metadata not allowlisted", template: "{metadata.tenant}", wantErr: "allowlist"},
		{name: "empty metadata key", template: "{metadata.}", wantErr: "invalid metadata"},
		{name: "oversized", template: strings.Repeat("x", maxTemplateTextLength+1), wantErr: "exceeds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileTemplate("t", tc.template, metadata)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v does not mention %q", err, tc.wantErr)
			}
		})
	}

	t.Run("missing metadata value fails at render", func(t *testing.T) {
		compiled, err := compileTemplate("t", "{metadata.username}", metadata)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if _, err := compiled.render(Message{}); err == nil {
			t.Fatal("render must fail when the allowlisted key is absent")
		}
	})
}

func TestValidateEmail(t *testing.T) {
	for _, addr := range []string{"a@b", "ops-1@example.co.uk", "x+y@sub.example.com"} {
		if err := validateEmail(addr); err != nil {
			t.Fatalf("validateEmail(%q) = %v", addr, err)
		}
	}
	for _, addr := range []string{"nope", "a@b@c", "@b", "a@", "a@.b", "a@b.", "a@b..c", "a b@c.com"} {
		if err := validateEmail(addr); err == nil {
			t.Fatalf("validateEmail(%q) accepted an invalid address", addr)
		}
	}
}
