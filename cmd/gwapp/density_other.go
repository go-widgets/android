// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !android

package main

// applyDensity is a no-op off Android: the X11, Wayland, Cocoa and Win32
// back-ends keep the compact DensityCompact default (the toolkit's zero value),
// so the desktop scene stays byte-for-byte what it was. The touch profile is
// selected only in the android-tagged density_android.go.
func applyDensity() {}
