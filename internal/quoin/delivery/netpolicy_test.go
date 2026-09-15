package delivery

import (
	"net"
	"strings"
	"testing"
)

func TestDestinationPolicy(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		allow    []string
		denied   bool
		mentions string
	}{
		{name: "public allowed", ip: "8.8.8.8"},
		{name: "public ipv6 allowed", ip: "2606:4700::1111"},
		{name: "metadata endpoint hard denied", ip: "169.254.169.254", allow: []string{"169.254.0.0/16"}, denied: true, mentions: "link-local"},
		{name: "mapped metadata hard denied", ip: "::ffff:169.254.169.254", allow: []string{"0.0.0.0/0"}, denied: true, mentions: "link-local"},
		{name: "ipv6 link-local hard denied", ip: "fe80::1", allow: []string{"fe80::/10"}, denied: true, mentions: "link-local"},
		{name: "unspecified denied", ip: "0.0.0.0", denied: true, mentions: "unspecified"},
		{name: "ipv6 unspecified denied", ip: "::", denied: true, mentions: "unspecified"},
		{name: "multicast denied", ip: "224.0.0.1", denied: true, mentions: "multicast"},
		{name: "broadcast denied", ip: "255.255.255.255", denied: true, mentions: "broadcast"},
		{name: "loopback needs allow", ip: "127.0.0.1", denied: true, mentions: "loopback"},
		{name: "loopback allowlisted", ip: "127.0.0.1", allow: []string{"127.0.0.0/8"}},
		{name: "rfc1918 needs allow", ip: "10.1.2.3", denied: true, mentions: "private"},
		{name: "rfc1918 allowlisted", ip: "192.168.7.9", allow: []string{"192.168.0.0/16"}},
		{name: "cgnat needs allow", ip: "100.64.0.1", denied: true, mentions: "CGNAT"},
		{name: "benchmark denied", ip: "198.18.0.5", denied: true, mentions: "benchmarking"},
		{name: "reserved v4 denied", ip: "240.0.0.1", denied: true, mentions: "reserved"},
		{name: "documentation range denied", ip: "203.0.113.9", denied: true, mentions: "reserved"},
		{name: "documentation ipv6 denied", ip: "2001:db8::1", denied: true, mentions: "reserved"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := newDestinationPolicy(tc.allow)
			if err != nil {
				t.Fatalf("newDestinationPolicy: %v", err)
			}
			err = policy.validate(net.ParseIP(tc.ip))
			if tc.denied {
				if err == nil {
					t.Fatalf("ip %s should be denied", tc.ip)
				}
				if !strings.Contains(err.Error(), tc.mentions) {
					t.Fatalf("error %q does not mention %q", err, tc.mentions)
				}
				return
			}
			if err != nil {
				t.Fatalf("ip %s should be allowed, got %v", tc.ip, err)
			}
		})
	}
}

func TestDestinationPolicyConstructionErrors(t *testing.T) {
	if _, err := newDestinationPolicy([]string{"10.0.0.0/8", "bogus"}); err == nil {
		t.Fatal("invalid CIDR must fail construction")
	}
	if _, err := newDestinationPolicy([]string{" "}); err == nil {
		t.Fatal("blank CIDR must fail construction")
	}
}

func TestValidateResolvedIPsFiltersAndDenies(t *testing.T) {
	policy, err := newDestinationPolicy([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("newDestinationPolicy: %v", err)
	}
	allowed, err := policy.validateResolvedIPs([]net.IP{
		net.ParseIP("10.0.0.1"),
		net.ParseIP("127.0.0.1"),
		net.ParseIP("169.254.169.254"),
		net.ParseIP("127.0.0.2"),
	})
	if err != nil {
		t.Fatalf("validateResolvedIPs: %v", err)
	}
	if len(allowed) != 2 {
		t.Fatalf("allowed = %v, want the two loopback addresses", allowed)
	}

	if _, err := policy.validateResolvedIPs([]net.IP{net.ParseIP("10.0.0.1")}); err == nil {
		t.Fatal("a resolution with no allowed addresses must fail")
	}
	if _, err := policy.validateResolvedIPs(nil); err == nil {
		t.Fatal("an empty resolution must fail")
	}
}
