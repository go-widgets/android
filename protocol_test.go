// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/go-widgets/toolkit"
)

func TestConfigRoundTrip(t *testing.T) {
	want := Config{W: 1080, H: 2400, Density: 263, BufPath: "/data/data/app/cache/gw-surface.rgba"}
	got, err := DecodeConfig(EncodeConfig(want))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	// An empty path is legal: it is how a host announces geometry only.
	if got, err := DecodeConfig(EncodeConfig(Config{W: 1, H: 1})); err != nil || got.BufPath != "" {
		t.Fatalf("empty path = %+v err=%v", got, err)
	}
}

func TestConfigDecodeErrors(t *testing.T) {
	if _, err := DecodeConfig(make([]byte, 15)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("short header err = %v, want ErrShortPayload", err)
	}
	// A path length longer than the body left is a truncated message, not a
	// reason to slice out of range.
	b := EncodeConfig(Config{W: 1, H: 1, BufPath: "/tmp/x"})
	if _, err := DecodeConfig(b[:len(b)-1]); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("truncated path err = %v, want ErrShortPayload", err)
	}
	// A negative length is refused rather than panicking on the slice.
	neg := EncodeConfig(Config{W: 1, H: 1})
	neg[12] = 0xff
	if _, err := DecodeConfig(neg); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("negative path length err = %v, want ErrShortPayload", err)
	}
}

func TestTouchRoundTrip(t *testing.T) {
	want := Touch{Action: TouchMove, X: 540, Y: -12, ID: 3}
	got, err := DecodeTouch(EncodeTouch(want))
	if err != nil || got != want {
		t.Fatalf("round trip = %+v err=%v, want %+v", got, err, want)
	}
	if _, err := DecodeTouch(make([]byte, 12)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("short err = %v, want ErrShortPayload", err)
	}
}

func TestKeyRoundTrip(t *testing.T) {
	want := Key{Action: KeyDown, Code: 66, Rune: 'é'}
	got, err := DecodeKey(EncodeKey(want))
	if err != nil || got != want {
		t.Fatalf("round trip = %+v err=%v, want %+v", got, err, want)
	}
	if _, err := DecodeKey(make([]byte, 8)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("short err = %v, want ErrShortPayload", err)
	}
}

func TestReadyRoundTrip(t *testing.T) {
	w, h, err := DecodeReady(EncodeReady(1080, 2400))
	if err != nil || w != 1080 || h != 2400 {
		t.Fatalf("round trip = %dx%d err=%v", w, h, err)
	}
	if _, _, err := DecodeReady(make([]byte, 7)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("short err = %v, want ErrShortPayload", err)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	want := Rect{X: 4, Y: 8, W: 15, H: 16}
	got, err := DecodeFrame(EncodeFrame(want))
	if err != nil || got != want {
		t.Fatalf("round trip = %+v err=%v, want %+v", got, err, want)
	}
	if _, err := DecodeFrame(make([]byte, 15)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("short err = %v, want ErrShortPayload", err)
	}
}

func TestMessageFraming(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, MsgFrame, EncodeFrame(Rect{W: 2, H: 3})); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	// A bodyless message (MsgBye) still carries its type.
	if err := WriteMessage(&buf, MsgBye, nil); err != nil {
		t.Fatalf("WriteMessage empty: %v", err)
	}
	typ, body, err := ReadMessage(&buf)
	if err != nil || typ != MsgFrame {
		t.Fatalf("first message type %#x err=%v", typ, err)
	}
	if r, err := DecodeFrame(body); err != nil || r != (Rect{W: 2, H: 3}) {
		t.Fatalf("first body = %+v err=%v", r, err)
	}
	if typ, body, err := ReadMessage(&buf); err != nil || typ != MsgBye || len(body) != 0 {
		t.Fatalf("second message = %#x %v err=%v", typ, body, err)
	}
	// A clean end between messages is io.EOF, so a caller can tell a closed
	// host from a truncated one.
	if _, _, err := ReadMessage(&buf); err != io.EOF {
		t.Fatalf("end of stream err = %v, want io.EOF", err)
	}
}

func TestReadMessageErrors(t *testing.T) {
	// A zero length claims a message with not even a type byte.
	if _, _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 0})); err == nil {
		t.Fatal("zero length should error")
	}
	// A length beyond MaxPayload is a desynchronised stream, refused before
	// any allocation.
	if _, _, err := ReadMessage(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("oversize length should error")
	}
	// Truncated after the length, and truncated inside the body.
	if _, _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 4})); err == nil {
		t.Fatal("missing type byte should error")
	}
	if _, _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 4, MsgFrame, 1})); err == nil {
		t.Fatal("truncated body should error")
	}
}

// errWriter fails on the Nth write, so both WriteMessage failure paths (header
// and body) are reachable without a real broken socket.
type errWriter struct {
	n    int
	fail int
}

func (w *errWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n == w.fail {
		return 0, errors.New("write failed")
	}
	return len(p), nil
}

func TestWriteMessageErrors(t *testing.T) {
	// Framing is ONE write, so there is one failure to report.
	if err := WriteMessage(&errWriter{fail: 1}, MsgFrame, []byte{1}); err == nil {
		t.Fatal("a failed write should error")
	}
}

func TestMapTouch(t *testing.T) {
	cases := []struct {
		name string
		t    Touch
		held bool
		want []toolkit.Event
	}{
		// Each sample yields the touch event a gesture-aware widget needs,
		// then the mouse event every other widget listens for.
		{"press", Touch{Action: TouchDown, X: 3, Y: 4, ID: 1}, false,
			[]toolkit.Event{
				{Kind: toolkit.EventTouchStart, X: 3, Y: 4, Code: "1"},
				{Kind: toolkit.EventClick, X: 3, Y: 4},
			}},
		{"release", Touch{Action: TouchUp, X: 3, Y: 4, ID: 1}, true,
			[]toolkit.Event{
				{Kind: toolkit.EventTouchEnd, X: 3, Y: 4, Code: "1"},
				{Kind: toolkit.EventMouseUp, X: 3, Y: 4},
			}},
		{"move with the finger down is a drag", Touch{Action: TouchMove, X: 5, Y: 6}, true,
			[]toolkit.Event{
				{Kind: toolkit.EventTouchMove, X: 5, Y: 6, Code: "0"},
				{Kind: toolkit.EventMouseDrag, X: 5, Y: 6},
			}},
		{"move with no finger down", Touch{Action: TouchMove, X: 5, Y: 6}, false,
			[]toolkit.Event{
				{Kind: toolkit.EventTouchMove, X: 5, Y: 6, Code: "0"},
				{Kind: toolkit.EventMouseMove, X: 5, Y: 6},
			}},
		{"a second finger keeps its own id", Touch{Action: TouchDown, X: 9, Y: 9, ID: 2}, false,
			[]toolkit.Event{
				{Kind: toolkit.EventTouchStart, X: 9, Y: 9, Code: "2"},
				{Kind: toolkit.EventClick, X: 9, Y: 9},
			}},
		{"an action the host does not forward", Touch{Action: 99}, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MapTouch(c.t, c.held); !slices.Equal(got, c.want) {
				t.Fatalf("MapTouch = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestMapTouchDrivesTheGestureRecognizer feeds a real toolkit.GestureRecognizer
// the events this back-end produces. It is the proof that the touch half is not
// decoration: tap, long press and swipe are toolkit machinery that only ever
// fires on EventTouch*, and before this back-end emitted them, every one of
// those gestures was dead on Android — the one platform they exist for.
func TestMapTouchDrivesTheGestureRecognizer(t *testing.T) {
	feed := func(g *toolkit.GestureRecognizer, samples ...Touch) {
		for _, s := range samples {
			for _, ev := range MapTouch(s, s.Action == TouchMove) {
				g.Feed(ev)
			}
		}
	}

	t.Run("tap", func(t *testing.T) {
		g := toolkit.NewGestureRecognizer()
		var tapped bool
		g.OnTap = func(x, y int) { tapped = true }
		feed(g,
			Touch{Action: TouchDown, X: 50, Y: 50, ID: 1},
			Touch{Action: TouchUp, X: 51, Y: 50, ID: 1},
		)
		if !tapped {
			t.Fatal("a press and release in place should be a tap")
		}
	})

	t.Run("swipe", func(t *testing.T) {
		g := toolkit.NewGestureRecognizer()
		got := SwipeDirNone
		g.OnSwipe = func(d toolkit.SwipeDir) { got = int(d) }
		feed(g,
			Touch{Action: TouchDown, X: 200, Y: 50, ID: 1},
			Touch{Action: TouchMove, X: 100, Y: 50, ID: 1},
			Touch{Action: TouchUp, X: 100, Y: 50, ID: 1},
		)
		if got != int(toolkit.SwipeLeft) {
			t.Fatalf("swipe = %v, want SwipeLeft", got)
		}
	})

	t.Run("long press", func(t *testing.T) {
		g := toolkit.NewGestureRecognizer()
		var held bool
		g.OnLongPress = func(x, y int) { held = true }
		feed(g, Touch{Action: TouchDown, X: 50, Y: 50, ID: 1})
		for i := 0; i < g.LongPressTicks; i++ {
			g.Tick()
		}
		if !held {
			t.Fatal("a touch held past LongPressTicks should be a long press")
		}
	})
}

// SwipeDirNone is a sentinel outside the SwipeDir range, so a test can tell
// "no swipe fired" from "SwipeLeft fired" (SwipeLeft being zero).
const SwipeDirNone = -1

func TestMapKey(t *testing.T) {
	cases := []struct {
		name string
		k    Key
		want []toolkit.Event
	}{
		{"named key press", Key{Action: KeyDown, Code: akeycodeEnter},
			[]toolkit.Event{{Kind: toolkit.EventKeyDown, Code: "Enter"}}},
		{"named key release", Key{Action: KeyUp, Code: akeycodeDpadUp},
			[]toolkit.Event{{Kind: toolkit.EventKeyUp, Code: "ArrowUp"}}},
		{"printable press commits a character", Key{Action: KeyDown, Code: 29, Rune: 'a'},
			[]toolkit.Event{
				{Kind: toolkit.EventKeyDown, Code: "a"},
				{Kind: toolkit.EventChar, Code: "a"},
			}},
		{"printable release", Key{Action: KeyUp, Code: 29, Rune: 'a'},
			[]toolkit.Event{{Kind: toolkit.EventKeyUp, Code: "a"}}},
		{"a key that is neither named nor printable", Key{Action: KeyDown, Code: 82}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MapKey(c.k); !slices.Equal(got, c.want) {
				t.Fatalf("MapKey = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestMapKeyMatchesX11Names pins the named-key vocabulary to the DOM-style
// names the X11 back-end emits: a widget's key handling must not depend on
// which back-end delivered the press.
func TestMapKeyMatchesX11Names(t *testing.T) {
	want := map[int]string{
		akeycodeDpadUp: "ArrowUp", akeycodeDpadDown: "ArrowDown",
		akeycodeDpadLeft: "ArrowLeft", akeycodeDpadRight: "ArrowRight",
		akeycodeEnter: "Enter", akeycodeDel: "Backspace", akeycodeTab: "Tab",
		akeycodeEscape: "Escape", akeycodeForwardDel: "Delete",
		akeycodeMoveHome: "Home", akeycodeMoveEnd: "End",
		akeycodePageUp: "PageUp", akeycodePageDown: "PageDown",
		akeycodeSpace: "Space",
	}
	for code, name := range want {
		got := MapKey(Key{Action: KeyDown, Code: code})
		if len(got) != 1 || got[0].Code != name {
			t.Errorf("key code %d = %+v, want Code %q", code, got, name)
		}
	}
	if len(androidKeys) != len(want) {
		t.Errorf("androidKeys has %d entries, the pinned set has %d", len(androidKeys), len(want))
	}
}

func TestClampRect(t *testing.T) {
	cases := []struct {
		name string
		in   Rect
		want Rect
	}{
		{"inside is unchanged", Rect{X: 1, Y: 2, W: 3, H: 4}, Rect{X: 1, Y: 2, W: 3, H: 4}},
		{"negative origin clips to zero", Rect{X: -2, Y: -3, W: 10, H: 10}, Rect{W: 8, H: 7}},
		{"overhang clips to the surface", Rect{X: 8, Y: 8, W: 10, H: 10}, Rect{X: 8, Y: 8, W: 2, H: 2}},
		{"wholly right of the surface", Rect{X: 10, Y: 0, W: 5, H: 5}, Rect{}},
		{"wholly below the surface", Rect{X: 0, Y: 10, W: 5, H: 5}, Rect{}},
		{"negative width", Rect{X: 0, Y: 0, W: -1, H: 5}, Rect{}},
		{"negative height", Rect{X: 0, Y: 0, W: 5, H: -1}, Rect{}},
		{"entirely left of the surface", Rect{X: -20, Y: 0, W: 5, H: 5}, Rect{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClampRect(c.in, 10, 10); got != c.want {
				t.Fatalf("ClampRect(%+v) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

func TestInsetsRoundTrip(t *testing.T) {
	want := Insets{Left: 0, Top: 96, Right: 0, Bottom: 132}
	got, err := DecodeInsets(EncodeInsets(want))
	if err != nil || got != want {
		t.Fatalf("round trip = %+v err=%v, want %+v", got, err, want)
	}
	if _, err := DecodeInsets(make([]byte, 15)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("short err = %v, want ErrShortPayload", err)
	}
}

func TestInsetsApply(t *testing.T) {
	cases := []struct {
		name string
		in   Insets
		want Rect
	}{
		{"nothing covering the surface", Insets{}, Rect{W: 100, H: 200}},
		{"status and navigation bars", Insets{Top: 30, Bottom: 40}, Rect{Y: 30, W: 100, H: 130}},
		{"a landscape cutout on the left", Insets{Left: 20}, Rect{X: 20, W: 80, H: 200}},
		{"all four edges", Insets{Left: 5, Top: 6, Right: 7, Bottom: 8}, Rect{X: 5, Y: 6, W: 88, H: 186}},
		// Insets wider than the surface collapse the area rather than
		// inverting it: a negative extent would send the tree a rectangle no
		// layout can honour.
		{"wider than the surface", Insets{Left: 80, Right: 80}, Rect{X: 80, W: 0, H: 200}},
		{"taller than the surface", Insets{Top: 150, Bottom: 150}, Rect{Y: 150, W: 100, H: 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.Apply(100, 200); got != c.want {
				t.Fatalf("Apply = %+v, want %+v", got, c.want)
			}
		})
	}
	if !(Insets{}).Empty() || (Insets{Top: 1}).Empty() {
		t.Fatal("Empty should report exactly the zero Insets")
	}
}
