// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

// This file is the transport that binds the sovereign codec (protocol.go) to a
// live Java host: it dials the host's abstract unix socket, creates and maps
// the shared framebuffer, hands its descriptor over, paints the go-widgets root
// into it and posts a frame per damaged rectangle. It is CGO-free — the whole Android side of the
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
	"golang.org/x/sys/unix"
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

	mu       sync.Mutex // guards the mapping and every write to conn
	buf      []byte     // RGBA framebuffer, mmap'd from the memfd shared with the host
	file     *os.File
	w, h     int
	density  int
	insets   Insets
	shareFD  int           // framebuffer descriptor to hand the host, or -1
	a11y     []A11yElement // last tree served, so an action can name an element
	fullBled bool          // lay the root out edge to edge, ignoring the insets

	root       toolkit.Widget
	dmg        damageRenderer
	down       map[int]bool // contacts currently down, by pointer id
	primary    int          // the contact that also drives the mouse events
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
		shareFD:    -1,
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

// Insets returns the margin of the surface the system is drawing over: the
// status and navigation bars, a display cutout, the soft keyboard. By default
// the widget tree is laid out inside what they leave; see [Client.SetFullBleed].
func (c *Client) Insets() Insets {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.insets
}

// SetFullBleed lays the widget tree out over the WHOLE surface, insets and
// all. It is for a root that means to reach under the system bars — a photo, a
// map, a video — and is then responsible for keeping anything readable out of
// the area [Client.Insets] reports.
func (c *Client) SetFullBleed(on bool) {
	c.mu.Lock()
	changed := c.fullBled != on
	c.fullBled = on
	c.mu.Unlock()
	if changed {
		c.frame()
	}
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
		held, primary := c.pointerState(t)
		c.deliver(MapTouch(t, held, primary))
	case MsgKey:
		k, err := DecodeKey(body)
		if err != nil {
			c.shutdown(err, false)
			return false
		}
		c.deliver(MapKey(k))
	case MsgInsets:
		ins, err := DecodeInsets(body)
		if err != nil {
			c.shutdown(err, false)
			return false
		}
		c.mu.Lock()
		changed := c.insets != ins
		c.insets = ins
		c.mu.Unlock()
		if changed {
			c.frame()
		}
	case MsgText:
		c.deliver(MapText(string(body)))
	case MsgTextDelete:
		n, err := DecodeTextDelete(body)
		if err != nil {
			c.shutdown(err, false)
			return false
		}
		c.deliver(MapTextDelete(n))
	case MsgA11yRequest:
		c.sendA11yTree()
	case MsgA11yAction:
		i, err := DecodeA11yAction(body)
		if err != nil {
			c.shutdown(err, false)
			return false
		}
		c.activateA11y(i)
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

// pointerState folds one sample into the client's contact bookkeeping and
// reports how to map it: whether a pointer was already down before this sample
// (so a move becomes a drag), and whether this contact is the PRIMARY one.
//
// The primary contact is the first finger down while no other is. Only it gets
// compatibility mouse events, because a second finger must not fire a second
// EventClick: a two-finger pinch would otherwise read as two taps to every
// widget in the tree. A browser draws the same line for the same reason.
func (c *Client) pointerState(t Touch) (held, primary bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held = len(c.down) > 0
	switch t.Action {
	case TouchDown:
		if len(c.down) == 0 {
			c.primary = t.ID
		}
		if c.down == nil {
			c.down = map[int]bool{}
		}
		c.down[t.ID] = true
	case TouchUp:
		delete(c.down, t.ID)
	}
	return held, t.ID == c.primary
}

// deliver hands mapped events to the widget tree and repaints. A paused
// Activity has no surface to paint into, so its events still reach the tree —
// state stays live across a pause — but no frame is posted.
//
// The coordinates are translated into the ROOT's own space first. The host
// reports a touch in surface pixels, but a toolkit container treats an incoming
// event as parent-local and adds its own origin to reach absolute coordinates —
// so a root laid out at the safe-area origin must be handed events relative to
// that origin, or every touch lands one inset too far.
//
// That was live from the moment insets arrived and invisible until something
// small was tapped: the demo's buttons are 426 pixels tall, so a 128-pixel error
// still landed inside the intended widget. A 20-pixel button is what showed it.
func (c *Client) deliver(evs []toolkit.Event) {
	c.mu.Lock()
	root := c.root
	origin := c.layoutOriginLocked()
	c.mu.Unlock()
	if root == nil {
		return
	}
	for _, ev := range evs {
		if hasPosition(ev.Kind) {
			ev.X -= origin.X
			ev.Y -= origin.Y
		}
		root.OnEvent(ev)
	}
	c.frame()
}

// layoutOriginLocked is where the root is laid out: the safe-area origin, or
// (0,0) for a full-bleed root. The caller holds c.mu.
func (c *Client) layoutOriginLocked() Rect {
	if c.fullBled {
		return Rect{}
	}
	a := c.insets.Apply(c.w, c.h)
	return Rect{X: a.X, Y: a.Y}
}

// hasPosition reports whether an event kind carries a meaningful X/Y. A key
// event does not, and shifting its zero coordinates would be noise.
func hasPosition(k toolkit.EventKind) bool {
	switch k {
	case toolkit.EventClick, toolkit.EventMouseUp, toolkit.EventMouseMove,
		toolkit.EventMouseDrag, toolkit.EventScroll,
		toolkit.EventTouchStart, toolkit.EventTouchMove, toolkit.EventTouchEnd:
		return true
	}
	return false
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
	w, h, fd := c.w, c.h, c.shareFD
	c.mu.Unlock()

	// The framebuffer descriptor rides WITH the message that announces it, as
	// SCM_RIGHTS ancillary data: the host reads the descriptors that arrived
	// with the bytes it just read, so attaching them to any other message
	// would leave it guessing which mapping they belong to.
	if err := c.sendFD(MsgReady, EncodeReady(w, h), fd); err != nil {
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
	memfdCreate = unix.MemfdCreate
	openMemfd   = func() (*os.File, error) {
		fd, err := memfdCreate("gw-surface", unix.MFD_CLOEXEC)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), "gw-surface"), nil
	}
	truncateBuffer = func(f *os.File, size int) error { return f.Truncate(int64(size)) }
	mapBuffer      = func(f *os.File, size int) ([]byte, error) {
		return syscall.Mmap(int(f.Fd()), 0, size,
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	}
	unmapBuffer = syscall.Munmap
)

// openSurfaceLocked opens the framebuffer to paint into: a memfd when the
// kernel has memfd_create, whose descriptor is then handed to the host over the
// socket, and otherwise the file the host named in cfg. It reports whether the
// descriptor has to be shared: a file needs no sharing, the host already knows
// its path. The caller holds c.mu.
func (c *Client) openSurfaceLocked(cfg Config, size int) (*os.File, bool, error) {
	if f, err := openMemfd(); err == nil {
		return f, true, nil
	} else if cfg.BufPath == "" {
		// No memfd and no fallback path: there is nowhere to paint.
		return nil, false, fmt.Errorf("android: no framebuffer: memfd_create: %w", err)
	}
	f, err := openBuffer(cfg.BufPath)
	if err != nil {
		return nil, false, fmt.Errorf("android: open framebuffer: %w", err)
	}
	return f, false, nil
}

// remapLocked replaces the framebuffer mapping with one of cfg's size. The
// caller holds c.mu.
//
// The framebuffer is a memfd: an anonymous, memory-backed descriptor the host
// maps by receiving it over the socket. A file in the app's storage would work
// too — and is the fallback when a kernel has no memfd_create — but its pages
// are file-backed, so every frame dirties page cache the kernel then writes to
// flash. Measured on a 1080x2400 surface: ~10.4 MB of Dirty against 128 kB at
// rest, flushed to storage about half a minute later, for pixels that are pure
// scratch.
func (c *Client) remapLocked(cfg Config) error {
	size := 4 * cfg.W * cfg.H
	f, shared, err := c.openSurfaceLocked(cfg, size)
	if err != nil {
		return err
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
	c.shareFD = -1
	if shared {
		c.shareFD = int(f.Fd())
	}
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
	// The tree is laid out inside the area the system is NOT drawing over, so
	// its first and last rows are not hidden behind the status and navigation
	// bars of an edge-to-edge window. The margins are still painted — in the
	// theme background, so the bars sit on the app's own colour rather than on
	// whatever the previous frame left there.
	area := full
	if !c.fullBled {
		a := c.insets.Apply(w, h)
		area = toolkit.Rect{X: a.X, Y: a.Y, W: a.W, H: a.H}
	}
	var rects []toolkit.Rect
	if c.dmg != nil {
		c.root.SetBounds(area)
		rects = c.dmg.RenderDamaged(p, c.theme)
	} else {
		p.FillRect(full, c.theme.Background)
		c.root.SetBounds(area)
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

// sendA11yTree walks the widget tree and answers the host's request. The walk
// happens HERE, on demand, and nowhere near the paint loop: a screen reader
// asking for the tree is rare, a frame is not.
func (c *Client) sendA11yTree() {
	c.mu.Lock()
	els := A11yElements(c.root)
	c.a11y = els
	c.mu.Unlock()
	_ = c.send(MsgA11yTree, EncodeA11yTree(els))
}

// activateA11y replays a screen reader's activation as an ordinary click at the
// centre of the element it names, so an accessibility action goes through the
// very code an ordinary touch does. An index that no longer matches the tree —
// the host is working from a snapshot — is ignored rather than guessed at.
func (c *Client) activateA11y(i int) {
	c.mu.Lock()
	var el A11yElement
	ok := i >= 0 && i < len(c.a11y)
	if ok {
		el = c.a11y[i]
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	x, y := el.Center()
	c.deliver([]toolkit.Event{{Kind: toolkit.EventClick, X: x, Y: y}})
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

// sendFD writes one framed message with fd attached as SCM_RIGHTS ancillary
// data, so the host receives the descriptor with the very bytes that announce
// it. A negative fd, or a transport that cannot carry descriptors (an
// in-process test pipe), falls back to a plain write: the host then maps the
// path it named itself.
func (c *Client) sendFD(typ uint8, body []byte, fd int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	uc, ok := c.conn.(*net.UnixConn)
	if fd < 0 || !ok {
		return WriteMessage(c.conn, typ, body)
	}
	// One sendmsg: the framing and the descriptor must not be split across
	// writes, or the host would attribute the descriptor to another message.
	_, _, err := uc.WriteMsgUnix(FrameMessage(typ, body), unix.UnixRights(fd), nil)
	return err
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

// SetSoftKeyboard asks the host to show or hide the on-screen keyboard.
//
// Only the host can: the keyboard is a window, and an application that owns no
// windows cannot raise one. Showing it also changes the insets — the keyboard
// covers the bottom of the surface — so the tree is laid out above it without
// this back-end doing anything special, because an IME inset is an inset like
// any other (see [Client.Insets]).
func (c *Client) SetSoftKeyboard(show bool) {
	b := byte(0)
	if show {
		b = 1
	}
	_ = c.send(MsgKeyboard, []byte{b})
}
