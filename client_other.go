// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import "github.com/go-widgets/toolkit"

// Client is the unavailable-here shape of the Android host surface. The host
// protocol needs a Linux abstract socket and a shared mapping, so there is
// nothing to connect to anywhere else — but the type and its method set exist
// on every GOOS so an application using this back-end still cross-builds and
// still vets, exactly as go-widgets/window's open_other.go keeps the native
// back-ends' entry point compiling everywhere.
type Client struct{}

// Dial reports that this environment has no Android host to talk to.
func Dial(string, *toolkit.Theme) (*Client, error) { return nil, ErrUnsupported }

// Run never runs: no surface was ever granted.
func (c *Client) Run(toolkit.Widget) error { return ErrUnsupported }

// Close is a no-op on a surface that was never opened.
func (c *Client) Close() error { return nil }

// Size reports an empty surface.
func (c *Client) Size() (int, int) { return 0, 0 }

// Density reports no display.
func (c *Client) Density() int { return 0 }

// Insets reports nothing covering a surface that does not exist.
func (c *Client) Insets() Insets { return Insets{} }

// SetFullBleed has no surface to bleed over.
func (c *Client) SetFullBleed(bool) {}

// SetSoftKeyboard has no host to ask for a keyboard.
func (c *Client) SetSoftKeyboard(bool) {}

// Repaint has nothing to repaint.
func (c *Client) Repaint() {}

// SetTitle has no host to retitle.
func (c *Client) SetTitle(string) {}

// String identifies the absent surface.
func (c *Client) String() string { return "android-host(unavailable)" }
