package app

// Component identity for the internal gRPC plane (ADR-0009): every internal
// connection is mTLS-authenticated with a deployment CA-signed client
// certificate, and the leaf certificate's CommonName is the component name
// (plinth/stele). Quoin authorizes services by that CN — the simplified
// kube-apiserver model.

import (
	"context"
	"crypto/x509"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// componentIdentity extracts the verified mTLS client identity from a gRPC
// call context. The listener already verified the chain against the
// deployment CA (RequireAndVerifyClientCert); here the leaf must carry the
// ClientAuth EKU and a non-empty CommonName.
func componentIdentity(ctx context.Context) (string, bool) {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok {
		return "", false
	}
	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return "", false
	}
	leaf := tlsInfo.State.VerifiedChains[0][0]
	if leaf.Subject.CommonName == "" {
		return "", false
	}
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			return leaf.Subject.CommonName, true
		}
	}
	return "", false
}

// requireComponentIdentity reports whether the verified mTLS client identity
// is exactly the named component (CN match). Slot names double as component
// certificate CNs, so callers pass qruntime.SlotPlinth or "stele".
func requireComponentIdentity(ctx context.Context, component string) bool {
	identity, ok := componentIdentity(ctx)
	return ok && identity == component
}
