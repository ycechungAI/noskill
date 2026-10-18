package perms

import (
	"net"
	"net/netip"
	"strings"
)

// CheckHost checks a host name (no port) against the network permission (SECURITY.md
// "Permissions"). Patterns: an exact name, "*.example.com" (subdomains only), "localhost", or "*"
// (any public name). IP literals are refused unless "localhost" is declared and the address is
// loopback: a skill names the hosts it talks to, and an address can't be reviewed at install.
func (s Set) CheckHost(host string) error {
	h := strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if h == "" {
		return deny("host", host, "empty host")
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if ip.IsLoopback() && s.hasPattern("localhost") {
			return nil
		}
		return deny("host", host, "IP addresses aren't allowed; declare a host name")
	}
	if net.ParseIP(h) != nil || strings.ContainsAny(h, "/@ ") {
		return deny("host", host, "not a host name")
	}
	for _, p := range s.Network {
		switch {
		case p == "localhost":
			if h == "localhost" {
				return nil
			}
		case p == "*":
			if h != "localhost" && !strings.HasSuffix(h, ".localhost") {
				return nil
			}
		case strings.HasPrefix(p, "*."):
			if strings.HasSuffix(h, p[1:]) {
				return nil
			}
		case h == strings.ToLower(p):
			return nil
		}
	}
	return deny("host", host, "not in this skill's network permission")
}

func (s Set) hasPattern(p string) bool {
	for _, n := range s.Network {
		if n == p {
			return true
		}
	}
	return false
}
