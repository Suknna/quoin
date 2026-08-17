// relayclient replays a webhook body through the same SteleRelay gRPC path
// with a chosen relay_id. It is used by the ticket acceptance to prove that
// replaying the SAME relay_id does not duplicate the occurrence (CONTEXT
// 「Stele」: internal retries reuse the id; Alertmanager-originated retries
// are new deliveries).
//
// Usage:
//
//	relayclient -endpoint <quoin-runtime:8443> -ca <ca.pem> -token <file> \
//	  -relay-id <id> -source <id> -credential <id> -snapshot <version> \
//	  -body <file>
//
// Prints the DeliveryStatus and exits 0 only for ACCEPTED.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func main() {
	endpoint := flag.String("endpoint", "", "quoin runtime endpoint host:port")
	caFile := flag.String("ca", "", "deployment runtime CA file")
	tokenFile := flag.String("token", "", "stele service token file")
	relayID := flag.String("relay-id", "", "relay id (reuse to replay)")
	sourceID := flag.Int64("source", 0, "alert source id")
	credentialID := flag.Int64("credential", 0, "credential id")
	snapshot := flag.Uint64("snapshot", 0, "credential snapshot version")
	bodyFile := flag.String("body", "", "exact webhook body file")
	flag.Parse()
	if *endpoint == "" || *caFile == "" || *tokenFile == "" || *relayID == "" || *sourceID == 0 || *credentialID == 0 || *snapshot == 0 || *bodyFile == "" {
		fmt.Fprintln(os.Stderr, "all flags are required")
		os.Exit(2)
	}
	caPEM, err := os.ReadFile(*caFile)
	if err != nil {
		fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		fatal(fmt.Errorf("invalid CA PEM"))
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fatal(err)
	}
	body, err := os.ReadFile(*bodyFile)
	if err != nil {
		fatal(err)
	}
	conn, err := grpc.NewClient(*endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "quoin", MinVersion: tls.VersionTLS13})),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(20<<20), grpc.MaxCallRecvMsgSize(20<<20)))
	if err != nil {
		fatal(err)
	}
	defer conn.Close()
	client := runtimev1.NewSteleRelayClient(conn)
	// RUNTIME-AUTH-006: the wire text is base64url; the token file holds the
	// raw 32 bytes.
	tokenText := base64.RawURLEncoding.EncodeToString(token)
	md := metadata.Pairs("authorization", "Bearer "+tokenText, "x-quoin-release", buildinfo.Release)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := client.Deliver(metadata.NewOutgoingContext(ctx, md), &runtimev1.DeliveryRelayRequest{
		RelayId: *relayID, SourceId: *sourceID, CredentialId: *credentialID,
		CredentialSnapshotVersion: *snapshot, Protocol: "alertmanager",
		Body: body, ReceivedAt: timestamppb.New(time.Now().UTC()), ReleaseVersion: buildinfo.Release,
	})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("status=%s detail=%s\n", response.GetStatus(), response.GetDetail())
	if response.GetStatus() != runtimev1.DeliveryStatus_DELIVERY_STATUS_ACCEPTED {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "relayclient:", err)
	os.Exit(1)
}
