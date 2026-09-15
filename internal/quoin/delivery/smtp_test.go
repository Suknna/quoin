package delivery

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"mime/quotedprintable"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// smtpTestServer is a minimal ESMTP server used to exercise the sender over
// real connections: implicit TLS, STARTTLS-required and (misconfigured)
// plaintext-listener modes. It records the conversation for assertions.
type smtpTestServer struct {
	t *testing.T

	// startTLSListener serves plain TCP; when false the listener is TLS
	// from the first byte (implicit TLS).
	startTLSListener bool
	// advertiseStartTLS controls whether EHLO offers STARTTLS.
	advertiseStartTLS bool
	// validUser/validPassword define accepted AUTH PLAIN credentials.
	validUser     string
	validPassword string
	rejectAuth    bool

	listener  net.Listener
	tlsConfig *tls.Config
	caPEM     []byte

	mu           sync.Mutex
	authUser     string
	authPassword string
	authAttempts int
	mailFrom     string
	rcptTo       string
	data         string
	quitSeen     bool
}

func newSMTPTestServer(t *testing.T, server *smtpTestServer) {
	t.Helper()
	caPEM, certPEM, certKeyPEM := generateTestCert(t, []net.IP{net.ParseIP("127.0.0.1")}, nil)
	cert, err := tls.X509KeyPair(certPEM, certKeyPEM)
	if err != nil {
		t.Fatalf("load certificate: %v", err)
	}
	server.tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.caPEM = caPEM
	var listener net.Listener
	if server.startTLSListener {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
	} else {
		listener, err = tls.Listen("tcp", "127.0.0.1:0", server.tlsConfig)
		if err != nil {
			t.Fatalf("tls listen: %v", err)
		}
	}
	server.listener = listener
	t.Cleanup(func() { _ = server.listener.Close() })
	go server.acceptLoop()
}

func (s *smtpTestServer) addr() string {
	return s.listener.Addr().String()
}

func (s *smtpTestServer) port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *smtpTestServer) caPEMBytes() []byte {
	return s.caPEM
}

func (s *smtpTestServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *smtpTestServer) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	tlsActive := !s.startTLSListener

	writeLine := func(line string) {
		_, _ = writer.WriteString(line + "\r\n")
		_ = writer.Flush()
	}
	writeLine("220 quoin-test ESMTP")

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		command := line
		if index := strings.IndexByte(line, ' '); index >= 0 {
			command = line[:index]
		}
		switch strings.ToUpper(command) {
		case "EHLO", "HELO":
			features := []string{"AUTH PLAIN", "8BITMIME"}
			if !tlsActive && s.advertiseStartTLS {
				features = []string{"STARTTLS"}
			}
			writeLine("250-quoin-test")
			for index, feature := range features {
				separator := "-"
				if index == len(features)-1 {
					separator = " "
				}
				writeLine("250" + separator + feature)
			}
		case "STARTTLS":
			writeLine("220 Go ahead")
			tlsConn := tls.Server(conn, s.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			reader = bufio.NewReader(tlsConn)
			writer = bufio.NewWriter(tlsConn)
			tlsActive = true
		case "AUTH":
			s.mu.Lock()
			s.authAttempts++
			s.mu.Unlock()
			parts := strings.Fields(line)
			if !tlsActive {
				writeLine("530 Must issue a STARTTLS command first")
				continue
			}
			if len(parts) != 3 || !strings.EqualFold(parts[1], "PLAIN") {
				writeLine("504 Unrecognized authentication type")
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(parts[2])
			if err != nil {
				writeLine("535 Malformed AUTH PLAIN response")
				continue
			}
			fields := strings.Split(string(decoded), "\x00")
			user, password := "", ""
			if len(fields) == 3 {
				user, password = fields[1], fields[2]
			}
			s.mu.Lock()
			s.authUser, s.authPassword = user, password
			s.mu.Unlock()
			if s.rejectAuth || user != s.validUser || password != s.validPassword {
				writeLine("535 5.7.8 Authentication credentials invalid")
				continue
			}
			writeLine("235 2.7.0 Authentication successful")
		case "MAIL":
			s.mu.Lock()
			s.mailFrom = extractAngleAddress(line)
			s.mu.Unlock()
			writeLine("250 2.1.0 Ok")
		case "RCPT":
			s.mu.Lock()
			s.rcptTo = extractAngleAddress(line)
			s.mu.Unlock()
			writeLine("250 2.1.5 Ok")
		case "DATA":
			writeLine("354 End data with <CR><LF>.<CR><LF>")
			var body strings.Builder
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				trimmed := strings.TrimRight(dataLine, "\r\n")
				if trimmed == "." {
					break
				}
				body.WriteString(dataLine)
			}
			s.mu.Lock()
			s.data = body.String()
			s.mu.Unlock()
			writeLine("250 2.0.0 Ok: queued")
		case "QUIT":
			s.mu.Lock()
			s.quitSeen = true
			s.mu.Unlock()
			writeLine("221 2.0.0 Bye")
			return
		default:
			writeLine("502 5.5.2 Command not implemented")
		}
	}
}

func (s *smtpTestServer) snapshot() (authUser, authPassword, mailFrom, rcptTo, data string, quitSeen bool, authAttempts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authUser, s.authPassword, s.mailFrom, s.rcptTo, s.data, s.quitSeen, s.authAttempts
}

// extractAngleAddress pulls the <addr> out of "MAIL FROM:<addr> ..." lines.
func extractAngleAddress(line string) string {
	open := strings.IndexByte(line, '<')
	close := strings.LastIndexByte(line, '>')
	if open < 0 || close <= open {
		return ""
	}
	return line[open+1 : close]
}

// decodeMessageBody extracts the quoted-printable text/plain body from a
// recorded DATA payload.
func decodeMessageBody(t *testing.T, data string) (headers string, body string) {
	t.Helper()
	separator := strings.Index(data, "\r\n\r\n")
	if separator < 0 {
		t.Fatalf("DATA has no header/body separator:\n%s", data)
	}
	headers = data[:separator]
	reader := quotedprintable.NewReader(strings.NewReader(data[separator+4:]))
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decode quoted-printable body: %v", err)
	}
	return headers, string(decoded)
}

// smtpTestConfig assembles a sender configuration for the given test server.
func smtpTestConfig(server *smtpTestServer) SMTPConfig {
	return SMTPConfig{
		Host:              "127.0.0.1",
		Port:              server.port(),
		Username:          "quoin",
		PasswordRef:       "ref:smtp-password",
		From:              "noreply@quoin.test",
		AllowPrivateCIDRs: []string{"127.0.0.0/8"},
		RootCAPEM:         server.caPEMBytes(),
	}
}

func emailMessage() Message {
	message := testMessage()
	message.Channel = ChannelEmail
	message.Recipient = "ops-1@quoin.test"
	return message
}

func TestSMTPSendOverSTARTTLS(t *testing.T) {
	server := &smtpTestServer{
		t:                 t,
		startTLSListener:  true,
		advertiseStartTLS: true,
		validUser:         "quoin",
		validPassword:     "smtp-pass-1",
	}
	newSMTPTestServer(t, server)
	config := smtpTestConfig(server)
	config.TLSMode = SMTPTLSStartTLS
	sender, err := NewSMTPSender(config, staticSecrets(map[string]string{"ref:smtp-password": "smtp-pass-1"}))
	if err != nil {
		t.Fatalf("NewSMTPSender: %v", err)
	}
	if err := sender.Send(context.Background(), emailMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	authUser, authPassword, mailFrom, rcptTo, data, quitSeen, _ := server.snapshot()
	if authUser != "quoin" || authPassword != "smtp-pass-1" {
		t.Fatalf("auth = %q/%q, want quoin/smtp-pass-1", authUser, authPassword)
	}
	if mailFrom != "noreply@quoin.test" {
		t.Fatalf("MAIL FROM = %q", mailFrom)
	}
	if rcptTo != "ops-1@quoin.test" {
		t.Fatalf("RCPT TO = %q", rcptTo)
	}
	if !quitSeen {
		t.Fatal("QUIT was not issued")
	}
	headers, body := decodeMessageBody(t, data)
	if !strings.Contains(headers, "From: noreply@quoin.test") ||
		!strings.Contains(headers, "To: ops-1@quoin.test") {
		t.Fatalf("missing From/To headers:\n%s", headers)
	}
	if !strings.Contains(headers, "X-Quoin-Delivery-ID: delivery-1") {
		t.Fatalf("missing delivery ID header:\n%s", headers)
	}
	if !strings.Contains(body, "123456") || !strings.Contains(body, "300 seconds") {
		t.Fatalf("body misses code or expiry:\n%s", body)
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	server := &smtpTestServer{
		t:             t,
		validUser:     "quoin",
		validPassword: "smtp-pass-1",
	}
	newSMTPTestServer(t, server)
	config := smtpTestConfig(server)
	config.TLSMode = SMTPTLSImplicit
	sender, err := NewSMTPSender(config, staticSecrets(map[string]string{"ref:smtp-password": "smtp-pass-1"}))
	if err != nil {
		t.Fatalf("NewSMTPSender: %v", err)
	}
	if err := sender.Send(context.Background(), emailMessage()); err != nil {
		t.Fatalf("Send over implicit TLS: %v", err)
	}
	if _, _, _, _, data, _, _ := server.snapshot(); !strings.Contains(data, "Subject:") {
		t.Fatalf("message was not accepted:\n%s", data)
	}
}

func TestSMTPSTARTTLSRequiredFailsClosed(t *testing.T) {
	// Plain listener that never offers STARTTLS: the sender must refuse to
	// continue instead of falling back to plaintext.
	server := &smtpTestServer{t: t, startTLSListener: true, validUser: "quoin", validPassword: "pw"}
	newSMTPTestServer(t, server)
	config := smtpTestConfig(server)
	sender, err := NewSMTPSender(config, staticSecrets(map[string]string{"ref:smtp-password": "pw"}))
	if err != nil {
		t.Fatalf("NewSMTPSender: %v", err)
	}
	err = sender.Send(context.Background(), emailMessage())
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("expected STARTTLS failure, got %v", err)
	}
	if _, _, _, _, data, _, attempts := server.snapshot(); attempts != 0 || data != "" {
		t.Fatalf("credentials or message were sent without TLS (auth attempts: %d)", attempts)
	}
}

func TestSMTPAuthFailureDoesNotLeakPassword(t *testing.T) {
	server := &smtpTestServer{
		t:                 t,
		startTLSListener:  true,
		advertiseStartTLS: true,
		validUser:         "quoin",
		validPassword:     "smtp-pass-1",
		rejectAuth:        true,
	}
	newSMTPTestServer(t, server)
	config := smtpTestConfig(server)
	sender, err := NewSMTPSender(config, staticSecrets(map[string]string{"ref:smtp-password": "smtp-pass-1"}))
	if err != nil {
		t.Fatalf("NewSMTPSender: %v", err)
	}
	err = sender.Send(context.Background(), emailMessage())
	if err == nil {
		t.Fatal("Send must fail when auth is rejected")
	}
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("rejection should wrap ErrRejected: %v", err)
	}
	if strings.Contains(err.Error(), "smtp-pass-1") {
		t.Fatalf("error must not contain the password: %v", err)
	}
	if _, _, _, _, data, _, _ := server.snapshot(); data != "" {
		t.Fatal("no message may be delivered after failed auth")
	}
}

func TestSMTPDestinationPolicyAndGuards(t *testing.T) {
	t.Run("metadata endpoint denied", func(t *testing.T) {
		config := SMTPConfig{Host: "169.254.169.254", Port: 587, From: "noreply@quoin.test"}
		sender, err := NewSMTPSender(config, staticSecrets(nil))
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		err = sender.Send(context.Background(), emailMessage())
		if err == nil || !strings.Contains(err.Error(), "link-local") {
			t.Fatalf("metadata endpoint must stay denied: %v", err)
		}
	})
	t.Run("loopback denied without allow CIDR", func(t *testing.T) {
		config := SMTPConfig{Host: "127.0.0.1", Port: 25, From: "noreply@quoin.test"}
		sender, err := NewSMTPSender(config, staticSecrets(nil))
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		err = sender.Send(context.Background(), emailMessage())
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("loopback must be denied without allow CIDR: %v", err)
		}
	})
	t.Run("non email channel rejected", func(t *testing.T) {
		config := SMTPConfig{Host: "smtp.example.com", Port: 587, From: "noreply@quoin.test"}
		sender, err := NewSMTPSender(config, staticSecrets(nil))
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		if err := sender.Send(context.Background(), testMessage()); err == nil {
			t.Fatal("SMS channel must be rejected by the SMTP sender")
		}
	})
	t.Run("recipient control characters rejected", func(t *testing.T) {
		config := SMTPConfig{Host: "smtp.example.com", Port: 587, From: "noreply@quoin.test"}
		sender, err := NewSMTPSender(config, staticSecrets(nil))
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		message := emailMessage()
		message.Recipient = "ops-1@quoin.test\r\nBcc: victim@example.com"
		if err := sender.Send(context.Background(), message); err == nil {
			t.Fatal("header injection via recipient must be rejected")
		}
	})
}

func TestSMTPTemplateRenderingAndMetadata(t *testing.T) {
	server := &smtpTestServer{
		t:                 t,
		startTLSListener:  true,
		advertiseStartTLS: true,
		validUser:         "quoin",
		validPassword:     "pw",
	}
	newSMTPTestServer(t, server)
	config := smtpTestConfig(server)
	config.TLSMode = SMTPTLSStartTLS
	config.Subject = "[{template}] verification for {metadata.username}"
	config.Body = "code={code} expires={expires_in_seconds} recipient={recipient}"
	config.MetadataKeys = []string{"username"}
	sender, err := NewSMTPSender(config, staticSecrets(map[string]string{"ref:smtp-password": "pw"}))
	if err != nil {
		t.Fatalf("NewSMTPSender: %v", err)
	}
	message := emailMessage()
	message.Metadata = map[string]string{"username": "ops-1"}
	if err := sender.Send(context.Background(), message); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, _, _, _, data, _, _ := server.snapshot()
	headers, body := decodeMessageBody(t, data)
	if !strings.Contains(headers, "Subject: [login_verification] verification for ops-1") {
		t.Fatalf("subject was not rendered as configured:\n%s", headers)
	}
	if !strings.Contains(body, "code=123456 expires=300 recipient=ops-1@quoin.test") {
		t.Fatalf("body was not rendered as configured:\n%s", body)
	}

	message.Metadata = nil
	err = sender.Send(context.Background(), message)
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("missing allowlisted metadata key must fail: %v", err)
	}
}

func TestSMTPConfigValidation(t *testing.T) {
	secrets := staticSecrets(nil)
	tests := []struct {
		name    string
		mutate  func(*SMTPConfig)
		wantErr string
	}{
		{name: "missing host", mutate: func(c *SMTPConfig) { c.Host = " " }, wantErr: "host is required"},
		{name: "bad port", mutate: func(c *SMTPConfig) { c.Port = 0 }, wantErr: "port"},
		{name: "bad from", mutate: func(c *SMTPConfig) { c.From = "not-an-address" }, wantErr: "from address"},
		{name: "username without password ref", mutate: func(c *SMTPConfig) { c.PasswordRef = "" }, wantErr: "both"},
		{name: "plaintext mode rejected", mutate: func(c *SMTPConfig) { c.TLSMode = "none" }, wantErr: "starttls"},
		{name: "unknown placeholder", mutate: func(c *SMTPConfig) {
			c.Subject = "{template.render(code)}"
		}, wantErr: "unknown placeholder"},
		{name: "metadata not allowlisted", mutate: func(c *SMTPConfig) {
			c.Body = "{metadata.tenant}"
		}, wantErr: "allowlist"},
		{name: "timeout out of bounds", mutate: func(c *SMTPConfig) { c.Timeout = 61 * time.Second }, wantErr: "timeout"},
		{name: "invalid allow CIDR", mutate: func(c *SMTPConfig) { c.AllowPrivateCIDRs = []string{"nope"} }, wantErr: "CIDR"},
		{name: "bad CA PEM", mutate: func(c *SMTPConfig) { c.RootCAPEM = []byte("junk") }, wantErr: "CA"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := SMTPConfig{
				Host:        "smtp.example.com",
				Port:        587,
				From:        "noreply@quoin.test",
				Username:    "quoin",
				PasswordRef: "ref:smtp-password",
			}
			tc.mutate(&config)
			_, err := NewSMTPSender(config, secrets)
			if err == nil {
				t.Fatalf("expected construction error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}
