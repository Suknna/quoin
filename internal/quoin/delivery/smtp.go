package delivery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// SMTPSender delivers each Message as a plain-text email over mandatory TLS.
type SMTPSender struct {
	policy      *destinationPolicy
	host        string
	port        string
	from        string
	username    string
	passwordRef string
	tlsMode     string
	subject     compiledTemplate
	body        compiledTemplate
	metadata    map[string]bool
	timeout     time.Duration
	secrets     SecretFunc
	tlsConfig   *tls.Config
}

// NewSMTPSender validates the configuration and compiles the templates and
// TLS settings once. Errors never contain secret values.
func NewSMTPSender(config SMTPConfig, secrets SecretFunc) (*SMTPSender, error) {
	if secrets == nil {
		return nil, errors.New("SMTP sender requires a secret resolver")
	}
	if strings.TrimSpace(config.Host) == "" {
		return nil, errors.New("SMTP host is required")
	}
	if len(config.Host) > 253 {
		return nil, errors.New("SMTP host is longer than 253 bytes")
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("SMTP port must be between 1 and 65535")
	}
	if err := validateEmail(config.From); err != nil {
		return nil, fmt.Errorf("SMTP from address: %w", err)
	}
	if (config.Username == "") != (config.PasswordRef == "") {
		return nil, errors.New("SMTP auth requires both username and password reference")
	}
	tlsMode := config.TLSMode
	if tlsMode == "" {
		tlsMode = SMTPTLSStartTLS
	}
	if tlsMode != SMTPTLSStartTLS && tlsMode != SMTPTLSImplicit {
		return nil, fmt.Errorf("SMTP TLS mode %q must be %q or %q; plaintext delivery of verification codes is not supported", tlsMode, SMTPTLSStartTLS, SMTPTLSImplicit)
	}
	metadata, err := compileMetadataKeys(config.MetadataKeys)
	if err != nil {
		return nil, fmt.Errorf("SMTP metadata allowlist: %w", err)
	}
	subject := config.Subject
	if subject == "" {
		subject = "Your verification code"
	}
	compiledSubject, err := compileTemplate("SMTP subject", subject, metadata)
	if err != nil {
		return nil, err
	}
	body := config.Body
	if body == "" {
		body = "Your verification code is {code}. It expires in {expires_in_seconds} seconds."
	}
	compiledBody, err := compileTemplate("SMTP body", body, metadata)
	if err != nil {
		return nil, err
	}
	timeout, err := buildTimeout("SMTP timeout", config.Timeout, defaultSMTPTimeout)
	if err != nil {
		return nil, err
	}
	policy, err := newDestinationPolicy(config.AllowPrivateCIDRs)
	if err != nil {
		return nil, fmt.Errorf("SMTP allow CIDRs: %w", err)
	}
	tlsConfig, err := buildTLSClientConfig(config.Host, config.RootCAPEM)
	if err != nil {
		return nil, fmt.Errorf("SMTP TLS: %w", err)
	}
	return &SMTPSender{
		policy:      policy,
		host:        config.Host,
		port:        fmt.Sprintf("%d", config.Port),
		from:        config.From,
		username:    config.Username,
		passwordRef: config.PasswordRef,
		tlsMode:     tlsMode,
		subject:     compiledSubject,
		body:        compiledBody,
		metadata:    metadata,
		timeout:     timeout,
		secrets:     secrets,
		tlsConfig:   tlsConfig,
	}, nil
}

// Send performs exactly one SMTP delivery attempt over mandatory TLS. The
// password and verification code never appear in returned errors.
func (s *SMTPSender) Send(ctx context.Context, message Message) error {
	if err := message.validate(); err != nil {
		return err
	}
	if message.Channel != ChannelEmail {
		return fmt.Errorf("SMTP sender only delivers %q messages, got %q", ChannelEmail, string(message.Channel))
	}
	deadline := time.Now().Add(s.timeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var password string
	if s.passwordRef != "" {
		resolved, err := s.secrets(ctx, s.passwordRef)
		if err != nil {
			return fmt.Errorf("SMTP auth secret: %w", err)
		}
		if resolved == "" {
			return errors.New("SMTP auth secret resolved to an empty value")
		}
		password = resolved
	}

	conn, err := s.dial(ctx)
	if err != nil {
		return err
	}
	// Break blocked reads/writes when the caller's context finishes first.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return fmt.Errorf("SMTP deadline: %w", err)
	}

	client, err := s.startSession(conn)
	if err != nil {
		_ = conn.Close()
		return err
	}
	if err := s.converse(client, message, password); err != nil {
		_ = client.Close()
		return err
	}
	return nil
}

func (s *SMTPSender) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: s.timeout}
	conn, err := s.policy.dialValidated(ctx, dialer, s.host, s.port)
	if err != nil {
		return nil, err
	}
	if s.tlsMode == SMTPTLSImplicit {
		tlsConn := tls.Client(conn, s.tlsConfig.Clone())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("SMTP TLS handshake: %w", err)
		}
		return tlsConn, nil
	}
	return conn, nil
}

// startSession greets the server and, for STARTTLS mode, upgrades the
// connection. A server without STARTTLS is a configuration failure, never a
// fallback to plaintext.
func (s *SMTPSender) startSession(conn net.Conn) (*smtp.Client, error) {
	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return nil, fmt.Errorf("SMTP greeting: %w", err)
	}
	if s.tlsMode == SMTPTLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			_ = client.Close()
			return nil, errors.New("SMTP server does not offer STARTTLS and plaintext delivery is not allowed")
		}
		if err := client.StartTLS(s.tlsConfig.Clone()); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("SMTP STARTTLS: %w", err)
		}
	}
	return client, nil
}

func (s *SMTPSender) converse(client *smtp.Client, message Message, password string) error {
	if s.username != "" {
		auth := smtp.PlainAuth("", s.username, password, s.host)
		// PlainAuth itself refuses credentials without TLS; the connection
		// is always TLS by this point.
		if err := client.Auth(auth); err != nil {
			return classifySMTPError("SMTP auth", err)
		}
	}
	if err := client.Mail(s.from); err != nil {
		return classifySMTPError("SMTP MAIL FROM", err)
	}
	if err := client.Rcpt(message.Recipient); err != nil {
		return classifySMTPError("SMTP RCPT TO", err)
	}
	writer, err := client.Data()
	if err != nil {
		return classifySMTPError("SMTP DATA", err)
	}
	if err := writeMessage(writer, s, message); err != nil {
		return fmt.Errorf("SMTP message write: %w", err)
	}
	if err := writer.Close(); err != nil {
		return classifySMTPError("SMTP message accept", err)
	}
	if err := client.Quit(); err != nil {
		// The message was already accepted by DATA.
		return nil
	}
	return nil
}

// classifySMTPError wraps permanent (5xx) rejections in ErrRejected and
// leaves transient codes as transport failures. Server text never carries
// credentials or the message body.
func classifySMTPError(what string, err error) error {
	var protocol *textproto.Error
	if errors.As(err, &protocol) && protocol.Code >= 500 {
		return fmt.Errorf("%s: %w: rejected by server", what, ErrRejected)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// writeMessage renders subject and body through the compiled templates and
// emits a minimal RFC 5322 message with a quoted-printable body so the
// payload stays 7-bit-clean.
func writeMessage(writer io.Writer, sender *SMTPSender, message Message) error {
	subject, err := sender.subject.render(message)
	if err != nil {
		return err
	}
	body, err := sender.body.render(message)
	if err != nil {
		return err
	}
	// Recipient and ID are validated to be free of CR/LF/control
	// characters, so headers cannot be injected.
	headers := strings.Join([]string{
		"From: " + sender.from,
		"To: " + message.Recipient,
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: <" + message.ID + "@quoin.delivery>",
		"X-Quoin-Delivery-ID: " + message.ID,
		"X-Quoin-Template: " + mime.QEncoding.Encode("utf-8", message.Template),
		"MIME-Version: 1.0",
		`Content-Type: text/plain; charset="utf-8"`,
		"Content-Transfer-Encoding: quoted-printable",
	}, "\r\n")
	if _, err := fmt.Fprintf(writer, "%s\r\n\r\n", headers); err != nil {
		return err
	}
	encoded := quotedprintable.NewWriter(writer)
	if _, err := strings.NewReader(body).WriteTo(encoded); err != nil {
		return err
	}
	return encoded.Close()
}
