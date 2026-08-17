// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

// This file is the transport that binds the sovereign codec (protocol.go) to a
// live Java host: it dials the host's abstract unix socket, maps the shared
// framebuffer the host named, paints the go-widgets root into it and posts a
// frame per damaged rectangle. It is CGO-free — the whole Android side of the
// process is a socket and an mmap, both of which Go makes on its own — so the
// application binary stays exactly as sovereign as it is on X11 or Wayland.
package android

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/go-widgets/painter"
	"github.com/go-widgets/toolkit"
)

// EnvSocket names the environment variable the Java host sets to the abstract
// socket it is listening on. The host generates a fresh name per launch, so two
// instances of the app never collide.
const EnvSocket = "GW_ANDROID_SOCKET"

// damageRenderer is the OPT-IN incremental-present capability, declared
// structurally so this package needs no import of window. It mirrors
// window.DamageRenderer exactly, as internal/wasmbox does.
type damageRenderer interface {
	RenderDamaged(p painter.Painter, th *toolkit.Theme) []toolkit.Rect
}

// Client is an open Android host surface bound to a go-widgets scene. It
// satisfies window.Backend (Run/Close/Size/String), so a go-widgets app runs
// through Open→Run unchanged whether the backend is X11, Wayland, Cocoa, Win32,
// wasmbox or — here — a Java host Activity.
type Client struct {
	title string
	theme *toolkit.Theme

	conn net.Conn
	br   *bufio.Reader

	mu      sync.Mutex // guards the mapping and every write to conn
	buf     []byte     // RGBA framebuffer, mmap'd from the host's file
	file    *os.File
	w, h    int
	density int

	root       toolkit.Widget
	dmg        damageRenderer
	held       bool // pointer state, so a move picks drag vs move
	paused     bool
	configured chan struct{} // closed once the first Config has been mapped
	configOnce sync.Once

	done      chan struct{}
	closeOnce sync.Once
	runErr    error
}

// Compile-time shape check against the window.Backend method set.
var _ interface {
	Run(root toolkit.Widget) error
	Close() error
	Size() (int, int)
	String() string
} = (*Client)(nil)

// Dial connects to the Java host named by $GW_ANDROID_SOCKET and blocks until
// the host has announced its surface geometry, so the returned Client is ready
// to paint. It is the Android-environment analogue of openX11/openWayland.
func Dial(title string, theme *toolkit.Theme) (*Client, error) {
	name := os.Getenv(EnvSocket)
	if name == "" {
		return nil, fmt.Errorf("android: $%s is not set (not launched by the host app)", EnvSocket)
	}
	if theme == nil {
		theme = toolkit.DefaultDark()
	}
	conn, err := net.Dial("unix", "@"+name)
	if err != nil {
		return nil, fmt.Errorf("android: cannot reach the host: %w", err)
	}
	c := &Client{
		title:      title,
		theme:      theme,
		conn:       conn,
		br:         bufio.NewReader(conn),
		configured: make(chan struct{}),
		done:       make(chan struct{}),
	}
	go c.pump()
	select {
	case <-c.configured:
	case <-c.done:
		return nil, fmt.Errorf("android: host closed before announcing a surface: %w", c.runErr)
	}
	return c, nil
}

// Run binds root, paints the seed frame and blocks while host messages drive
// the widget tree, until the host closes the surface (or Close ends it). It is
// the Android analogue of the X11 and Wayland event loops.
func (c *Client) Run(root toolkit.Widget) error {
	c.mu.Lock()
	c.root = root
	c.dmg, _ = root.(damageRenderer)
	c.mu.Unlock()
	c.frame()
	<-c.done
	return c.runErr
}

// Size returns the current surface size in physical pixels.
func (c *Client) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w, c.h
}

// Density returns the display density in hundredths, as the host read it from
// Android's DisplayMetrics (a 3x panel is 300). It is this back-end's spelling
// of the backing-scale factor.
func (c *Client) Density() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.density
}

// String identifies the surface for debugging.
func (c *Client) String() string {
	w, h := c.Size()
	return fmt.Sprintf("android-host(%dx%d density=%d)", w, h, c.Density())
}

// Close ends the session: it tells the host the application is going away,
// unmaps the framebuffer and ends Run. Idempotent.
func (c *Client) Close() error {
	c.shutdown(nil, true)
	return nil
}

// Repaint asks for a repaint from ANY goroutine, satisfying window.Repainter.
func (c *Client) Repaint() { c.frame() }

// pump reads host messages until the stream ends, dispatching each one. It is
// the only reader of the connection.
func (c *Client) pump() {
	for {
		typ, body, err := ReadMessage(c.br)
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			c.shutdown(err, false)
			return
		}
		if !c.dispatch(typ, body) {
			return
		}
	}
}

// dispatch routes one decoded host message into backend action. It reports
// whether the pump should keep reading.
func (c *Client) dispatch(typ uint8, body []byte) bool {
	switch typ {
	case MsgConfig:
		cfg, err := DecodeConfig(body)
		if err != nil {
			c.shutdown(err, false)
			return false
		}
		if err := c.reconfigure(cfg); err != nil {
			c.shutdown(err, false)
			return false
		}
	case MsgTouch:
		t, err := DecodeTouch(body)
		if err != nil {
			c.shutdown(err, false)
			return false
		}
		c.deliver(MapTouch(t, c.pointerHeld(t)))
	case MsgKey:
		k, err := DecodeKey(body)
		if err != nil {
			c.shutdown(err, false)
			return false
		}
		c.deliver(MapKey(k))
	case MsgLifecycle:
		if len(body) < 1 {
			c.shutdown(ErrShortPayload, false)
			return false
		}
		c.setPaused(body[0] == LifecyclePause)
		if body[0] == LifecycleResume {
			c.frame()
		}
	case MsgClose:
		c.shutdown(nil, false)
		return false
	}
	return true
}

// pointerHeld returns the button state to map t against, then folds t into it:
// a press holds the pointer down, a release lifts it, so the NEXT move maps to
// a drag exactly as the X11 and wasmbox back-ends do.
func (c *Client) pointerHeld(t Touch) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.held
	switch t.Action {
	case TouchDown:
		c.held = true
	case TouchUp:
		c.held = false
	}
	return held
}

// deliver hands mapped events to the widget tree and repaints. A paused
// Activity has no surface to paint into, so its events still reach the tree —
// state stays live across a pause — but no frame is posted.
func (c *Client) deliver(evs []toolkit.Event) {
	c.mu.Lock()
	root := c.root
	c.mu.Unlock()
	if root == nil {
		return
	}
	for _, ev := range evs {
		root.OnEvent(ev)
	}
	c.frame()
}

// reconfigure adopts a new surface geometry: it maps (or remaps) the host's
// framebuffer file at the announced size, tells the host the mapping is live,
// and repaints. It is called for the opening Config and for every resize or
// rotation.
func (c *Client) reconfigure(cfg Config) error {
	if cfg.W <= 0 || cfg.H <= 0 {
		return fmt.Errorf("android: host announced a %dx%d surface", cfg.W, cfg.H)
	}
	c.mu.Lock()
	if err := c.remapLocked(cfg); err != nil {
		c.mu.Unlock()
		return err
	}
	w, h := c.w, c.h
	c.mu.Unlock()

	if err := c.send(MsgReady, EncodeReady(w, h)); err != nil {
		return err
	}
	c.configOnce.Do(func() { close(c.configured) })
	c.frame()
	return nil
}

// openBuffer, truncateBuffer, mapBuffer and unmapBuffer wrap the syscalls
// behind package variables so tests can force their (kernel-rare) failure paths
// and reach full branch coverage without a real fault — the convention
// go-widgets/window's internal/x11 already follows.
var (
	openBuffer = func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	}
	truncateBuffer = func(f *os.File, size int) error { return f.Truncate(int64(size)) }
	mapBuffer      = func(f *os.File, size int) ([]byte, error) {
		return syscall.Mmap(int(f.Fd()), 0, size,
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	}
	unmapBuffer = syscall.Munmap
)

// remapLocked replaces the framebuffer mapping with one of cfg's size. The
// caller holds c.mu.
func (c *Client) remapLocked(cfg Config) error {
	size := 4 * cfg.W * cfg.H
	f, err := openBuffer(cfg.BufPath)
	if err != nil {
		return fmt.Errorf("android: open framebuffer: %w", err)
	}
	if err := truncateBuffer(f, size); err != nil {
		f.Close()
		return fmt.Errorf("android: size framebuffer: %w", err)
	}
	buf, err := mapBuffer(f, size)
	if err != nil {
		f.Close()
		return fmt.Errorf("android: map framebuffer: %w", err)
	}
	c.unmapLocked()
	c.buf, c.file = buf, f
	c.w, c.h, c.density = cfg.W, cfg.H, cfg.Density
	return nil
}

// unmapLocked releases the current mapping, if any. The caller holds c.mu.
func (c *Client) unmapLocked() {
	if c.buf != nil {
		_ = unmapBuffer(c.buf)
		c.buf = nil
	}
	if c.file != nil {
		_ = c.file.Close()
		c.file = nil
	}
}

// frame repaints and posts. A plain root repaints the whole surface and posts
// one full-surface frame; an incremental (DamageRenderer) root repaints only
// the rectangles it reports and posts one frame per clamped, non-empty rect.
// A paused Activity paints nothing: its surface is gone until it resumes.
func (c *Client) frame() {
	c.mu.Lock()
	if c.paused || c.buf == nil || c.root == nil {
		c.mu.Unlock()
		return
	}
	w, h := c.w, c.h
	p := painter.NewPixelPainter(c.buf, w, h)
	full := toolkit.Rect{X: 0, Y: 0, W: w, H: h}
	var rects []toolkit.Rect
	if c.dmg != nil {
		c.root.SetBounds(full)
		rects = c.dmg.RenderDamaged(p, c.theme)
	} else {
		p.FillRect(full, c.theme.Background)
		c.root.SetBounds(full)
		c.root.Draw(p, c.theme)
		rects = []toolkit.Rect{full}
	}
	c.mu.Unlock()

	for _, r := range rects {
		cr := ClampRect(Rect{X: r.X, Y: r.Y, W: r.W, H: r.H}, w, h)
		if cr.W > 0 && cr.H > 0 {
			_ = c.send(MsgFrame, EncodeFrame(cr))
		}
	}
}

// setPaused records an Activity transition.
func (c *Client) setPaused(p bool) {
	c.mu.Lock()
	c.paused = p
	c.mu.Unlock()
}

// send writes one framed message to the host under the connection lock, so
// frames posted from a repainting goroutine never interleave with a title
// update.
func (c *Client) send(typ uint8, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return WriteMessage(c.conn, typ, body)
}

// SetTitle updates the host's window title.
func (c *Client) SetTitle(title string) {
	c.mu.Lock()
	c.title = title
	c.mu.Unlock()
	_ = c.send(MsgTitle, []byte(title))
}

// shutdown ends the session once, recording the error that ended it. sendBye
// posts MsgBye first (the application-initiated path); a host-initiated end
// passes false, the host already knows.
func (c *Client) shutdown(err error, sendBye bool) {
	c.closeOnce.Do(func() {
		c.runErr = err
		if sendBye {
			_ = c.send(MsgBye, nil)
		}
		c.mu.Lock()
		c.unmapLocked()
		c.mu.Unlock()
		_ = c.conn.Close()
		close(c.done)
	})
}
