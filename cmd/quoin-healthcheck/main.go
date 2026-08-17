// quoin-healthcheck asserts component ops endpoints from inside scratch
// containers. `quoin-healthcheck <url>` expects the frozen livez body; the
// --json mode captures readiness or metrics bodies for acceptance evidence.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	expectStatus := flag.Int("status", 0, "expected exact HTTP status; 0 uses the liveness contract")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: quoin-healthcheck [--status N] <url>")
		os.Exit(2)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if *expectStatus != 0 {
		fmt.Println(string(body))
		if response.StatusCode != *expectStatus {
			fmt.Fprintf(os.Stderr, "healthcheck status=%d want=%d\n", response.StatusCode, *expectStatus)
			os.Exit(1)
		}
		return
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok\n" {
		fmt.Fprintf(os.Stderr, "healthcheck status=%d body=%q\n", response.StatusCode, body)
		os.Exit(1)
	}
}
