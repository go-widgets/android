// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

// The branch tests: the paths a healthy host never takes — a malformed
// message, a framebuffer the kernel refuses, a damage-reporting root — plus the
// small surface API a real application calls but the round-trip test does not.
package android

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-widgets/painter"
	"github.com/go-widgets/toolkit"
)

// dialConfigured brings up a fake host and a configured client, returning both.
func dialConfigured(t *testing.T, w, h int) (*fakeHost, *Client) {
	t.Helper()
	host := newFakeHost(t, w, h)
	host.serve()
	c, err := Dial("test", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if typ, _ := host.next(); typ != MsgReady {
		t.Fatalf("first message = %#x, want MsgReady", typ)
	}
	return host, c
}

// waitDone waits for the client's Run loop to end, on a deadline.
func waitDone(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(testDeadline):
		t.Fatal("the client never shut down")
	}
}

func TestClientSurfaceAPI(t *testing.T) {
	host, c := dialConfigured(t, 320, 240)
	if got := c.String(); !strings.Contains(got, "320x240") || !strings.Contains(got, "density=200") {
		t.Fatalf("String = %q, want it to name the surface and density", got)
	}
	go func() { _ = c.Run(toolkit.NewVBox()) }()
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("seed message = %#x, want MsgFrame", typ)
	}
	// Repaint is window.Repainter: a frame on demand, from any goroutine.
	c.Repaint()
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("Repaint message = %#x, want MsgFrame", typ)
	}
	c.SetTitle("renamed")
	typ, body := host.next()
	if typ != MsgTitle || string(body) != "renamed" {
		t.Fatalf("SetTitle sent %#x %q, want MsgTitle \"renamed\"", typ, body)
	}
}

func TestClientKeyReachesTree(t *testing.T) {
	host, c := dialConfigured(t, 200, 100)
	keys := make(chan string, 4)
	go func() { _ = c.Run(&recordingRoot{keys: keys}) }()
	host.next() // seed frame

	if err := WriteMessage(host.conn, MsgKey, EncodeKey(Key{
		Action: KeyDown, Code: 66, // KEYCODE_ENTER
	})); err != nil {
		t.Fatalf("sending a key: %v", err)
	}
	select {
	case got := <-keys:
		if got != "Enter" {
			t.Fatalf("the tree saw %q, want \"Enter\"", got)
		}
	case <-time.After(testDeadline):
		t.Fatal("the key never reached the widget tree")
	}
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("post-key message = %#x, want MsgFrame", typ)
	}
}

func TestClientTouchDragSequence(t *testing.T) {
	host, c := dialConfigured(t, 200, 100)
	events := make(chan toolkit.Event, 8)
	go func() { _ = c.Run(&recordingRoot{events: events}) }()
	host.next() // seed frame

	// down → move → up → move: the middle move is a DRAG because the finger is
	// down, the last one is a plain move because it was lifted.
	for _, tch := range []Touch{
		{Action: TouchDown, X: 10, Y: 10},
		{Action: TouchMove, X: 20, Y: 20},
		{Action: TouchUp, X: 20, Y: 20},
		{Action: TouchMove, X: 30, Y: 30},
	} {
		if err := WriteMessage(host.conn, MsgTouch, EncodeTouch(tch)); err != nil {
			t.Fatalf("sending a touch: %v", err)
		}
	}
	want := []toolkit.EventKind{
		toolkit.EventClick, toolkit.EventMouseDrag,
		toolkit.EventMouseUp, toolkit.EventMouseMove,
	}
	for i, w := range want {
		select {
		case got := <-events:
			if got.Kind != w {
				t.Fatalf("event %d = %v, want %v", i, got.Kind, w)
			}
		case <-time.After(testDeadline):
			t.Fatalf("event %d never arrived", i)
		}
	}
}

func TestClientTouchBeforeRun(t *testing.T) {
	// A touch that lands between the surface being configured and Run binding
	// a tree has nowhere to go, and must not panic.
	host, _ := dialConfigured(t, 100, 100)
	if err := WriteMessage(host.conn, MsgTouch, EncodeTouch(Touch{Action: TouchDown})); err != nil {
		t.Fatalf("sending a touch: %v", err)
	}
	// Prove the client is still alive by making it answer a resize.
	if err := WriteMessage(host.conn, MsgConfig, EncodeConfig(Config{
		W: 120, H: 90, BufPath: host.path,
	})); err != nil {
		t.Fatalf("resizing: %v", err)
	}
	if typ, _ := host.next(); typ != MsgReady {
		t.Fatalf("message after an early touch = %#x, want MsgReady", typ)
	}
}

func TestClientMalformedMessagesEndTheSession(t *testing.T) {
	cases := []struct {
		name string
		typ  uint8
		body []byte
	}{
		{"config", MsgConfig, []byte{1, 2, 3}},
		{"touch", MsgTouch, []byte{1}},
		{"key", MsgKey, []byte{1}},
		{"lifecycle", MsgLifecycle, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, cl := dialConfigured(t, 100, 100)
			if err := WriteMessage(host.conn, c.typ, c.body); err != nil {
				t.Fatalf("sending: %v", err)
			}
			waitDone(t, cl)
			if cl.runErr == nil {
				t.Fatal("a malformed message should end the session with an error")
			}
		})
	}
}

func TestClientUnknownMessageIsIgnored(t *testing.T) {
	// A newer host may send a message this application does not know; it must
	// be skipped, not fatal, so an older app keeps running under a newer host.
	host, _ := dialConfigured(t, 100, 100)
	if err := WriteMessage(host.conn, 0x7f, []byte{1, 2, 3}); err != nil {
		t.Fatalf("sending: %v", err)
	}
	if err := WriteMessage(host.conn, MsgConfig, EncodeConfig(Config{
		W: 110, H: 90, BufPath: host.path,
	})); err != nil {
		t.Fatalf("resizing: %v", err)
	}
	if typ, _ := host.next(); typ != MsgReady {
		t.Fatalf("message after an unknown one = %#x, want MsgReady", typ)
	}
}

func TestClientFramebufferFailures(t *testing.T) {
	failure := errors.New("forced")
	cases := []struct {
		name    string
		install func(t *testing.T)
	}{
		{"open", func(t *testing.T) {
			swap(t, &openBuffer, func(string) (*os.File, error) { return nil, failure })
		}},
		{"truncate", func(t *testing.T) {
			swap(t, &truncateBuffer, func(*os.File, int) error { return failure })
		}},
		{"mmap", func(t *testing.T) {
			swap(t, &mapBuffer, func(*os.File, int) ([]byte, error) { return nil, failure })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.install(t)
			host := newFakeHost(t, 64, 64)
			host.serve()
			if _, err := Dial("test", nil); err == nil {
				t.Fatal("Dial should fail when the framebuffer cannot be mapped")
			}
		})
	}
}

func TestClientDamageRootPostsOnlyItsRectangles(t *testing.T) {
	host, c := dialConfigured(t, 100, 100)
	root := &damageRoot{rects: []toolkit.Rect{
		{X: 10, Y: 10, W: 20, H: 20},
		{X: 90, Y: 90, W: 40, H: 40}, // overhangs: clamped to the surface
		{X: 200, Y: 0, W: 5, H: 5},   // wholly outside: dropped
	}}
	go func() { _ = c.Run(root) }()

	want := []Rect{{X: 10, Y: 10, W: 20, H: 20}, {X: 90, Y: 90, W: 10, H: 10}}
	for i, w := range want {
		typ, body := host.next()
		if typ != MsgFrame {
			t.Fatalf("frame %d = %#x, want MsgFrame", i, typ)
		}
		if got, err := DecodeFrame(body); err != nil || got != w {
			t.Fatalf("frame %d damage = %+v err=%v, want %+v", i, got, err, w)
		}
	}
}

// swap replaces a package variable for the duration of a test.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// recordingRoot is a leaf widget that records what the back-end delivers.
type recordingRoot struct {
	toolkit.Rect
	events chan toolkit.Event
	keys   chan string
}

func (r *recordingRoot) Bounds() toolkit.Rect                 { return r.Rect }
func (r *recordingRoot) SetBounds(b toolkit.Rect)             { r.Rect = b }
func (r *recordingRoot) Draw(painter.Painter, *toolkit.Theme) {}
func (r *recordingRoot) HitTest(px, py int) bool              { return true }

func (r *recordingRoot) OnEvent(e toolkit.Event) {
	if r.events != nil {
		select {
		case r.events <- e:
		default:
		}
	}
	if r.keys != nil && e.Kind == toolkit.EventKeyDown {
		select {
		case r.keys <- e.Code:
		default:
		}
	}
}

// damageRoot is a root that reports its own damage, exercising the incremental
// present path (window.DamageRenderer).
type damageRoot struct {
	recordingRoot
	rects []toolkit.Rect
}

func (d *damageRoot) RenderDamaged(painter.Painter, *toolkit.Theme) []toolkit.Rect {
	return d.rects
}

func TestClientHostVanishing(t *testing.T) {
	// A host that simply closes the socket — the Activity was destroyed — ends
	// the session cleanly: Run returns nil, not an error, because nothing went
	// wrong, the window is just gone.
	host, c := dialConfigured(t, 64, 64)
	if err := host.conn.Close(); err != nil {
		t.Fatalf("closing the host end: %v", err)
	}
	waitDone(t, c)
	if c.runErr != nil {
		t.Fatalf("Run error = %v, want nil for a host that closed cleanly", c.runErr)
	}
}

func TestReconfigureReportsASendFailure(t *testing.T) {
	// The mapping succeeded but the host went away before it could be told:
	// reconfigure reports the write failure rather than pretending the host
	// knows about a surface it will never hear of.
	a, b := net.Pipe()
	_ = b.Close()
	_ = a.Close()
	c := &Client{
		conn:       a,
		theme:      toolkit.DefaultDark(),
		configured: make(chan struct{}),
		done:       make(chan struct{}),
	}
	err := c.reconfigure(Config{W: 4, H: 4, BufPath: filepath.Join(t.TempDir(), "surface.rgba")})
	if err == nil {
		t.Fatal("reconfigure should report a failed handshake write")
	}
}
