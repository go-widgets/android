// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import "github.com/go-aosp/hostenv"

// # WHY THIS IS NOW A DELEGATION
//
// The mechanism below was written here and proven here, on a real installable
// APK. It is not widget code: nothing in it knows about a surface, a socket or
// a widget tree, and its only imports were from the standard library.
//
// That made it unreachable for its next consumers. This package is a widget
// back-end carrying a socket protocol, a client, an a11y bridge and twelve
// indirect dependencies; a package manager or a daemon cannot take all of that
// to get a resolver, so the next one writes a second copy -- and one was in
// fact drafted in go-zpkg/artifact before it was deleted (go-zpkg/artifact#235).
//
// So it moved to github.com/go-aosp/hostenv, which imports nothing but the
// standard library, and this file keeps the names that were already published.
//
// ⛔ THE EXPORTED NAMES STAY, AND THEY ARE NOT DEPRECATED. EnvDNS is set by
// this repository's own Java host (host/java/.../GwHostActivity.java) and by
// anything built on it, and InstallResolver is called from client.go. Deleting
// either would break a contract that is already in the field, for no gain: a
// one-line delegation costs nothing and the move stays invisible to every
// caller.

// EnvDNS names the environment variable the Java host sets to the system's DNS
// servers, comma separated, when it spawns the application.
//
// Android has no /etc/resolv.conf, and its resolver lives behind libc, which is
// behind cgo -- the one thing a CGO-free application does not have. Go's pure
// resolver therefore finds no nameserver, falls back to localhost, and every
// lookup fails with a connection refused. The host can see the real servers
// through ConnectivityManager, so it passes them here.
const EnvDNS = hostenv.EnvDNS

// InstallResolver points net.DefaultResolver at the DNS servers named by
// $GW_ANDROID_DNS, and reports whether it did.
//
// It returns false, changing nothing, when the variable is unset or lists no
// usable server: off Android, or in an APK whose application never asked for
// ACCESS_NETWORK_STATE, the default resolver is either already correct or
// deliberately absent, and silently rewiring it would be worse than leaving it.
//
// Since the move it also accepts hostenv.EnvDNSAlias, so a host that prefers a
// vendor-neutral name works without a change here.
func InstallResolver() bool { return hostenv.InstallResolver() }
