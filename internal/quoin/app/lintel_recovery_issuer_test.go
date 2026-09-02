package app_test

// T35 temporary recovery issuer coverage: the real gRPC path — Register
// through the recovery one-time token, the replacement's first Hello with
// the real embedded catalog digest, generation-bound first-auth wait and the
// active close. Stdout must carry exactly one envelope line and no diagnostics.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func issuerDigest(parts ...string) string {
	sum := sha256.Sum256([]byte(filepath.Join(parts...)))
	return hex.EncodeToString(sum[:])
}

func TestTicket35RecoveryIssuerServesRegisterAndFirstHello(t *testing.T) {
	root := t.TempDir()
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory:             filepath.Join(root, "data"),
		RootKeyFile:               filepath.Join(root, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(root, "tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(root, "tls.key"),
		SteleServiceTokenFile:     filepath.Join(root, "stele"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// Old credential: real registration + first Hello.
	slots := qruntime.NewService(database.SQL)
	ctx := context.Background()
	var session [32]byte
	_, handle, _, err := slots.PrepareRegistration(ctx, "lintel", 1, session)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, generation, err := slots.RevealToken(handle, session)
	if err != nil {
		t.Fatal(err)
	}
	oldLong, _, err := slots.Register(ctx, "lintel", raw, generation, "boot-old", buildinfo.Release, buildinfo.Release)
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := slots.Adjudicate(ctx, oldLong, "lintel", "boot-old", 1, buildinfo.Release, buildinfo.Release, catalog.Digest(), catalog.Digest()); err != nil || !decision.Accepted {
		t.Fatalf("old hello: %+v %v", decision, err)
	}
	// The issuer takes the exclusive data-directory lock; the fixture's
	// handle must release it first (exactly like the stopped production
	// window where only the one-shot command runs).
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	fence := qruntime.LintelRecoveryFence{
		Backend: "compose", Disposition: "exclusively_reattached",
		DispositionDigest: issuerDigest("disposition"),
		FenceReportDigest: issuerDigest("fence"),
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdoutReader, stdoutWriter := io.Pipe()
	issueDone := make(chan error, 1)
	go func() {
		issueDone <- app.LintelRecoveryIssue(serveCtx, config, fence, 2*time.Minute, stdoutWriter, io.Discard)
	}()

	envelopeReader := bufio.NewReader(stdoutReader)
	line, err := envelopeReader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Slot       string `json:"slot"`
		Generation int64  `json:"generation"`
		Token      string `json:"token"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("envelope %q: %v", line, err)
	}
	if envelope.Slot != "lintel" || envelope.Generation != 2 || envelope.Token == "" {
		t.Fatalf("envelope=%+v", envelope)
	}

	// The register one-shot consumes the envelope; the issue session exits
	// as soon as the Register RPC is consumed.
	connection, err := grpc.DialContext(ctx, "127.0.0.1:8443", grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := runtimev1.NewRuntimeControlClient(connection)
	registerCtx, registerCancel := context.WithTimeout(ctx, 30*time.Second)
	defer registerCancel()
	registered, err := client.Register(metadata.NewOutgoingContext(registerCtx, metadata.Pairs("authorization", "Bearer "+envelope.Token)), &runtimev1.RegisterRuntimeRequest{
		Slot: runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL, OneTimeToken: envelope.Token,
		Generation: uint64(envelope.Generation), BootId: "boot-new", ReleaseVersion: buildinfo.Release,
	})
	if err != nil {
		t.Fatalf("recovery register: %v", err)
	}
	streamCtx, streamCancel := context.WithTimeout(ctx, 30*time.Second)
	defer streamCancel()
	stream, err := client.Connect(metadata.NewOutgoingContext(streamCtx, metadata.Pairs("authorization", "Bearer "+registered.GetLongTermToken())))
	if err != nil {
		t.Fatal(err)
	}
	hello := &runtimev1.Hello{
		Slot: runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL, BootId: "boot-new", ConnectionEpoch: 1,
		ReleaseVersion: buildinfo.Release, JourneyCatalogDigest: catalog.Digest(), JourneyCatalogVersion: catalog.Version,
		BrowserCapacitySlots: 1,
	}
	if err := stream.Send(&runtimev1.ControlEnvelope{MessageId: 1, ConnectionEpoch: 1, BootId: "boot-new", Msg: &runtimev1.ControlEnvelope_Hello{Hello: hello}}); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if helloAck := ack.GetHelloAck(); helloAck == nil || !helloAck.GetAccepted() {
		t.Fatalf("hello ack=%+v", ack)
	}

	select {
	case err := <-issueDone:
		if err != nil {
			t.Fatalf("issue session: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("issue session did not exit after registration")
	}
	awaitDone := make(chan error, 1)
	go func() {
		awaitDone <- app.LintelRecoveryAwait(serveCtx, config, fence, 2*time.Minute, io.Discard)
	}()
	select {
	case err := <-awaitDone:
		if err != nil {
			t.Fatalf("await session: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("issuer did not exit after first authenticated Hello")
	}
	// Stdout hygiene: nothing beyond the envelope line was written.
	if err := stdoutWriter.Close(); err != nil {
		t.Fatal(err)
	}
	rest, readErr := io.ReadAll(stdoutReader)
	if readErr != nil && readErr != io.EOF {
		t.Fatal(readErr)
	}
	if len(rest) != 0 {
		t.Fatalf("issuer stdout carried extra bytes: %q", rest)
	}
	var firstAuth *string
	verified, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	if err := verified.SQL.QueryRowContext(ctx, `SELECT first_authenticated_at FROM runtime_credentials WHERE slot='lintel' AND generation=2`).Scan(&firstAuth); err != nil || firstAuth == nil {
		t.Fatalf("first auth=%v err=%v", firstAuth, err)
	}
}
