// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-widgets/toolkit"
)

// testDeadline bounds every wait in this file. Each wait is on a deadline, not
// on a spin count, so the test neither flakes on a slow machine nor hides a
// hang behind a lucky iteration count.
const testDeadline = 10 * time.Second

// fakeHost is an in-process stand-in for the Java Activity: it owns the socket
// and the shared file exactly as the real host does, so the client is exercised
// over a REAL unix socket and a REAL mapping rather than a mock.
type fakeHost struct {
	t    *testing.T
	ln   net.Listener
	conn net.Conn
	br   *bufio.Reader
	path string
	w, h int
}

// newFakeHost binds the abstract socket the client will dial and points
// $GW_ANDROID_SOCKET at it.
func newFakeHost(t *testing.T, w, h int) *fakeHost {
	t.Helper()
	name := fmt.Sprintf("gw-androidhost-test-%d-%s", os.Getpid(), t.Name())
	ln, err := net.Listen("unix", "@"+name)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv(EnvSocket, name)
	return &fakeHost{t: t, ln: ln, path: filepath.Join(t.TempDir(), "surface.rgba"), w: w, h: h}
}

// serve accepts the client and announces the surface, mirroring what the Java
// host does the moment its SurfaceView has a size.
func (h *fakeHost) serve() {
	go func() {
		conn, err := h.ln.Accept()
		if err != nil {
			return
		}
		h.conn = conn
		h.br = bufio.NewReader(conn)
		_ = WriteMessage(conn, MsgConfig, EncodeConfig(Config{
			W: h.w, H: h.h, Density: 200, BufPath: h.path,
		}))
	}()
}

// next reads one application message, failing the test on timeout so a stalled
// client is reported as a stall rather than as a hang.
func (h *fakeHost) next() (uint8, []byte) {
	h.t.Helper()
	type msg struct {
		typ  uint8
		body []byte
		err  error
	}
	ch := make(chan msg, 1)
	go func() {
		typ, body, err := ReadMessage(h.br)
		ch <- msg{typ, body, err}
	}()
	select {
	case m := <-ch:
		if m.err != nil {
			h.t.Fatalf("reading from the application: %v", m.err)
		}
		return m.typ, m.body
	case <-time.After(testDeadline):
		h.t.Fatal("timed out waiting for an application message")
		return 0, nil
	}
}

func TestClientRoundTrip(t *testing.T) {
	const w, h = 200, 120
	host := newFakeHost(t, w, h)
	host.serve()

	c, err := Dial("test", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// The application answers the geometry announcement by mapping the buffer
	// and saying so.
	typ, body := host.next()
	if typ != MsgReady {
		t.Fatalf("first message = %#x, want MsgReady", typ)
	}
	if gw, gh, err := DecodeReady(body); err != nil || gw != w || gh != h {
		t.Fatalf("ready = %dx%d err=%v, want %dx%d", gw, gh, err, w, h)
	}
	if gw, gh := c.Size(); gw != w || gh != h {
		t.Fatalf("Size = %dx%d, want %dx%d", gw, gh, w, h)
	}
	if c.Density() != 200 {
		t.Fatalf("Density = %d, want 200", c.Density())
	}

	clicked := make(chan struct{}, 1)
	box := toolkit.NewVBox()
	box.Append(toolkit.NewButton("Click me", func() {
		select {
		case clicked <- struct{}{}:
		default:
		}
	}))
	go func() {
		if err := c.Run(box); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	// The seed frame covers the whole surface.
	if typ, body := host.next(); typ != MsgFrame {
		t.Fatalf("seed message = %#x, want MsgFrame", typ)
	} else if r, err := DecodeFrame(body); err != nil || r != (Rect{W: w, H: h}) {
		t.Fatalf("seed damage = %+v err=%v, want the full surface", r, err)
	}

	// The surface really was painted: the theme's background reached the
	// shared file, so the host would blit pixels rather than zeroes.
	pixels, err := os.ReadFile(host.path)
	if err != nil {
		t.Fatalf("reading the shared surface: %v", err)
	}
	if len(pixels) != 4*w*h {
		t.Fatalf("shared surface is %d bytes, want %d", len(pixels), 4*w*h)
	}
	if allZero(pixels) {
		t.Fatal("the shared surface is still blank after the seed frame")
	}

	// A press on the button reaches the widget tree and provokes a frame.
	if err := WriteMessage(host.conn, MsgTouch, EncodeTouch(Touch{
		Action: TouchDown, X: w / 2, Y: h / 2,
	})); err != nil {
		t.Fatalf("sending a touch: %v", err)
	}
	select {
	case <-clicked:
	case <-time.After(testDeadline):
		t.Fatal("the button never fired: the touch did not reach the widget tree")
	}
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("post-touch message = %#x, want MsgFrame", typ)
	}
}

func TestClientResize(t *testing.T) {
	host := newFakeHost(t, 100, 80)
	host.serve()
	c, err := Dial("test", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if typ, _ := host.next(); typ != MsgReady {
		t.Fatalf("first message = %#x, want MsgReady", typ)
	}
	go func() { _ = c.Run(toolkit.NewVBox()) }()
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("seed message = %#x, want MsgFrame", typ)
	}

	// A rotation is just a second Config: the surface is remapped at the new
	// size and the tree is laid out again, in the same process.
	if err := WriteMessage(host.conn, MsgConfig, EncodeConfig(Config{
		W: 80, H: 100, Density: 200, BufPath: host.path,
	})); err != nil {
		t.Fatalf("sending the new geometry: %v", err)
	}
	typ, body := host.next()
	if typ != MsgReady {
		t.Fatalf("resize message = %#x, want MsgReady", typ)
	}
	if gw, gh, err := DecodeReady(body); err != nil || gw != 80 || gh != 100 {
		t.Fatalf("ready after resize = %dx%d err=%v, want 80x100", gw, gh, err)
	}
	if gw, gh := c.Size(); gw != 80 || gh != 100 {
		t.Fatalf("Size after resize = %dx%d, want 80x100", gw, gh)
	}
	if typ, body := host.next(); typ != MsgFrame {
		t.Fatalf("post-resize message = %#x, want MsgFrame", typ)
	} else if r, _ := DecodeFrame(body); r != (Rect{W: 80, H: 100}) {
		t.Fatalf("post-resize damage = %+v, want the whole new surface", r)
	}
}

func TestClientPauseStopsPainting(t *testing.T) {
	host := newFakeHost(t, 64, 64)
	host.serve()
	c, err := Dial("test", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	host.next() // ready
	go func() { _ = c.Run(toolkit.NewVBox()) }()
	host.next() // seed frame

	// A paused Activity has no surface: events still reach the tree, so state
	// stays live, but nothing is posted until it resumes.
	if err := WriteMessage(host.conn, MsgLifecycle, []byte{LifecyclePause}); err != nil {
		t.Fatalf("pausing: %v", err)
	}
	if err := WriteMessage(host.conn, MsgTouch, EncodeTouch(Touch{Action: TouchDown, X: 1, Y: 1})); err != nil {
		t.Fatalf("touching while paused: %v", err)
	}
	if err := WriteMessage(host.conn, MsgLifecycle, []byte{LifecycleResume}); err != nil {
		t.Fatalf("resuming: %v", err)
	}
	// Exactly one frame arrives — the resume's. Had the paused touch painted,
	// this would be the touch's frame and the next read would block out.
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("post-resume message = %#x, want MsgFrame", typ)
	}
	if err := WriteMessage(host.conn, MsgClose, nil); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if typ, _, err := ReadMessage(host.br); err == nil {
		t.Fatalf("expected the stream to end, got message %#x", typ)
	}
}

func TestDialErrors(t *testing.T) {
	t.Setenv(EnvSocket, "")
	if _, err := Dial("test", nil); err == nil {
		t.Fatal("Dial with no socket in the environment should error")
	}
	t.Setenv(EnvSocket, "gw-androidhost-nobody-listening")
	if _, err := Dial("test", nil); err == nil {
		t.Fatal("Dial with nothing listening should error")
	}
}

func TestClientRejectsBadGeometry(t *testing.T) {
	host := newFakeHost(t, 0, 0)
	host.serve()
	if _, err := Dial("test", nil); err == nil {
		t.Fatal("Dial should fail when the host announces an empty surface")
	}
}

// allZero reports whether every byte is zero, i.e. nothing was painted.
func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
