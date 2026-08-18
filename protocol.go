// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Package android implements the application half of the go-widgets
// Android host protocol, so a go-widgets application runs inside a real
// Android app exactly as it runs on X11, Wayland, Cocoa or Win32.
//
// Android hands out no drawable surface to a process that is not the app: the
// whole graphics API is behind JNI, and JNI needs cgo. So the app is split in
// two. A thin Java host owns the Activity, the SurfaceView and the input
// stream; the go-widgets application is an ordinary CGO-free executable the
// host spawns, which paints into a shared mapping and tells the host which
// rectangle changed. The split is the same one the Linux back-ends already
// live with — a socket protocol plus a shared pixel buffer — with the Java
// host standing where the X server or the Wayland compositor stands.
//
// This file is the SOVEREIGN, transport-agnostic codec: the wire messages, the
// framing, and the input→toolkit.Event mapping, over plain Go values. It
// carries no syscall and no net dependency, so it builds — and is unit-tested
// to 100% — on every GOOS. The transport that dials the host socket, maps the
// buffer and drives a widget tree lives in client.go.
package android

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/go-widgets/toolkit"
)

// Message types. Host→app messages are below 0x80, app→host at or above it, so
// a misrouted message is a decode error rather than a plausible other message.
const (
	// MsgConfig carries the surface geometry and the shared buffer path. The
	// host sends it once at start-up and again on every resize or rotation.
	MsgConfig uint8 = 0x01
	// MsgTouch carries one pointer sample in surface pixels.
	MsgTouch uint8 = 0x02
	// MsgKey carries one key event: an Android key code plus the unicode rune
	// the host's key-character map produced (0 when the key produces none).
	MsgKey uint8 = 0x03
	// MsgLifecycle carries an Activity transition: the app keeps its widget
	// tree across a pause, but stops painting until it resumes.
	MsgLifecycle uint8 = 0x04
	// MsgClose asks the application to end its Run loop.
	MsgClose uint8 = 0x05
	// MsgA11yRequest asks the application for its accessibility tree. The host
	// sends it only when something is actually reading one, so an app with no
	// screen reader attached never builds a tree at all.
	MsgA11yRequest uint8 = 0x07
	// MsgA11yAction carries the index of the element a screen reader activated.
	MsgA11yAction uint8 = 0x08
	// MsgInsets carries the area of the surface the system is drawing over.
	// It is its own message rather than a Config field because insets change
	// on their own schedule: the soft keyboard opening does not resize the
	// surface, and a bar auto-hiding does not either.
	MsgInsets uint8 = 0x06

	// MsgReady tells the host the shared buffer is mapped at the announced
	// size, so the host may map it in turn. Every MsgFrame that follows
	// refers to this mapping, until the next MsgReady replaces it.
	MsgReady uint8 = 0x81
	// MsgFrame tells the host which surface-local rectangle changed.
	MsgFrame uint8 = 0x82
	// MsgTitle updates the host's window title.
	MsgTitle uint8 = 0x83
	// MsgBye tells the host the application ended.
	MsgBye uint8 = 0x84
	// MsgA11yTree answers MsgA11yRequest with the accessibility elements.
	MsgA11yTree uint8 = 0x85
)

// Touch actions, matching the three MotionEvent actions the host forwards.
const (
	TouchDown uint8 = 0
	TouchUp   uint8 = 1
	TouchMove uint8 = 2
)

// Key actions.
const (
	KeyDown uint8 = 0
	KeyUp   uint8 = 1
)

// Lifecycle states.
const (
	LifecyclePause  uint8 = 0
	LifecycleResume uint8 = 1
)

// MaxPayload bounds one decoded message body. The largest message a host
// legitimately sends is a Config carrying a filesystem path, so a frame beyond
// this is a desynchronised stream — refused rather than allocated.
const MaxPayload = 1 << 16

// ErrShortPayload reports a message whose body is too short for its type.
var ErrShortPayload = errors.New("android: truncated message payload")

// Rect is a surface-local rectangle in pixels. It mirrors toolkit.Rect but is
// kept local so the codec stays a leaf with one toolkit dependency (the event
// model).
type Rect struct{ X, Y, W, H int }

// Config is the host's geometry announcement.
type Config struct {
	// W and H are the surface size in physical pixels.
	W, H int
	// Density is the display density in hundredths (Android's
	// DisplayMetrics.density × 100, so a 3.0x panel arrives as 300). It is the
	// Android spelling of the backing-scale factor the Cocoa back-end reads
	// from the screen.
	Density int
	// BufPath is the file the application maps as its framebuffer. The host
	// picks it inside the app's own storage, which both processes share.
	BufPath string
}

// Touch is one pointer sample.
type Touch struct {
	Action uint8
	X, Y   int
	// ID is the pointer index, so a later multi-touch host can be told apart
	// from this one without a protocol break. Single-touch hosts send 0.
	ID int
}

// Insets is the margin of the surface the system draws over, in pixels.
//
// An Android window is edge-to-edge from API 35: the surface really is the
// whole screen, and the status bar, the navigation bar, a display cutout and
// the soft keyboard are painted ON TOP of it rather than shrinking it. So a
// widget tree laid out to the full surface is correct in size and wrong in
// practice — its first and last rows are behind the bars. These are the four
// edges to keep clear.
type Insets struct{ Left, Top, Right, Bottom int }

// Empty reports whether nothing is covering the surface.
func (i Insets) Empty() bool { return i == Insets{} }

// Apply returns the part of a w×h surface that nothing is drawn over. It never
// returns a negative extent: insets wider than the surface (a phone folded to
// a sliver, a bad host) collapse the area to zero rather than inverting it.
func (i Insets) Apply(w, h int) Rect {
	r := Rect{X: i.Left, Y: i.Top, W: w - i.Left - i.Right, H: h - i.Top - i.Bottom}
	if r.W < 0 {
		r.W = 0
	}
	if r.H < 0 {
		r.H = 0
	}
	return r
}

// EncodeInsets builds a MsgInsets body.
func EncodeInsets(i Insets) []byte {
	b := appendInt32(make([]byte, 0, 16), i.Left)
	b = appendInt32(b, i.Top)
	b = appendInt32(b, i.Right)
	return appendInt32(b, i.Bottom)
}

// DecodeInsets parses a MsgInsets body.
func DecodeInsets(b []byte) (Insets, error) {
	if len(b) < 16 {
		return Insets{}, ErrShortPayload
	}
	return Insets{
		Left:   int32At(b, 0),
		Top:    int32At(b, 4),
		Right:  int32At(b, 8),
		Bottom: int32At(b, 12),
	}, nil
}

// Key is one key event.
type Key struct {
	Action uint8
	// Code is the Android KeyEvent key code.
	Code int
	// Rune is the character the key produced, or 0 for a key that produces
	// none (an arrow, a modifier, the back key).
	Rune rune
}

// EncodeConfig builds a MsgConfig body.
func EncodeConfig(c Config) []byte {
	b := make([]byte, 0, 16+len(c.BufPath))
	b = appendInt32(b, c.W)
	b = appendInt32(b, c.H)
	b = appendInt32(b, c.Density)
	b = appendInt32(b, len(c.BufPath))
	return append(b, c.BufPath...)
}

// DecodeConfig parses a MsgConfig body.
func DecodeConfig(b []byte) (Config, error) {
	if len(b) < 16 {
		return Config{}, ErrShortPayload
	}
	c := Config{W: int32At(b, 0), H: int32At(b, 4), Density: int32At(b, 8)}
	n := int32At(b, 12)
	if n < 0 || 16+n > len(b) {
		return Config{}, ErrShortPayload
	}
	c.BufPath = string(b[16 : 16+n])
	return c, nil
}

// EncodeTouch builds a MsgTouch body.
func EncodeTouch(t Touch) []byte {
	b := make([]byte, 0, 13)
	b = append(b, t.Action)
	b = appendInt32(b, t.X)
	b = appendInt32(b, t.Y)
	return appendInt32(b, t.ID)
}

// DecodeTouch parses a MsgTouch body.
func DecodeTouch(b []byte) (Touch, error) {
	if len(b) < 13 {
		return Touch{}, ErrShortPayload
	}
	return Touch{Action: b[0], X: int32At(b, 1), Y: int32At(b, 5), ID: int32At(b, 9)}, nil
}

// EncodeKey builds a MsgKey body.
func EncodeKey(k Key) []byte {
	b := make([]byte, 0, 9)
	b = append(b, k.Action)
	b = appendInt32(b, k.Code)
	return appendInt32(b, int(k.Rune))
}

// DecodeKey parses a MsgKey body.
func DecodeKey(b []byte) (Key, error) {
	if len(b) < 9 {
		return Key{}, ErrShortPayload
	}
	return Key{Action: b[0], Code: int32At(b, 1), Rune: rune(int32At(b, 5))}, nil
}

// EncodeReady builds a MsgReady body: the size the application actually mapped.
func EncodeReady(w, h int) []byte {
	return appendInt32(appendInt32(make([]byte, 0, 8), w), h)
}

// DecodeReady parses a MsgReady body.
func DecodeReady(b []byte) (w, h int, err error) {
	if len(b) < 8 {
		return 0, 0, ErrShortPayload
	}
	return int32At(b, 0), int32At(b, 4), nil
}

// EncodeFrame builds a MsgFrame body naming the damaged rectangle.
func EncodeFrame(r Rect) []byte {
	b := appendInt32(make([]byte, 0, 16), r.X)
	b = appendInt32(b, r.Y)
	b = appendInt32(b, r.W)
	return appendInt32(b, r.H)
}

// DecodeFrame parses a MsgFrame body.
func DecodeFrame(b []byte) (Rect, error) {
	if len(b) < 16 {
		return Rect{}, ErrShortPayload
	}
	return Rect{X: int32At(b, 0), Y: int32At(b, 4), W: int32At(b, 8), H: int32At(b, 12)}, nil
}

// FrameMessage returns one framed message: a 4-byte big-endian length covering
// the type byte and the body, then the type byte, then the body. Big-endian
// keeps the Java host on DataInputStream.readInt with no byte-swapping.
//
// It exists as bytes rather than as writes because a message that carries an
// ancillary descriptor has to reach the host in ONE sendmsg: split across two
// writes, the host could attribute the descriptor to the wrong message.
func FrameMessage(typ uint8, body []byte) []byte {
	b := make([]byte, 5+len(body))
	binary.BigEndian.PutUint32(b, uint32(len(body)+1))
	b[4] = typ
	copy(b[5:], body)
	return b
}

// WriteMessage writes one framed message.
func WriteMessage(w io.Writer, typ uint8, body []byte) error {
	_, err := w.Write(FrameMessage(typ, body))
	return err
}

// ReadMessage reads one framed message. It returns io.EOF when the stream ends
// cleanly between messages, so a caller can tell a closed host from a truncated
// one.
func ReadMessage(r io.Reader) (typ uint8, body []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:4]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:4]))
	if n < 1 || n > MaxPayload {
		return 0, nil, fmt.Errorf("android: message length %d out of range", n)
	}
	if _, err := io.ReadFull(r, hdr[4:]); err != nil {
		return 0, nil, err
	}
	body = make([]byte, n-1)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return hdr[4], body, nil
}

// MapTouch maps one pointer sample to toolkit events.
//
// It emits TWO events per sample: the touch event first, then a mouse event.
// That is not belt and braces, it is what a touch platform owes a toolkit that
// has both. go-widgets models touch directly — EventTouchStart/Move/End carry
// a pointer id in Event.Code, and GestureRecognizer turns them into taps, long
// presses and swipes — so a back-end that only emitted mouse events would
// leave every gesture-aware widget deaf on the one kind of device gestures are
// for. The compatibility mouse event follows because most widgets, and every
// widget written before touch existed, listen for EventClick; a browser does
// exactly this, for exactly this reason.
//
// The mouse half mirrors the wasmbox and X11 mappings: a press is a click, a
// move with the finger down is a drag. A touch screen has no hover, so a move
// with no finger down cannot occur and is mapped to a plain move rather than
// dropped, keeping a synthetic host (a test, a replay) honest.
func MapTouch(t Touch, held bool) []toolkit.Event {
	id := strconv.Itoa(t.ID)
	switch t.Action {
	case TouchDown:
		return []toolkit.Event{
			{Kind: toolkit.EventTouchStart, X: t.X, Y: t.Y, Code: id},
			{Kind: toolkit.EventClick, X: t.X, Y: t.Y},
		}
	case TouchUp:
		return []toolkit.Event{
			{Kind: toolkit.EventTouchEnd, X: t.X, Y: t.Y, Code: id},
			{Kind: toolkit.EventMouseUp, X: t.X, Y: t.Y},
		}
	case TouchMove:
		kind := toolkit.EventMouseMove
		if held {
			kind = toolkit.EventMouseDrag
		}
		return []toolkit.Event{
			{Kind: toolkit.EventTouchMove, X: t.X, Y: t.Y, Code: id},
			{Kind: kind, X: t.X, Y: t.Y},
		}
	default:
		return nil
	}
}

// Android KeyEvent key codes the toolkit has a named key for. Only the codes
// that map to a toolkit key are listed; anything else reaches the tree as its
// unicode rune, or not at all.
const (
	akeycodeDpadUp     = 19
	akeycodeDpadDown   = 20
	akeycodeDpadLeft   = 21
	akeycodeDpadRight  = 22
	akeycodeEnter      = 66
	akeycodeDel        = 67 // backspace
	akeycodeTab        = 61
	akeycodeEscape     = 111
	akeycodeForwardDel = 112
	akeycodeMoveHome   = 122
	akeycodeMoveEnd    = 123
	akeycodePageUp     = 92
	akeycodePageDown   = 93
	akeycodeSpace      = 62
)

// androidKeys maps an Android key code to the DOM-style key name the toolkit's
// widgets switch on (toolkit.Event.Code) — the same names internal/x11's
// keysym table produces, so a widget's key handling is identical on every
// back-end.
var androidKeys = map[int]string{
	akeycodeDpadUp:     "ArrowUp",
	akeycodeDpadDown:   "ArrowDown",
	akeycodeDpadLeft:   "ArrowLeft",
	akeycodeDpadRight:  "ArrowRight",
	akeycodeEnter:      "Enter",
	akeycodeDel:        "Backspace",
	akeycodeTab:        "Tab",
	akeycodeEscape:     "Escape",
	akeycodeForwardDel: "Delete",
	akeycodeMoveHome:   "Home",
	akeycodeMoveEnd:    "End",
	akeycodePageUp:     "PageUp",
	akeycodePageDown:   "PageDown",
	akeycodeSpace:      "Space",
}

// MapKey maps one Android key event to toolkit events, mirroring the wasmbox
// and X11 mappings: a named key is one EventKeyDown/EventKeyUp; a key that
// committed a character is an EventKeyDown followed by an EventChar on press,
// and an EventKeyUp on release. A key that is neither named nor printable
// reaches the tree as nothing.
func MapKey(k Key) []toolkit.Event {
	press := k.Action == KeyDown
	kind := toolkit.EventKeyDown
	if !press {
		kind = toolkit.EventKeyUp
	}
	if name, ok := androidKeys[k.Code]; ok {
		return []toolkit.Event{{Kind: kind, Code: name}}
	}
	if k.Rune == 0 {
		return nil
	}
	code := string(k.Rune)
	if !press {
		return []toolkit.Event{{Kind: toolkit.EventKeyUp, Code: code}}
	}
	return []toolkit.Event{
		{Kind: toolkit.EventKeyDown, Code: code},
		{Kind: toolkit.EventChar, Code: code},
	}
}

// ClampRect clips r to a w×h surface, returning a zero-area rectangle when
// nothing of r is inside. The host trusts the rectangle it is given, so the
// application clamps before sending.
func ClampRect(r Rect, w, h int) Rect {
	if r.X < 0 {
		r.W += r.X
		r.X = 0
	}
	if r.Y < 0 {
		r.H += r.Y
		r.Y = 0
	}
	if r.X+r.W > w {
		r.W = w - r.X
	}
	if r.Y+r.H > h {
		r.H = h - r.Y
	}
	if r.W < 0 || r.H < 0 || r.X >= w || r.Y >= h {
		return Rect{}
	}
	return r
}

// appendInt32 appends v as a 4-byte big-endian integer.
func appendInt32(b []byte, v int) []byte {
	return binary.BigEndian.AppendUint32(b, uint32(int32(v)))
}

// int32At reads the 4-byte big-endian integer at off.
func int32At(b []byte, off int) int { return int(int32(binary.BigEndian.Uint32(b[off:]))) }

// ErrUnsupported reports an environment with no Android host: every GOOS but
// Linux, where the abstract socket and the shared mapping the host protocol
// needs do not exist. A cross-built application gets this from Dial and can
// report it and exit cleanly, exactly as go-widgets/window does off its
// supported back-ends.
var ErrUnsupported = errors.New("android: no Android host on this platform")
