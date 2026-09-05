// Command faultclient is the deterministic TCP exchange actor of the
// network-fault primitives. It runs inside the qualification's docker
// network where TCP RST and pacing reach the client unmodified by host
// port-forwarding: `upstream` serves the frozen paced 64 KiB body and
// `exchange` dials a target, writes the probe payload and reads until
// EOF, deadline or reset, reporting the raw observation as JSON. It
// never classifies: the deterministic verifier on the host owns
// classification (VERIFY-VERDICT-004).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

// exchangeReport is the raw client-side fact set of one exchange.
type exchangeReport struct {
	Address     string `json:"address"`
	Received    int    `json:"received"`
	ElapsedMS   int64  `json:"elapsedMs"`
	DeadlineHit bool   `json:"deadlineHit"`
	ErrorText   string `json:"errorText,omitempty"`
	Reset       bool   `json:"reset"`
	Eof         bool   `json:"eof"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: faultclient <upstream|exchange> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "upstream":
		upstreamCommand(os.Args[2:])
	case "exchange":
		exchangeCommand(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: faultclient <upstream|exchange> [flags]")
		os.Exit(2)
	}
}

// upstreamCommand serves the frozen paced body forever, one connection
// at a time per goroutine.
func upstreamCommand(arguments []string) {
	flags := flag.NewFlagSet("upstream", flag.ContinueOnError)
	address := flags.String("address", ":19090", "listen address")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "faultclient upstream:", err)
		os.Exit(1)
	}
	fmt.Printf("faultclient upstream listening on %s\n", *address)
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go servePacedBody(connection)
	}
}

func servePacedBody(connection net.Conn) {
	defer connection.Close()
	body := frozenBody()
	for offset := 0; offset < len(body); offset += chunkSize {
		if _, err := connection.Write(body[offset : offset+chunkSize]); err != nil {
			return
		}
		time.Sleep(pause)
	}
}

// exchangeCommand dials once and reports the raw observation.
func exchangeCommand(arguments []string) {
	flags := flag.NewFlagSet("exchange", flag.ContinueOnError)
	address := flags.String("address", "", "target address host:port")
	payload := flags.String("payload", "quoin-t40-probe", "probe payload written after dialing")
	deadline := flags.Duration("deadline", 8*time.Second, "read deadline")
	report := flags.String("report", "", "where the JSON report is written (default stdout)")
	if err := flags.Parse(arguments); err != nil || *address == "" || flags.NArg() != 0 {
		os.Exit(2)
	}
	observation := exchange(*address, *payload, *deadline)
	body, err := json.MarshalIndent(observation, "", "  ")
	if err != nil {
		os.Exit(1)
	}
	if *report == "" {
		fmt.Println(string(body))
		os.Exit(0)
	}
	if err := os.WriteFile(*report, append(body, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "faultclient exchange:", err)
		os.Exit(1)
	}
	fmt.Printf("faultclient exchange received=%d elapsedMs=%d reset=%t eof=%t\n", observation.Received, observation.ElapsedMS, observation.Reset, observation.Eof)
}

func exchange(address, payload string, readDeadline time.Duration) exchangeReport {
	report := exchangeReport{Address: address}
	started := time.Now()
	connection, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		report.ElapsedMS = time.Since(started).Milliseconds()
		report.ErrorText = err.Error()
		// A reset during the handshake is the same fact as a reset
		// mid-stream: the peer sent RST.
		report.Reset = contains(err.Error(), "connection reset by peer")
		return report
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(readDeadline))
	if _, err := connection.Write([]byte(payload)); err != nil {
		report.ElapsedMS = time.Since(started).Milliseconds()
		report.ErrorText = err.Error()
		return report
	}
	buffer := make([]byte, 8192)
	for {
		read, err := connection.Read(buffer)
		report.Received += read
		if err != nil {
			report.ElapsedMS = time.Since(started).Milliseconds()
			report.ErrorText = err.Error()
			if os.IsTimeout(err) {
				report.DeadlineHit = true
			}
			report.Reset = contains(err.Error(), "connection reset by peer")
			report.Eof = err.Error() == "EOF" || contains(err.Error(), "EOF")
			return report
		}
	}
}

func contains(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
