// Command otpdelivery is a local test fixture that receives Quoin
// verification-code deliveries over both outbound channels in one process:
// an HTTPS webhook receiver and an implicit-TLS SMTP server. Accepted
// deliveries are appended as JSON lines so E2E harnesses can assert on them.
//
// It is test support only: no production binary depends on it.
//
// Usage:
//
//	otpdelivery --tls-cert runtime-tls.crt --tls-key runtime-tls.key \
//	    [--https-listen 127.0.0.1:8445] [--smtp-listen 127.0.0.1:8587] \
//	    [--record deliveries.jsonl] [--smtp-user quoin --smtp-pass secret]
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type recorder struct {
	mu   sync.Mutex
	file io.Writer
}

type webhookDelivery struct {
	Kind            string            `json:"kind"`
	ReceivedAt      string            `json:"received_at"`
	DeliveryID      string            `json:"delivery_id"`
	Channel         string            `json:"channel"`
	Recipient       string            `json:"recipient"`
	Template        string            `json:"template"`
	Code            string            `json:"code"`
	ExpiresInSecond int               `json:"expires_in_seconds"`
	Headers         map[string]string `json:"headers,omitempty"`
}

type smtpDelivery struct {
	Kind       string `json:"kind"`
	ReceivedAt string `json:"received_at"`
	MailFrom   string `json:"mail_from"`
	RcptTo     string `json:"rcpt_to"`
	Data       string `json:"data"`
}

func (r *recorder) write(value any) {
	line, err := json.Marshal(value)
	if err != nil {
		log.Printf("record marshal: %v", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := fmt.Fprintf(r.file, "%s\n", line); err != nil {
		log.Printf("record write: %v", err)
	}
}

func main() {
	tlsCert := flag.String("tls-cert", "", "TLS certificate PEM (required)")
	tlsKey := flag.String("tls-key", "", "TLS private key PEM (required)")
	httpsListen := flag.String("https-listen", "127.0.0.1:8445", "HTTPS webhook listen address")
	smtpListen := flag.String("smtp-listen", "127.0.0.1:8587", "implicit-TLS SMTP listen address")
	recordPath := flag.String("record", "", "JSONL file to append deliveries to (default stdout)")
	smtpUser := flag.String("smtp-user", "", "required SMTP AUTH PLAIN username (empty accepts any)")
	smtpPass := flag.String("smtp-pass", "", "required SMTP AUTH PLAIN password")
	flag.Parse()
	if *tlsCert == "" || *tlsKey == "" {
		log.Fatal("--tls-cert and --tls-key are required")
	}
	certificate, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
	if err != nil {
		log.Fatalf("load TLS key pair: %v", err)
	}

	output := io.Writer(os.Stdout)
	var closer io.WriteCloser
	if *recordPath != "" {
		file, err := os.OpenFile(*recordPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			log.Fatalf("open record file: %v", err)
		}
		output, closer = file, file
	}
	defer func() {
		if closer != nil {
			_ = closer.Close()
		}
	}()
	records := &recorder{file: output}

	go runSMTP(records, *smtpListen, certificate, *smtpUser, *smtpPass)
	runHTTPS(records, *httpsListen, certificate)
}

func runHTTPS(records *recorder, address string, certificate tls.Certificate) {
	server := &http.Server{
		Addr: address,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var payload struct {
			DeliveryID string         `json:"delivery_id"`
			Channel    string         `json:"channel"`
			Recipient  string         `json:"recipient"`
			Template   string         `json:"template"`
			Variables  map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "payload is not JSON", http.StatusBadRequest)
			return
		}
		headers := map[string]string{}
		for name, values := range r.Header {
			headers[name] = strings.Join(values, ",")
		}
		code, _ := payload.Variables["code"].(string)
		seconds := 0
		if raw, ok := payload.Variables["expires_in_seconds"].(float64); ok {
			seconds = int(raw)
		}
		records.write(webhookDelivery{
			Kind:            "webhook",
			ReceivedAt:      time.Now().UTC().Format(time.RFC3339Nano),
			DeliveryID:      payload.DeliveryID,
			Channel:         payload.Channel,
			Recipient:       payload.Recipient,
			Template:        payload.Template,
			Code:            code,
			ExpiresInSecond: seconds,
			Headers:         headers,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	})
	log.Printf("otpdelivery HTTPS listening on https://%s", address)
	log.Fatal(server.ListenAndServeTLS("", ""))
}

// runSMTP serves a minimal implicit-TLS ESMTP loop: greeting, EHLO,
// optional AUTH PLAIN, MAIL/RCPT/DATA, QUIT. The full DATA payload is
// recorded so harnesses can assert on rendered headers and body.
func runSMTP(records *recorder, address string, certificate tls.Certificate, user, password string) {
	listener, err := tls.Listen("tcp", address, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		log.Fatalf("smtp listen: %v", err)
	}
	log.Printf("otpdelivery SMTP listening on %s", address)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go serveSMTPConn(records, conn, user, password)
	}
}

func serveSMTPConn(records *recorder, conn net.Conn, user, password string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	writeLine := func(line string) {
		_, _ = writer.WriteString(line + "\r\n")
		_ = writer.Flush()
	}
	writeLine("220 otpdelivery ESMTP fixture")

	var mailFrom, rcptTo, data string
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
			// RFC 5321 multiline replies: every line but the last uses the
			// "-" separator, or clients end the reply early.
			writeLine("250-otpdelivery")
			writeLine("250-AUTH PLAIN")
			writeLine("250 8BITMIME")
		case "AUTH":
			fields := strings.Fields(line)
			accepted := len(fields) == 3 && strings.EqualFold(fields[1], "PLAIN")
			if accepted && user != "" {
				decoded, decodeErr := base64.StdEncoding.DecodeString(fields[2])
				parts := strings.Split(string(decoded), "\x00")
				accepted = decodeErr == nil && len(parts) == 3 && parts[1] == user && parts[2] == password
			}
			if accepted {
				writeLine("235 2.7.0 Authentication successful")
			} else {
				writeLine("535 5.7.8 Authentication credentials invalid")
			}
		case "MAIL":
			mailFrom = angleAddress(line)
			writeLine("250 2.1.0 Ok")
		case "RCPT":
			rcptTo = angleAddress(line)
			writeLine("250 2.1.5 Ok")
		case "DATA":
			writeLine("354 End data with <CR><LF>.<CR><LF>")
			var body strings.Builder
			for {
				dataLine, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}
				if strings.TrimRight(dataLine, "\r\n") == "." {
					break
				}
				body.WriteString(dataLine)
			}
			data = body.String()
			writeLine("250 2.0.0 Ok: queued")
		case "QUIT":
			if data != "" {
				records.write(smtpDelivery{
					Kind:       "smtp",
					ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
					MailFrom:   mailFrom,
					RcptTo:     rcptTo,
					Data:       data,
				})
			}
			writeLine("221 2.0.0 Bye")
			return
		default:
			writeLine("502 5.5.2 Command not implemented")
		}
	}
}

// angleAddress pulls <addr> out of "MAIL FROM:<addr> ..." lines.
func angleAddress(line string) string {
	open := strings.IndexByte(line, '<')
	close := strings.LastIndexByte(line, '>')
	if open < 0 || close <= open {
		return ""
	}
	return line[open+1 : close]
}
