// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import (
	"errors"
	"testing"
)

// TestUnsupportedSurface pins the off-Linux shape: every entry point answers,
// none of them pretends to have a surface, and the two that can fail say why.
func TestUnsupportedSurface(t *testing.T) {
	if _, err := Dial("test", nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Dial error = %v, want ErrUnsupported", err)
	}
	var c Client
	if err := c.Run(nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Run error = %v, want ErrUnsupported", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close error = %v, want nil", err)
	}
	if w, h := c.Size(); w != 0 || h != 0 {
		t.Fatalf("Size = %dx%d, want 0x0", w, h)
	}
	if c.Density() != 0 {
		t.Fatalf("Density = %d, want 0", c.Density())
	}
	if c.String() == "" {
		t.Fatal("String should still name the absent surface")
	}
	c.Repaint()
	c.SetTitle("ignored")
}
