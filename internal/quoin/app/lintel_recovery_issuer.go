package app

// The temporary same-Release Lintel recovery Runtime service (T35,
// OPS-HELPER-005 / VERIFY-RECOVERY-002) runs as TWO exec sessions on the
// recovery holder, split exactly along the stream-lifecycle dependency that
// would otherwise deadlock: `docker compose run -i` / `kubectl run -i`
// consume stdin until EOF, and a single long-lived serve session would hold
// that EOF until the first-auth wait ends — which itself needs the register
// one-shot to finish. The `issue` session mints the token, writes the single
// stdout envelope and exits as soon as the Register RPC consumes it (EOF
// propagates immediately); the `await` session then serves the ordinary
// Connect handshake and exits on the exact replacement generation's first
// authenticated Hello. Stdout never carries anything but the envelope; all
// diagnostics go to stderr with non-secret stable codes.

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/grpc"
)

// recoveryEvent writes a non-secret JSON line to the recovery process's
// stderr (OPS-LOG-001 shape; stdout stays reserved for the envelope).
func recoveryEvent(stderr io.Writer, level, code, message string) {
	fmt.Fprintf(stderr, `{"ts":%q,"level":%q,"component":"quoin","release":%q,"code":%q,"message":%q}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano), level, buildinfo.Release, code, message)
}

// recoveryServe registers the REAL runtime gRPC surface (same release, same
// embedded Journey Catalog digest) on :8443 with the deployment's Runtime
// TLS identity and starts serving. Registration happens strictly before
// Serve — grpc fatal-errors on late RegisterService.
func recoveryServe(config contract.QuoinConfig, slots *qruntime.Service) (*grpc.Server, net.Listener, chan error, error) {
	server := grpc.NewServer()
	RegisterRuntimeControl(server, NewRuntimeControl(slots, buildinfo.Release, catalog.Digest(), nil))
	listener, err := net.Listen("tcp", ":8443")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listen recovery Runtime gRPC: %w", err)
	}
	certificate, err := tls.LoadX509KeyPair(config.RuntimeTLSCertificateFile, config.RuntimeTLSPrivateKeyFile)
	if err != nil {
		_ = listener.Close()
		return nil, nil, nil, fmt.Errorf("load Runtime TLS identity: %w", err)
	}
	// grpc-go enforces ALPN "h2" for TLS clients; the Runtime listener must
	// offer it exactly like the normal server.
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(tls.NewListener(listener, tlsConfig)) }()
	return server, listener, serveErr, nil
}

// LintelRecoveryIssue is the short session: enter/resume the frozen recovery
// fence, mint the one-time token (in memory only), serve Register/Connect,
// and exit as soon as the token is consumed — 0 once the replacement is
// confirmed, or after the registration deadline. On resume (replacement
// already current) it exits 0 immediately without any envelope.
func LintelRecoveryIssue(ctx context.Context, config contract.QuoinConfig, fence qruntime.LintelRecoveryFence, registrationTimeout time.Duration, stdout, stderr io.Writer) error {
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		return err
	}
	defer database.Close()
	slots := qruntime.NewService(database.SQL)
	begin, err := slots.BeginLintelRecoveryRegistration(ctx, fence)
	if err != nil {
		return err
	}
	server, listener, serveErr, err := recoveryServe(config, slots)
	if err != nil {
		return err
	}
	defer server.Stop()
	defer listener.Close()

	if begin.NeedsRegistration {
		envelope, err := json.Marshal(map[string]any{"slot": "lintel", "generation": begin.ReplacementGeneration, "token": begin.RegistrationToken})
		if err != nil {
			return err
		}
		// The only stdout bytes this process ever emits: the caller pipes
		// them directly into `lintel register` stdin and never captures
		// them; EOF propagates when this session exits.
		if _, err := fmt.Fprintf(stdout, "%s\n", envelope); err != nil {
			return err
		}
		fmt.Fprintf(stderr, `{"ts":%q,"level":"info","component":"quoin","release":%q,"code":"lintel_recovery.serving","maintenance_revision":%d,"replacement_generation":%d,"needs_registration":true}`+"\n",
			time.Now().UTC().Format(time.RFC3339Nano), buildinfo.Release, begin.MaintenanceRevision, begin.ReplacementGeneration)
		deadline := time.Now().Add(registrationTimeout)
		for {
			var confirmed sql.NullString
			err := database.SQL.QueryRowContext(ctx, `SELECT confirmed_at FROM runtime_credentials WHERE slot='lintel' AND generation=?`, begin.ReplacementGeneration).Scan(&confirmed)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if confirmed.Valid {
				fmt.Fprintf(stderr, `{"ts":%q,"level":"info","component":"quoin","release":%q,"code":"lintel_recovery.registered","replacement_generation":%d}`+"\n",
					time.Now().UTC().Format(time.RFC3339Nano), buildinfo.Release, begin.ReplacementGeneration)
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-serveErr:
				if err != nil {
					return fmt.Errorf("recovery Runtime gRPC ended: %w", err)
				}
				return errors.New("recovery Runtime gRPC ended before registration")
			case <-time.After(500 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("one-time registration token was not consumed within %s", registrationTimeout)
			}
		}
	}
	// Resume: a replacement credential already exists; the await session
	// owns the first-auth wait.
	fmt.Fprintf(stderr, `{"ts":%q,"level":"info","component":"quoin","release":%q,"code":"lintel_recovery.serving","maintenance_revision":%d,"replacement_generation":%d,"needs_registration":false}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano), buildinfo.Release, begin.MaintenanceRevision, begin.ReplacementGeneration)
	return nil
}

// LintelRecoveryAwait is the long session: serve the ordinary Connect
// handshake and exit 0 once the exact replacement generation records its
// first authenticated Hello.
func LintelRecoveryAwait(ctx context.Context, config contract.QuoinConfig, fence qruntime.LintelRecoveryFence, firstAuthTimeout time.Duration, stderr io.Writer) error {
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		return err
	}
	defer database.Close()
	slots := qruntime.NewService(database.SQL)
	begin, err := slots.BeginLintelRecoveryRegistration(ctx, fence)
	if err != nil {
		return err
	}
	server, listener, serveErr, err := recoveryServe(config, slots)
	if err != nil {
		return err
	}
	// Close actively when the wait ends: the Lintel control stream stays
	// long-lived by design, so a graceful stop would wait forever; the
	// runtime reconnects to the normal Quoin once it is started.
	defer server.Stop()
	defer listener.Close()

	fmt.Fprintf(stderr, `{"ts":%q,"level":"info","component":"quoin","release":%q,"code":"lintel_recovery.awaiting","maintenance_revision":%d,"replacement_generation":%d}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano), buildinfo.Release, begin.MaintenanceRevision, begin.ReplacementGeneration)
	deadline := time.Now().Add(firstAuthTimeout)
	for {
		var firstAuth sql.NullString
		err := database.SQL.QueryRowContext(ctx, `SELECT first_authenticated_at FROM runtime_credentials WHERE slot='lintel' AND generation=?`, begin.ReplacementGeneration).Scan(&firstAuth)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if firstAuth.Valid {
			fmt.Fprintf(stderr, `{"ts":%q,"level":"info","component":"quoin","release":%q,"code":"lintel_recovery.first_authenticated","replacement_generation":%d}`+"\n",
				time.Now().UTC().Format(time.RFC3339Nano), buildinfo.Release, begin.ReplacementGeneration)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-serveErr:
			if err != nil {
				return fmt.Errorf("recovery Runtime gRPC ended: %w", err)
			}
			return errors.New("recovery Runtime gRPC ended before the replacement first Hello")
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("replacement generation %d did not send its first Hello within %s", begin.ReplacementGeneration, firstAuthTimeout)
		}
	}
}

// HoldLintelRecovery keeps a distroless recovery Pod's main container alive
// without any shell: the issue/await sessions run through `kubectl exec -i`,
// so the registration envelope never enters the pod logs. The holder exits
// cleanly on the pod termination signal.
func HoldLintelRecovery(ctx context.Context) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-signals:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
