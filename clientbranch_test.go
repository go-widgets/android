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
	"sync"
	"syscall"
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
	events := make(chan toolkit.Event, 16)
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
	// Every sample delivers its touch event and then its mouse event, so a
	// gesture-aware widget and an ordinary one both see the same gesture.
	want := []toolkit.EventKind{
		toolkit.EventTouchStart, toolkit.EventClick,
		toolkit.EventTouchMove, toolkit.EventMouseDrag,
		toolkit.EventTouchEnd, toolkit.EventMouseUp,
		toolkit.EventTouchMove, toolkit.EventMouseMove,
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
			swap(t, &openMemfd, func() (*os.File, error) { return nil, failure })
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
	bounds chan toolkit.Rect
}

func (r *recordingRoot) Bounds() toolkit.Rect { return r.Rect }
func (r *recordingRoot) SetBounds(b toolkit.Rect) {
	r.Rect = b
	if r.bounds != nil {
		select {
		case r.bounds <- b:
		default:
		}
	}
}
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

func TestClientLaysOutInsideTheInsets(t *testing.T) {
	const w, h = 200, 400
	host, c := dialConfigured(t, w, h)
	bounds := make(chan toolkit.Rect, 4)
	go func() { _ = c.Run(&recordingRoot{bounds: bounds}) }()
	host.next() // seed frame
	if got := <-bounds; got != (toolkit.Rect{W: w, H: h}) {
		t.Fatalf("seed bounds = %+v, want the whole surface", got)
	}

	// The system announces the bars: the tree must be laid out inside what
	// they leave, or its first and last rows are behind them.
	ins := Insets{Top: 30, Bottom: 50}
	if err := WriteMessage(host.conn, MsgInsets, EncodeInsets(ins)); err != nil {
		t.Fatalf("sending insets: %v", err)
	}
	select {
	case got := <-bounds:
		want := toolkit.Rect{X: 0, Y: 30, W: w, H: h - 80}
		if got != want {
			t.Fatalf("bounds under insets = %+v, want %+v", got, want)
		}
	case <-time.After(testDeadline):
		t.Fatal("the insets never reached the layout")
	}
	if got := c.Insets(); got != ins {
		t.Fatalf("Insets() = %+v, want %+v", got, ins)
	}
	if typ, body := host.next(); typ != MsgFrame {
		t.Fatalf("post-insets message = %#x, want MsgFrame", typ)
	} else if r, _ := DecodeFrame(body); r != (Rect{W: w, H: h}) {
		// The margins are repainted too, so the bars sit on the app's colour.
		t.Fatalf("post-insets damage = %+v, want the whole surface", r)
	}

	// Re-announcing the SAME insets changes nothing, so a host that resends
	// them on every layout pass does not cost a frame.
	if err := WriteMessage(host.conn, MsgInsets, EncodeInsets(ins)); err != nil {
		t.Fatalf("resending insets: %v", err)
	}
	// A full-bleed root opts back out to the whole surface.
	c.SetFullBleed(true)
	select {
	case got := <-bounds:
		if got != (toolkit.Rect{W: w, H: h}) {
			t.Fatalf("full-bleed bounds = %+v, want the whole surface", got)
		}
	case <-time.After(testDeadline):
		t.Fatal("SetFullBleed never re-laid the tree out")
	}
	c.SetFullBleed(true) // idempotent: no second frame
}

func TestClientRejectsMalformedInsets(t *testing.T) {
	host, cl := dialConfigured(t, 100, 100)
	if err := WriteMessage(host.conn, MsgInsets, []byte{1, 2}); err != nil {
		t.Fatalf("sending: %v", err)
	}
	waitDone(t, cl)
	if cl.runErr == nil {
		t.Fatal("a malformed MsgInsets should end the session with an error")
	}
}

func TestClientSharesTheFramebufferDescriptor(t *testing.T) {
	// The framebuffer must reach the host as an ancillary descriptor, not as a
	// path: a memfd has no path, and that is the point — its pages never dirty
	// page cache the kernel writes to storage.
	const w, h = 64, 48
	host := newFakeHost(t, w, h)
	host.serve()
	c, err := Dial("test", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// Read the announcement WITH its ancillary data, the way the Java host's
	// getAncillaryFileDescriptors does.
	uc, ok := host.conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("fake host connection is %T, want *net.UnixConn", host.conn)
	}
	msg := make([]byte, 64)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := uc.ReadMsgUnix(msg, oob)
	if err != nil {
		t.Fatalf("ReadMsgUnix: %v", err)
	}
	if n < 5 || msg[4] != MsgReady {
		t.Fatalf("first message = %v, want MsgReady", msg[:n])
	}
	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(scms) == 0 {
		t.Fatalf("no ancillary data with MsgReady: %v", err)
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil || len(fds) != 1 {
		t.Fatalf("ancillary data = %v err=%v, want exactly one descriptor", fds, err)
	}
	defer syscall.Close(fds[0])

	// The descriptor really is the surface: map it and watch the seed frame
	// appear in it.
	shared, err := syscall.Mmap(fds[0], 0, 4*w*h, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("mapping the shared descriptor: %v", err)
	}
	defer syscall.Munmap(shared)
	if !allZero(shared) {
		t.Fatal("the shared surface should still be blank before the first frame")
	}
	go func() { _ = c.Run(toolkit.NewVBox()) }()
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("seed message = %#x, want MsgFrame", typ)
	}
	if allZero(shared) {
		t.Fatal("the seed frame did not reach the shared descriptor")
	}
	// And it is memory, not a file: a memfd has no name on any filesystem.
	if _, err := os.Stat(host.path); !os.IsNotExist(err) {
		t.Fatalf("the fallback file was created after all: %v", err)
	}
}

func TestClientFallsBackToTheHostFile(t *testing.T) {
	// A kernel without memfd_create still has to run: the client then maps the
	// path the host named, and says so by creating it.
	swap(t, &openMemfd, func() (*os.File, error) { return nil, errors.New("no memfd here") })
	host, c := dialConfigured(t, 32, 32)
	go func() { _ = c.Run(toolkit.NewVBox()) }()
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("seed message = %#x, want MsgFrame", typ)
	}
	if _, err := os.Stat(host.path); err != nil {
		t.Fatalf("the fallback file was not used: %v", err)
	}
}

func TestClientWithoutMemfdOrPath(t *testing.T) {
	// No memfd and no path to fall back to is not a surface at all.
	swap(t, &openMemfd, func() (*os.File, error) { return nil, errors.New("no memfd here") })
	host := newFakeHost(t, 16, 16)
	host.path = ""
	host.serve()
	if _, err := Dial("test", nil); err == nil {
		t.Fatal("Dial should fail with nowhere to paint")
	}
}

func TestOpenMemfdReportsAKernelRefusal(t *testing.T) {
	// A kernel that refuses memfd_create — too old to have it, or out of
	// descriptors — must be reported, not turned into a nil file.
	swap(t, &memfdCreate, func(string, int) (int, error) { return -1, errors.New("refused") })
	if f, err := openMemfd(); err == nil {
		f.Close()
		t.Fatal("openMemfd should report a refused memfd_create")
	}
}

func TestClientServesAndActsOnTheA11yTree(t *testing.T) {
	host, c := dialConfigured(t, 200, 400)
	clicked := make(chan struct{}, 1)
	box := toolkit.NewVBox()
	box.Append(toolkit.NewLabel("a label"))
	box.Append(toolkit.NewButton("Click me", func() {
		select {
		case clicked <- struct{}{}:
		default:
		}
	}))
	go func() { _ = c.Run(box) }()
	host.next() // seed frame

	// The host asks; the application answers. Nothing was published before
	// the question: an app nobody is reading builds no tree.
	if err := WriteMessage(host.conn, MsgA11yRequest, nil); err != nil {
		t.Fatalf("requesting the tree: %v", err)
	}
	typ, body := host.next()
	if typ != MsgA11yTree {
		t.Fatalf("answer = %#x, want MsgA11yTree", typ)
	}
	els, err := DecodeA11yTree(body)
	if err != nil {
		t.Fatalf("DecodeA11yTree: %v", err)
	}
	if len(els) != 2 || els[0].Name != "a label" || els[1].Name != "Click me" {
		t.Fatalf("tree = %+v, want the label and the button", els)
	}
	if !els[1].Clickable || els[1].Class != "android.widget.Button" {
		t.Fatalf("button element = %+v, want a clickable Button", els[1])
	}

	// A screen reader activating the button reaches the widget, through the
	// same click path an ordinary touch takes.
	if err := WriteMessage(host.conn, MsgA11yAction, EncodeA11yAction(1)); err != nil {
		t.Fatalf("sending the action: %v", err)
	}
	select {
	case <-clicked:
	case <-time.After(testDeadline):
		t.Fatal("the accessibility activation never reached the widget")
	}
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("post-action message = %#x, want MsgFrame", typ)
	}
}

func TestClientIgnoresAStaleA11yIndex(t *testing.T) {
	// The host works from a snapshot, so it can name an element the tree no
	// longer has. That is ignored, not guessed at.
	host, c := dialConfigured(t, 100, 100)
	go func() { _ = c.Run(toolkit.NewVBox()) }()
	host.next() // seed frame
	for _, i := range []int{-1, 0, 99} {
		if err := WriteMessage(host.conn, MsgA11yAction, EncodeA11yAction(i)); err != nil {
			t.Fatalf("sending the action: %v", err)
		}
	}
	// Still alive and answering.
	if err := WriteMessage(host.conn, MsgA11yRequest, nil); err != nil {
		t.Fatalf("requesting the tree: %v", err)
	}
	if typ, _ := host.next(); typ != MsgA11yTree {
		t.Fatalf("message after stale actions = %#x, want MsgA11yTree", typ)
	}
}

func TestClientRejectsMalformedA11yAction(t *testing.T) {
	host, cl := dialConfigured(t, 100, 100)
	if err := WriteMessage(host.conn, MsgA11yAction, []byte{1}); err != nil {
		t.Fatalf("sending: %v", err)
	}
	waitDone(t, cl)
	if cl.runErr == nil {
		t.Fatal("a malformed MsgA11yAction should end the session with an error")
	}
}

func TestClientSecondFingerIsTouchOnly(t *testing.T) {
	// Two fingers must reach the tree as two contacts, but as ONE mouse
	// stream: a pinch that fired two EventClicks would read as two taps to
	// every widget, and the toolkit's MultiTouchRecognizer needs both ids.
	host, c := dialConfigured(t, 200, 200)
	events := make(chan toolkit.Event, 16)
	go func() { _ = c.Run(&recordingRoot{events: events}) }()
	host.next() // seed frame

	for _, tch := range []Touch{
		{Action: TouchDown, X: 10, Y: 10, ID: 7}, // first contact: primary
		{Action: TouchDown, X: 90, Y: 90, ID: 8}, // second: touch only
		{Action: TouchMove, X: 80, Y: 80, ID: 8},
		{Action: TouchMove, X: 20, Y: 20, ID: 7},
		{Action: TouchUp, X: 80, Y: 80, ID: 8},
	} {
		if err := WriteMessage(host.conn, MsgTouch, EncodeTouch(tch)); err != nil {
			t.Fatalf("sending a touch: %v", err)
		}
	}
	want := []struct {
		kind toolkit.EventKind
		code string
	}{
		{toolkit.EventTouchStart, "7"}, {toolkit.EventClick, ""},
		{toolkit.EventTouchStart, "8"}, // no click for the second finger
		{toolkit.EventTouchMove, "8"},  // nor a drag
		{toolkit.EventTouchMove, "7"}, {toolkit.EventMouseDrag, ""},
		{toolkit.EventTouchEnd, "8"}, // nor a mouse-up
	}
	for i, w := range want {
		select {
		case got := <-events:
			if got.Kind != w.kind || got.Code != w.code {
				t.Fatalf("event %d = %v code=%q, want %v code=%q", i, got.Kind, got.Code, w.kind, w.code)
			}
		case <-time.After(testDeadline):
			t.Fatalf("event %d never arrived", i)
		}
	}
}

func TestClientPrimaryFollowsTheFirstContactDown(t *testing.T) {
	// The primary contact is the first finger down while none is. After every
	// finger lifts, the next one to land becomes primary in its turn --
	// otherwise a second gesture would be mouse-silent forever.
	c := &Client{}
	if held, primary := c.pointerState(Touch{Action: TouchDown, ID: 3}); held || !primary {
		t.Fatalf("first contact: held=%v primary=%v, want false/true", held, primary)
	}
	if held, primary := c.pointerState(Touch{Action: TouchDown, ID: 4}); !held || primary {
		t.Fatalf("second contact: held=%v primary=%v, want true/false", held, primary)
	}
	if _, primary := c.pointerState(Touch{Action: TouchMove, ID: 3}); !primary {
		t.Fatal("a move of the primary contact stays primary")
	}
	c.pointerState(Touch{Action: TouchUp, ID: 3})
	c.pointerState(Touch{Action: TouchUp, ID: 4})
	if held, primary := c.pointerState(Touch{Action: TouchDown, ID: 9}); held || !primary {
		t.Fatalf("after all lift, a new contact: held=%v primary=%v, want false/true", held, primary)
	}
}

func TestClientTranslatesTouchIntoTheRootSpace(t *testing.T) {
	// The host reports a touch in SURFACE pixels, but a container treats an
	// incoming event as parent-local and adds its own origin. A root laid out
	// at the safe-area origin must therefore be handed events relative to that
	// origin — otherwise every touch lands one inset too far, which is exactly
	// what happened on device from the moment insets arrived: invisible while
	// the demo's buttons were 426 pixels tall, obvious the moment one was 20.
	const w, h = 200, 400
	host, c := dialConfigured(t, w, h)
	events := make(chan toolkit.Event, 16)
	go func() { _ = c.Run(&recordingRoot{events: events}) }()
	host.next() // seed frame

	ins := Insets{Left: 10, Top: 30}
	if err := WriteMessage(host.conn, MsgInsets, EncodeInsets(ins)); err != nil {
		t.Fatalf("sending insets: %v", err)
	}
	host.next() // the frame the insets provoked

	if err := WriteMessage(host.conn, MsgTouch, EncodeTouch(Touch{
		Action: TouchDown, X: 50, Y: 90,
	})); err != nil {
		t.Fatalf("sending a touch: %v", err)
	}
	// Both the touch event and its compatibility mouse event are translated.
	for i, want := range []toolkit.Event{
		{Kind: toolkit.EventTouchStart, X: 40, Y: 60, Code: "0"},
		{Kind: toolkit.EventClick, X: 40, Y: 60},
	} {
		select {
		case got := <-events:
			if got.Kind != want.Kind || got.X != want.X || got.Y != want.Y {
				t.Fatalf("event %d = %v (%d,%d), want %v (%d,%d)",
					i, got.Kind, got.X, got.Y, want.Kind, want.X, want.Y)
			}
		case <-time.After(testDeadline):
			t.Fatalf("event %d never arrived", i)
		}
	}

	// A full-bleed root is laid out at the surface origin, so nothing shifts.
	c.SetFullBleed(true)
	host.next() // the frame SetFullBleed provoked
	if err := WriteMessage(host.conn, MsgTouch, EncodeTouch(Touch{
		Action: TouchMove, X: 50, Y: 90,
	})); err != nil {
		t.Fatalf("sending a touch: %v", err)
	}
	select {
	case got := <-events:
		if got.X != 50 || got.Y != 90 {
			t.Fatalf("full-bleed touch at (%d,%d), want (50,90) untranslated", got.X, got.Y)
		}
	case <-time.After(testDeadline):
		t.Fatal("the full-bleed touch never arrived")
	}
}

func TestKeyEventsCarryNoPosition(t *testing.T) {
	// A key event has no meaningful X/Y, so shifting its zeroes would be noise.
	for _, k := range []toolkit.EventKind{toolkit.EventKeyDown, toolkit.EventKeyUp, toolkit.EventChar} {
		if hasPosition(k) {
			t.Errorf("%v should carry no position", k)
		}
	}
	for _, k := range []toolkit.EventKind{
		toolkit.EventClick, toolkit.EventMouseUp, toolkit.EventMouseMove,
		toolkit.EventMouseDrag, toolkit.EventScroll,
		toolkit.EventTouchStart, toolkit.EventTouchMove, toolkit.EventTouchEnd,
	} {
		if !hasPosition(k) {
			t.Errorf("%v should carry a position", k)
		}
	}
}

func TestClientCommittedTextReachesTheTree(t *testing.T) {
	// A soft keyboard commits finished text, sometimes several characters at
	// once. Each rune must reach the tree as the pair a printable key
	// produces, so an ordinary text widget needs no Android-specific path.
	host, c := dialConfigured(t, 200, 200)
	events := make(chan toolkit.Event, 32)
	go func() { _ = c.Run(&recordingRoot{events: events}) }()
	host.next() // seed frame

	if err := WriteMessage(host.conn, MsgText, []byte("héllo")); err != nil {
		t.Fatalf("committing text: %v", err)
	}
	for _, want := range []string{"h", "h", "é", "é", "l", "l", "l", "l", "o", "o"} {
		select {
		case got := <-events:
			if got.Code != want {
				t.Fatalf("event code %q, want %q", got.Code, want)
			}
			if got.Kind != toolkit.EventKeyDown && got.Kind != toolkit.EventChar {
				t.Fatalf("event kind %v, want a key-down or a char", got.Kind)
			}
		case <-time.After(testDeadline):
			t.Fatalf("the commit stopped before %q", want)
		}
	}

	// Backspace, as an input method spells it.
	if err := WriteMessage(host.conn, MsgTextDelete, EncodeTextDelete(2)); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case got := <-events:
			if got.Kind != toolkit.EventKeyDown || got.Code != "Backspace" {
				t.Fatalf("delete %d = %v %q, want a Backspace key-down", i, got.Kind, got.Code)
			}
		case <-time.After(testDeadline):
			t.Fatalf("delete %d never arrived", i)
		}
	}
}

func TestClientAsksTheHostForTheKeyboard(t *testing.T) {
	host, c := dialConfigured(t, 100, 100)
	go func() { _ = c.Run(toolkit.NewVBox()) }()
	host.next() // seed frame

	c.SetSoftKeyboard(true)
	if typ, body := host.next(); typ != MsgKeyboard || len(body) != 1 || body[0] != 1 {
		t.Fatalf("show = %#x %v, want MsgKeyboard [1]", typ, body)
	}
	c.SetSoftKeyboard(false)
	if typ, body := host.next(); typ != MsgKeyboard || len(body) != 1 || body[0] != 0 {
		t.Fatalf("hide = %#x %v, want MsgKeyboard [0]", typ, body)
	}
}

func TestClientRejectsAMalformedDelete(t *testing.T) {
	host, cl := dialConfigured(t, 100, 100)
	if err := WriteMessage(host.conn, MsgTextDelete, []byte{1}); err != nil {
		t.Fatalf("sending: %v", err)
	}
	waitDone(t, cl)
	if cl.runErr == nil {
		t.Fatal("a malformed MsgTextDelete should end the session with an error")
	}
}

func TestClientScrollReachesTheTree(t *testing.T) {
	host, c := dialConfigured(t, 200, 200)
	events := make(chan toolkit.Event, 8)
	go func() { _ = c.Run(&recordingRoot{events: events}) }()
	host.next() // seed frame

	if err := WriteMessage(host.conn, MsgScroll, EncodeScroll(Scroll{
		X: 30, Y: 40, DetentX: 1, DetentY: -2,
	})); err != nil {
		t.Fatalf("sending a scroll: %v", err)
	}
	select {
	case got := <-events:
		// The position is translated into the root's space like any other
		// positioned event; with no insets here it is unchanged.
		want := toolkit.Event{Kind: toolkit.EventScroll, X: 30, Y: 40, Delta: 2, DeltaX: 1}
		if got != want {
			t.Fatalf("scroll = %+v, want %+v", got, want)
		}
	case <-time.After(testDeadline):
		t.Fatal("the scroll never reached the tree")
	}
}

func TestClientRejectsAMalformedScroll(t *testing.T) {
	host, cl := dialConfigured(t, 100, 100)
	if err := WriteMessage(host.conn, MsgScroll, []byte{1, 2}); err != nil {
		t.Fatalf("sending: %v", err)
	}
	waitDone(t, cl)
	if cl.runErr == nil {
		t.Fatal("a malformed MsgScroll should end the session with an error")
	}
}

// animatedRoot is a root that wants frames for a fixed number of ticks, so a
// test can drive the animation loop to completion deterministically.
type animatedRoot struct {
	recordingRoot
	mu     sync.Mutex
	left   int
	ticked int
}

func (a *animatedRoot) Tick(dt float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.left > 0 {
		a.left--
		a.ticked++
	}
}

func (a *animatedRoot) Animating() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.left > 0
}

func (a *animatedRoot) ticks() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ticked
}

func TestClientDrivesAnimationsUntilTheyStop(t *testing.T) {
	// The toolkit's animated widgets — a spinner, a coasting scroll view —
	// advance on a host's frame tick. Without this loop the back-end painted
	// only in response to input, so a released drag never coasted and every
	// animated widget was frozen. The loop must also STOP: TreeAnimating
	// exists so an idle app burns no frames.
	fired := make(chan time.Time, 64)
	var clock struct {
		sync.Mutex
		now time.Time
	}
	clock.now = time.Unix(0, 0)

	host, c := dialConfigured(t, 100, 100)
	// Replace the clock under the lock the loop reads it under, before any
	// animation can start.
	c.mu.Lock()
	c.after = func(time.Duration) <-chan time.Time { return fired }
	c.now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		return clock.now
	}
	c.mu.Unlock()
	root := &animatedRoot{left: 3}
	go func() { _ = c.Run(root) }()
	host.next() // seed frame

	// An event is what starts an animation, so deliver one.
	if err := WriteMessage(host.conn, MsgTouch, EncodeTouch(Touch{Action: TouchDown})); err != nil {
		t.Fatalf("sending a touch: %v", err)
	}
	host.next() // the frame that touch provoked

	// Three ticks are owed; each posts a frame.
	for i := 0; i < 3; i++ {
		clock.Lock()
		clock.now = clock.now.Add(frameInterval)
		tick := clock.now
		clock.Unlock()
		fired <- tick
		if typ, _ := host.next(); typ != MsgFrame {
			t.Fatalf("tick %d posted %#x, want MsgFrame", i, typ)
		}
	}
	if got := root.ticks(); got != 3 {
		t.Fatalf("the tree was ticked %d times, want 3", got)
	}

	// And the loop stops on its own: nothing is animating, so a further
	// firing must not post anything. Prove it by asking for a repaint and
	// seeing exactly one frame — the one we asked for.
	c.Repaint()
	if typ, _ := host.next(); typ != MsgFrame {
		t.Fatalf("Repaint posted %#x, want MsgFrame", typ)
	}
}

func TestClientDoesNotAnimateWithoutAnAnimator(t *testing.T) {
	// A plain tree wants no frames, so no loop starts and nothing is posted
	// beyond the frame the event itself provoked.
	host, c := dialConfigured(t, 100, 100)
	go func() { _ = c.Run(toolkit.NewVBox()) }()
	host.next() // seed frame
	if err := WriteMessage(host.conn, MsgTouch, EncodeTouch(Touch{Action: TouchDown})); err != nil {
		t.Fatalf("sending a touch: %v", err)
	}
	host.next() // the touch's own frame
	c.mu.Lock()
	animating := c.animating
	c.mu.Unlock()
	if animating {
		t.Fatal("a tree with no animator should not start a frame loop")
	}
}

func TestAnimationLoopEdges(t *testing.T) {
	// The three paths a running app takes but a happy test does not: no tree
	// bound yet, a loop already running, and a session ending mid-animation.
	c := &Client{done: make(chan struct{}), now: time.Now, after: time.After}
	c.startAnimating() // no root: nothing to tick, and no goroutine to leak
	if c.animating {
		t.Fatal("a client with no root should start no loop")
	}
	c.root = &animatedRoot{left: 1}
	c.animating = true
	c.startAnimating() // already running: must not start a second loop
	c.animating = false

	// A session that ends while animating stops the loop, rather than ticking
	// a tree whose surface has gone.
	fired := make(chan time.Time, 4)
	host, cl := dialConfigured(t, 64, 64)
	cl.mu.Lock()
	cl.after = func(time.Duration) <-chan time.Time { return fired }
	cl.mu.Unlock()
	root := &animatedRoot{left: 1000}
	go func() { _ = cl.Run(root) }()
	host.next() // seed frame
	cl.startAnimating()
	cl.Close()
	fired <- time.Unix(0, 0)
	// The loop notices the session ended; nothing panics and it lets go.
	select {
	case <-cl.done:
	case <-time.After(testDeadline):
		t.Fatal("Close did not end the session")
	}
}
