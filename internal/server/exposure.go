package server

import (
	"fmt"
	"net"
)

// checkExposure is the static-driver fence (components.md §11 enforcement
// contract): account-switch login is only honest where the network is trusted.
// Loopback and private ranges (RFC1918, CGNAT 100.64/10 — Tailscale — ULA,
// link-local) pass; a globally reachable bind with static accounts refuses to
// start unless the operator sets the deliberately scary --allow-unsecured.
// Enforced here, outside any driver, so no driver can forget to be checked.
func checkExposure(bindAddr string, staticAccounts, allowUnsecured bool, ifaceAddrs func() ([]net.Addr, error)) error {
	if !staticAccounts {
		return nil // nothing attributable to protect; bearer tokens carry their own secrecy
	}
	host, _, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return fmt.Errorf("cannot parse listen address %q: %w", bindAddr, err)
	}
	exposed, where := hostExposed(host, ifaceAddrs)
	if !exposed {
		return nil
	}
	if allowUnsecured {
		return nil // the caller logs the override loudly
	}
	return fmt.Errorf(
		"refusing to start: account-switch login (passwordless) is enabled and %s is reachable beyond your private network. "+
			"Anyone on the internet could open the account picker. "+
			"Bind a private address (e.g. your LAN or Tailscale IP), or — only if you truly mean it — start with --allow-unsecured",
		where)
}

// hostExposed reports whether the bind host makes the listener reachable from
// non-private networks, and a human-readable description of what is exposed.
func hostExposed(host string, ifaceAddrs func() ([]net.Addr, error)) (bool, string) {
	if host == "localhost" {
		return false, ""
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname bind we cannot classify: treat as exposed, name it.
		return true, fmt.Sprintf("hostname %q (unclassifiable)", host)
	}
	if ip.IsUnspecified() {
		// 0.0.0.0 / :: listens on every interface — exposed iff any interface
		// address is global.
		addrs, err := ifaceAddrs()
		if err != nil {
			return true, "all interfaces (could not enumerate them)"
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipGlobal(ipn.IP) {
				return true, fmt.Sprintf("all interfaces including public address %s", ipn.IP)
			}
		}
		return false, ""
	}
	if ipGlobal(ip) {
		return true, fmt.Sprintf("public address %s", ip)
	}
	return false, ""
}

// cgnat is 100.64.0.0/10 (RFC 6598) — the range Tailscale assigns from.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// ipGlobal: not loopback, not private (RFC1918 / ULA), not link-local, not CGNAT.
func ipGlobal(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !cgnat.Contains(ip)
}
