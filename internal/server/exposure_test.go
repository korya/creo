package server

import (
	"net"
	"strings"
	"testing"
)

func addrs(cidrs ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range cidrs {
			_, ipn, err := net.ParseCIDR(c)
			if err != nil {
				return nil, err
			}
			out = append(out, ipn)
		}
		return out, nil
	}
}

func TestCheckExposure(t *testing.T) {
	privateOnly := addrs("127.0.0.1/8", "192.168.1.10/24")
	withPublic := addrs("127.0.0.1/8", "192.168.1.10/24", "203.0.113.7/24")

	cases := []struct {
		name           string
		bind           string
		static         bool
		allowUnsecured bool
		ifaces         func() ([]net.Addr, error)
		wantRefusal    bool
	}{
		{"loopback", "127.0.0.1:8080", true, false, privateOnly, false},
		{"localhost name", "localhost:8080", true, false, privateOnly, false},
		{"lan rfc1918", "192.168.1.10:8080", true, false, privateOnly, false},
		{"lan 10/8", "10.0.0.5:8080", true, false, privateOnly, false},
		{"tailscale cgnat", "100.101.102.103:8080", true, false, privateOnly, false},
		{"ipv6 ula", "[fd7a:115c:a1e0::1]:8080", true, false, privateOnly, false},
		{"public ip", "203.0.113.7:8080", true, false, privateOnly, true},
		{"public ip, no static accounts", "203.0.113.7:8080", false, false, privateOnly, false},
		{"public ip, override", "203.0.113.7:8080", true, true, privateOnly, false},
		{"wildcard, private host", "0.0.0.0:8080", true, false, privateOnly, false},
		{"wildcard, public interface", "0.0.0.0:8080", true, false, withPublic, true},
		{"wildcard v6, public interface", "[::]:8080", true, false, withPublic, true},
		{"unclassifiable hostname", "myhost.example.com:8080", true, false, privateOnly, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkExposure(c.bind, c.static, c.allowUnsecured, c.ifaces)
			if c.wantRefusal && err == nil {
				t.Fatal("want refusal, got nil")
			}
			if !c.wantRefusal && err != nil {
				t.Fatalf("want start, got refusal: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "allow-unsecured") {
				t.Fatalf("refusal must name the override: %v", err)
			}
		})
	}
}
