// Package delivery owns the outbound verification-code delivery adapters
// from docs/authentication-design.md section 8: SMTP and one fixed-destination
// HTTPS webhook. Senders only transport a Message to the configured trusted
// receiver; they never decide authentication outcomes.
//
// Every sender in this package keeps the destination fixed (each dialed IP is
// revalidated after DNS resolution; private networks need explicit allow
// CIDRs; link-local/metadata addresses are never dialable), validates TLS
// with no skip-verify path, uses restricted structured payload mappings with
// fixed placeholders, follows no redirects, bounds deadlines and response
// sizes, keeps secrets out of errors, and performs exactly one attempt
// (no retry: timeouts cannot prove non-delivery, so retries need receiver
// dedup and belong to the core orchestration).
package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Channel names the delivery medium; values match the "channel" field of the
// default webhook payload.
type Channel string

const (
	ChannelEmail Channel = "email"
	ChannelSMS   Channel = "sms"
)

// Sender accepts one delivery and performs exactly one outbound attempt.
// A nil error means the receiver accepted the delivery, not that the
// recipient was reached or authenticated.
type Sender interface {
	Send(ctx context.Context, message Message) error
}

// SecretFunc resolves a configured secret reference at send time.
// Implementations must not leak values into returned errors or logs; the
// result is used only for the duration of one Send call.
type SecretFunc func(ctx context.Context, reference string) (string, error)

// Message is one verification delivery. Code is sensitive: adapters must
// transport it but never log or embed it in errors.
type Message struct {
	// ID is the unique delivery identifier (webhook payload field, SMTP
	// X-Quoin-Delivery-ID header). Webhook receivers can dedup on it.
	ID string
	// Channel selects the medium (email or SMS).
	Channel Channel
	// Recipient is the email address or phone number; callers must have
	// resolved it to the account's verified contact (design sections 1, 4).
	Recipient string
	// Template names the logical message template.
	Template string
	// Code is the verification code. Sensitive.
	Code string
	// ExpiresIn is the remaining validity of Code.
	ExpiresIn time.Duration
	// Metadata carries extra non-secret template variables; only keys
	// allowlisted in the sender configuration are reachable from payloads.
	Metadata map[string]string
}

// ErrRejected wraps receivers' definitive rejections (non-2xx webhook status,
// SMTP permanent failure); inspect with errors.Is. It carries no secrets.
var ErrRejected = errors.New("delivery rejected")

func (m Message) validate() error {
	if err := validateIdentifier("message ID", m.ID); err != nil {
		return err
	}
	switch m.Channel {
	case ChannelEmail, ChannelSMS:
	default:
		return fmt.Errorf("message channel %q is not supported", string(m.Channel))
	}
	if err := validateSafeString("message recipient", m.Recipient, maxRecipientLength); err != nil {
		return err
	}
	if m.Channel == ChannelEmail {
		if err := validateEmail(m.Recipient); err != nil {
			return fmt.Errorf("message recipient: %w", err)
		}
	}
	if err := validateSafeString("message template", m.Template, maxIdentifierLength); err != nil {
		return err
	}
	if strings.TrimSpace(m.Code) == "" {
		return errors.New("message code is empty")
	}
	if len(m.Code) > maxCodeLength {
		return fmt.Errorf("message code exceeds %d bytes", maxCodeLength)
	}
	if m.ExpiresIn <= 0 {
		return errors.New("message expiry must be positive")
	}
	if len(m.Metadata) > maxMetadataEntries {
		return fmt.Errorf("message metadata exceeds %d entries", maxMetadataEntries)
	}
	for key, value := range m.Metadata {
		if err := validateSafeString("metadata key", key, maxIdentifierLength); err != nil {
			return err
		}
		if len(value) > maxMetadataValueLength {
			return fmt.Errorf("metadata value for key %q exceeds %d bytes", key, maxMetadataValueLength)
		}
	}
	return nil
}

const (
	maxIdentifierLength     = 128
	maxRecipientLength      = 320
	maxCodeLength           = 256
	maxMetadataEntries      = 32
	maxMetadataValueLength  = 4096
	maxTemplateTextLength   = 4096
	maxMappingFields        = 32
	maxHeaderNameLength     = 128
	maxHeaderValueLength    = 4096
	maxJSONPathDepth        = 4
	maxJSONPathSegmentBound = 64
)

func validateSafeString(what, value string, maxLength int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if len(value) > maxLength {
		return fmt.Errorf("%s exceeds %d bytes", what, maxLength)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", what)
	}
	if strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%s contains control characters", what)
	}
	return nil
}

// validateIdentifier also rejects braces so identifiers cannot collide with
// placeholder syntax.
func validateIdentifier(what, value string) error {
	if err := validateSafeString(what, value, maxIdentifierLength); err != nil {
		return err
	}
	if strings.ContainsAny(value, "{}") {
		return fmt.Errorf("%s must not contain braces", what)
	}
	return nil
}

// validateEmail is a deliberately small structural check for configured
// sender addresses and email recipients; the receiver stays the authority on
// address validity.
func validateEmail(addr string) error {
	if strings.Count(addr, "@") != 1 {
		return errors.New("address must contain exactly one @")
	}
	local, domain, _ := strings.Cut(addr, "@")
	if local == "" || len(local) > 64 {
		return errors.New("address local part is empty or longer than 64 bytes")
	}
	if domain == "" || len(domain) > 253 {
		return errors.New("address domain is empty or longer than 253 bytes")
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return errors.New("address domain has invalid dot placement")
	}
	if strings.ContainsAny(local+domain, " \t") {
		return errors.New("address contains whitespace")
	}
	return nil
}

// placeholderToken is one compiled `{name}` occurrence. Substitution is a
// pure fixed-name lookup: no expression language, no control flow.
type placeholderToken struct {
	metadata bool // {metadata.KEY} instead of a fixed name
	name     string
}

type compiledTemplate struct {
	segments []compiledSegment
}

type compiledSegment struct {
	literal string
	token   placeholderToken // meaningful only when the segment has a token
}

var fixedPlaceholderNames = map[string]bool{
	"delivery_id":        true,
	"channel":            true,
	"recipient":          true,
	"template":           true,
	"code":               true,
	"expires_in_seconds": true,
}

// compileTemplate validates one template against the fixed placeholder set
// plus the metadata allowlist. Braces that do not form a known placeholder
// are configuration errors.
func compileTemplate(what, template string, metadataKeys map[string]bool) (compiledTemplate, error) {
	if len(template) > maxTemplateTextLength {
		return compiledTemplate{}, fmt.Errorf("%s exceeds %d bytes", what, maxTemplateTextLength)
	}
	var compiled compiledTemplate
	var literal strings.Builder
	for {
		open := strings.IndexByte(template, '{')
		if open < 0 {
			literal.WriteString(template)
			break
		}
		literal.WriteString(template[:open])
		rest := template[open+1:]
		close := strings.IndexByte(rest, '}')
		if close < 0 {
			return compiledTemplate{}, fmt.Errorf("%s has an unterminated '{' placeholder", what)
		}
		token, err := resolvePlaceholder(what, rest[:close], metadataKeys)
		if err != nil {
			return compiledTemplate{}, err
		}
		compiled.segments = append(compiled.segments, compiledSegment{literal: literal.String(), token: token})
		literal.Reset()
		template = rest[close+1:]
	}
	compiled.segments = append(compiled.segments, compiledSegment{literal: literal.String()})
	return compiled, nil
}

func resolvePlaceholder(what, name string, metadataKeys map[string]bool) (placeholderToken, error) {
	if fixedPlaceholderNames[name] {
		return placeholderToken{name: name}, nil
	}
	if key, ok := strings.CutPrefix(name, "metadata."); ok {
		if key == "" || len(key) > maxIdentifierLength {
			return placeholderToken{}, fmt.Errorf("%s has an invalid metadata placeholder", what)
		}
		if !metadataKeys[key] {
			return placeholderToken{}, fmt.Errorf("%s uses metadata key %q which is not in the configured allowlist", what, key)
		}
		return placeholderToken{metadata: true, name: key}, nil
	}
	return placeholderToken{}, fmt.Errorf("%s uses unknown placeholder %q; only fixed names and {metadata.KEY} are allowed", what, name)
}

// render substitutes the template against the Message. A missing allowlisted
// metadata key fails the send so receivers never get half-substituted
// payloads.
func (t compiledTemplate) render(message Message) (string, error) {
	var out strings.Builder
	for _, segment := range t.segments {
		out.WriteString(segment.literal)
		if segment.token.name == "" && !segment.token.metadata {
			continue
		}
		value, err := placeholderValue(segment.token, message)
		if err != nil {
			return "", err
		}
		out.WriteString(value)
	}
	return out.String(), nil
}

func placeholderValue(token placeholderToken, message Message) (string, error) {
	if token.metadata {
		value, ok := message.Metadata[token.name]
		if !ok {
			return "", fmt.Errorf("message metadata is missing allowlisted key %q", token.name)
		}
		return value, nil
	}
	switch token.name {
	case "delivery_id":
		return message.ID, nil
	case "channel":
		return string(message.Channel), nil
	case "recipient":
		return message.Recipient, nil
	case "template":
		return message.Template, nil
	case "code":
		return message.Code, nil
	case "expires_in_seconds":
		return fmt.Sprintf("%d", int64(message.ExpiresIn/time.Second)), nil
	default:
		// Unreachable: resolvePlaceholder only emits known names.
		return "", fmt.Errorf("unsupported placeholder %q", token.name)
	}
}
