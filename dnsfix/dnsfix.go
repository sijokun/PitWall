// Package dnsfix routes name lookups through public DNS servers. The
// reMarkable's /etc/resolv.conf is often empty or points at a resolver that
// never answers, so api.openf1.org and livetiming.formula1.com fail to
// resolve even though the network is up.
//
// Install replaces net.DefaultResolver — which every net.Dialer, and so
// every HTTP client in this program, uses — with one that queries 8.8.8.8
// first and 1.1.1.1 as backup. UDP "dials" almost never fail (no packet is
// sent), so a dead server shows up as a query timeout rather than a dial
// error; the servers are therefore rotated per dial, and the Go resolver's
// own retry lands on the next one. Nameservers configured on the system are
// appended as a last resort.
package dnsfix

import (
	"bufio"
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Env overrides the server list (comma-separated, port optional). Setting it
// to "system" leaves Go's own resolver alone.
const Env = "F1_DNS"

// Public are the resolvers tried first, in order.
var Public = []string{"8.8.8.8:53", "1.1.1.1:53"}

// next rotates the starting server across dials.
var next atomic.Uint64

// Install points net.DefaultResolver at the public servers. Call once at
// startup, before any lookups.
func Install() {
	list := servers()
	if len(list) == 0 {
		return // F1_DNS=system
	}
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			start := int(next.Add(1)-1) % len(list)
			var firstErr error
			for i := range list {
				c, err := d.DialContext(ctx, network, list[(start+i)%len(list)])
				if err == nil {
					return c, nil
				}
				if firstErr == nil {
					firstErr = err
				}
			}
			return nil, firstErr
		},
	}
}

// servers is the resolver list: the override or the public servers, then any
// system nameservers as a fallback. Nil means "leave the resolver alone".
func servers() []string {
	var list []string
	switch v := strings.TrimSpace(os.Getenv(Env)); {
	case strings.EqualFold(v, "system"):
		return nil
	case v != "":
		for _, s := range strings.Split(v, ",") {
			if s = withPort(strings.TrimSpace(s)); s != "" {
				list = append(list, s)
			}
		}
	default:
		list = append(list, Public...)
	}
	seen := map[string]bool{}
	for _, s := range list {
		seen[s] = true
	}
	for _, s := range systemServers() {
		if !seen[s] {
			seen[s] = true
			list = append(list, s)
		}
	}
	return list
}

// systemServers reads the nameservers from /etc/resolv.conf (empty when the
// file is missing or, as on the device, has none).
func systemServers() []string {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			if s := withPort(fields[1]); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// withPort appends the DNS port when the address has none.
func withPort(s string) string {
	if s == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s
	}
	return net.JoinHostPort(s, "53")
}
