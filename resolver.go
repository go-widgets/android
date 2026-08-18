// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// EnvDNS names the environment variable the Java host sets to the system's DNS
// servers, comma separated, when it spawns the application.
//
// Android has no /etc/resolv.conf, and its resolver lives behind libc, which is
// behind cgo -- the one thing a CGO-free application does not have. Go's pure
// resolver therefore finds no nameserver, falls back to localhost, and every
// lookup fails with a connection refused. The host can see the real servers
// through ConnectivityManager, so it passes them here.
const EnvDNS = "GW_ANDROID_DNS"

// resolverDial is the dialer InstallResolver uses, replaceable in tests.
var resolverDial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext

// InstallResolver points net.DefaultResolver at the DNS servers named by
// $GW_ANDROID_DNS, and reports whether it did.
//
// It returns false, changing nothing, when the variable is unset or lists no
// usable server: off Android, or in an APK whose application never asked for
// ACCESS_NETWORK_STATE, the default resolver is either already correct or
// deliberately absent, and silently rewiring it would be worse than leaving it.
//
// Dial is called with the address Go picked from its (empty) configuration and
// ignores it, substituting a configured server chosen by attempt: the first is
// tried first, and a retry moves to the next, so a dead server does not pin
// every lookup to it. Each server keeps the network Go asked for -- "udp" or
// "tcp", the latter when a truncated answer must be re-fetched -- because the
// choice is the resolver's, not ours.
func InstallResolver() bool {
	servers := parseServers(os.Getenv(EnvDNS))
	if len(servers) == 0 {
		return false
	}
	var attempt atomic.Uint64
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			n := attempt.Add(1) - 1
			return resolverDial(ctx, network, servers[n%uint64(len(servers))])
		},
	}
	return true
}

// parseServers splits the host's comma-separated list into dialable addresses,
// dropping empties and bracketing IPv6 literals, which need it before a port.
func parseServers(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, net.JoinHostPort(s, "53"))
	}
	return out
}
