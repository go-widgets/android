// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
)

func TestParseServers(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"blanks only", " , ,, ", nil},
		{"one", "8.8.8.8", []string{"8.8.8.8:53"}},
		{"several, spaced", " 8.8.8.8 , 1.1.1.1 ", []string{"8.8.8.8:53", "1.1.1.1:53"}},
		// An IPv6 literal must be bracketed before a port, which is the whole
		// reason this goes through net.JoinHostPort rather than a concatenation.
		{"ipv6", "fd00::1", []string{"[fd00::1]:53"}},
		{"mixed", "fd00::1,8.8.8.8", []string{"[fd00::1]:53", "8.8.8.8:53"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseServers(tc.raw); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseServers(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestInstallResolverDeclinesWithoutServers pins the rule that matters most:
// off Android the default resolver is already correct, so a no-op must leave it
// exactly as it was rather than rewire it to nothing.
func TestInstallResolverDeclinesWithoutServers(t *testing.T) {
	before := net.DefaultResolver
	t.Cleanup(func() { net.DefaultResolver = before })

	for _, raw := range []string{"", " , "} {
		t.Setenv(EnvDNS, raw)
		if InstallResolver() {
			t.Fatalf("InstallResolver() = true for %q, want false", raw)
		}
		if net.DefaultResolver != before {
			t.Fatalf("the default resolver was replaced for %q", raw)
		}
	}
}

// TestInstallResolverDialsTheConfiguredServers proves the substitution: the
// address Go hands the Dial hook is ignored, and a configured server is used
// instead. Recording the network too pins that "tcp" is passed through rather
// than forced to "udp" -- the resolver asks for tcp to re-fetch a truncated
// answer, and overriding that would break long replies.
func TestInstallResolverDialsTheConfiguredServers(t *testing.T) {
	before, beforeDial := net.DefaultResolver, resolverDial
	t.Cleanup(func() { net.DefaultResolver, resolverDial = before, beforeDial })

	var gotNet, gotAddr []string
	sentinel := errors.New("dialed")
	resolverDial = func(_ context.Context, network, addr string) (net.Conn, error) {
		gotNet = append(gotNet, network)
		gotAddr = append(gotAddr, addr)
		return nil, sentinel
	}

	t.Setenv(EnvDNS, "8.8.8.8,1.1.1.1")
	if !InstallResolver() {
		t.Fatal("InstallResolver() = false with two servers")
	}
	r := net.DefaultResolver
	if !r.PreferGo {
		t.Fatal("the installed resolver must PreferGo: the cgo path is what is missing")
	}
	for i, network := range []string{"udp", "udp", "tcp"} {
		if _, err := r.Dial(context.Background(), network, "127.0.0.1:53"); !errors.Is(err, sentinel) {
			t.Fatalf("dial %d: err = %v, want the sentinel", i, err)
		}
	}
	// Round-robin by attempt: server 1, server 2, then back to server 1.
	wantAddr := []string{"8.8.8.8:53", "1.1.1.1:53", "8.8.8.8:53"}
	if !reflect.DeepEqual(gotAddr, wantAddr) {
		t.Fatalf("dialed %v, want %v", gotAddr, wantAddr)
	}
	if wantNet := []string{"udp", "udp", "tcp"}; !reflect.DeepEqual(gotNet, wantNet) {
		t.Fatalf("networks %v, want %v", gotNet, wantNet)
	}
}
