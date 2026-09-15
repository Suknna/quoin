package delivery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// destinationPolicy decides whether a concrete IP may be dialed. It is
// checked for every IP of every dial, after DNS resolution, so a changed DNS
// answer cannot bypass the configured restrictions.
//
// Hard-denied regardless of configuration: unspecified, multicast, broadcast
// and link-local addresses (169.254.0.0/16 hosts the cloud metadata
// endpoints). Private, loopback and other non-global ranges are denied
// unless explicitly allowlisted; public unicast is allowed.
type destinationPolicy struct {
	allow []*net.IPNet
}

func newDestinationPolicy(allowCIDRs []string) (*destinationPolicy, error) {
	policy := &destinationPolicy{}
	for _, cidr := range allowCIDRs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			return nil, errors.New("allow CIDRs must not contain empty entries")
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("allow CIDR %q is not a valid CIDR: %w", cidr, err)
		}
		policy.allow = append(policy.allow, network)
	}
	return policy, nil
}

// validate returns a non-nil error when the IP must never be dialed. The
// error names the address class, never payload or secret content.
func (p *destinationPolicy) validate(ip net.IP) error {
	if ip == nil {
		return errors.New("destination address is empty")
	}
	if v4 := ip.To4(); v4 != nil {
		// Classify IPv4-mapped IPv6 addresses by their IPv4 reality.
		ip = v4
	}
	if reason := hardDeniedReason(ip); reason != "" {
		return fmt.Errorf("destination %s is denied: %s", ip, reason)
	}
	for _, network := range p.allow {
		if network.Contains(ip) {
			return nil
		}
	}
	if reason := nonPublicReason(ip); reason != "" {
		return fmt.Errorf("destination %s is denied: %s (configure an explicit allow CIDR to reach it)", ip, reason)
	}
	return nil
}

func hardDeniedReason(ip net.IP) string {
	if ip.IsUnspecified() {
		return "unspecified address"
	}
	if ip.IsMulticast() {
		return "multicast address"
	}
	if ip.IsLinkLocalUnicast() {
		// Cloud metadata endpoints live here; this deny wins over allow CIDRs.
		return "link-local address (metadata endpoints are never dialable)"
	}
	if ip.Equal(net.IPv4bcast) {
		return "broadcast address"
	}
	return ""
}

func nonPublicReason(ip net.IP) string {
	switch {
	case ip.IsLoopback():
		return "loopback address"
	case ip.IsPrivate():
		return "private network address"
	case ipIn(ip, "100.64.0.0/10"):
		return "shared address space (CGNAT)"
	case ipIn(ip, "198.18.0.0/15"):
		return "benchmarking address space"
	case ipIn(ip, "192.0.0.0/24"),
		ipIn(ip, "192.0.2.0/24"),
		ipIn(ip, "198.51.100.0/24"),
		ipIn(ip, "203.0.113.0/24"),
		ipIn(ip, "240.0.0.0/4"),
		ipIn(ip, "2001:db8::/32"):
		return "reserved or documentation address space"
	default:
		return ""
	}
}

func ipIn(ip net.IP, cidr string) bool {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		// Compile-time constant CIDRs; unreachable.
		return false
	}
	return network.Contains(ip)
}

func (p *destinationPolicy) validateResolvedIPs(ips []net.IP) ([]net.IP, error) {
	var allowed []net.IP
	var firstDenial error
	for _, ip := range ips {
		err := p.validate(ip)
		if err == nil {
			allowed = append(allowed, ip)
			continue
		}
		if firstDenial == nil {
			firstDenial = err
		}
	}
	if len(allowed) == 0 {
		if firstDenial != nil {
			return nil, firstDenial
		}
		return nil, errors.New("destination resolved to no usable addresses")
	}
	return allowed, nil
}

// dialValidated resolves host and dials the first allowed address, trying
// the remaining allowed addresses on transport failure. Validating at dial
// time closes the DNS-rebinding window.
func (p *destinationPolicy) dialValidated(ctx context.Context, dialer *net.Dialer, host, port string) (net.Conn, error) {
	ips, err := resolveHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve destination host: %w", err)
	}
	allowed, err := p.validateResolvedIPs(ips)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range allowed {
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no destination address was dialable")
	}
	return nil, fmt.Errorf("dial destination: %w", lastErr)
}

// resolveHost rejects zone-scoped addresses: they are never valid outbound
// delivery destinations.
func resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if addr.Zone() != "" {
			return nil, errors.New("destination host resolves to a zone-scoped address, which is never dialable")
		}
		ips = append(ips, addr.AsSlice())
	}
	return ips, nil
}
