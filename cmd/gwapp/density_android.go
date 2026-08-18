// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build android

package main

import "github.com/go-widgets/toolkit"

// applyDensity selects the toolkit's touch profile on Android, where the whole
// UI is a fingertip target: every widget sizes for the finger (>=44px hit
// targets, comfortable metrics). It runs once, before any widget is built, so
// the choice is global for the process. The desktop back-ends compile the
// no-op in density_other.go instead and keep the compact DensityCompact default
// (the toolkit's zero value).
func applyDensity() { toolkit.SetDensity(toolkit.DensityTouch) }
