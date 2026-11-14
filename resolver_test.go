// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"net"
	"testing"

	"github.com/go-aosp/hostenv"
)

// ⛔ THE PUBLISHED NAME, AS A LITERAL. EnvDNS is now an alias for a constant in
// another module, so a rename over there would silently change what this
// repository's Java host must set -- and the host sets it from its own Java
// literal (host/java/org/gowidgets/androidhost/GwHostActivity.java), which no
// Go change can reach. Writing the string out here is what makes an upstream
// rename fail LOUDLY, on this side, instead of producing an APK whose payload
// never resolves a name.
//
// If this test ever goes red, the fix is not to update the literal: it is to
// decide whether the Java host, every other host, and every payload in the
// field are being renamed too.
func TestTheDNSVariableKeepsItsPublishedName(t *testing.T) {
	const published = "GW_ANDROID_DNS"
	if EnvDNS != published {
		t.Fatalf("EnvDNS = %q, want %q — the Java host sets the literal, so this is a contract, not a spelling", EnvDNS, published)
	}
	if hostenv.EnvDNS != published {
		t.Errorf("hostenv.EnvDNS = %q, want %q — the shared module renamed a published identity", hostenv.EnvDNS, published)
	}
}

// InstallResolver is a delegation now, so what is left to test here is that it
// IS delegated: the two outcomes a caller depends on still happen, through
// whatever the shared module does internally.
func TestInstallResolverStillDeclinesAndStillInstalls(t *testing.T) {
	before := net.DefaultResolver
	t.Cleanup(func() { net.DefaultResolver = before })

	// Both generations unset: the default resolver is left exactly as it was.
	t.Setenv(EnvDNS, "")
	t.Setenv(hostenv.EnvDNSAlias, "")
	if InstallResolver() {
		t.Error("InstallResolver() = true with no servers named")
	}
	if net.DefaultResolver != before {
		t.Error("the default resolver was replaced although no servers were named")
	}

	t.Setenv(EnvDNS, "8.8.8.8,1.1.1.1")
	if !InstallResolver() {
		t.Fatal("InstallResolver() = false although the host named two servers")
	}
	if net.DefaultResolver == before {
		t.Error("the default resolver was not replaced although servers were named")
	}
	if !net.DefaultResolver.PreferGo {
		t.Error("the installed resolver must PreferGo: the cgo path is what is missing on Android")
	}
}

// And the alias works from here without this repository changing, which is the
// one behaviour the move ADDED rather than preserved.
func TestTheNeutralAliasIsAcceptedToo(t *testing.T) {
	before := net.DefaultResolver
	t.Cleanup(func() { net.DefaultResolver = before })

	t.Setenv(EnvDNS, "")
	t.Setenv(hostenv.EnvDNSAlias, "9.9.9.9")
	if !InstallResolver() {
		t.Fatal("InstallResolver() = false although the neutral alias named a server")
	}
}
