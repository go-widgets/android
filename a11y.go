// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"encoding/binary"

	"github.com/go-widgets/toolkit"
)

// The accessibility half of the protocol.
//
// Android reaches an app's accessibility tree by ASKING: a screen reader drives
// AccessibilityNodeProvider, which the host implements, and the host then needs
// the elements of a tree it does not own. So this is a PULL — the host requests
// the tree, the application answers — and not a publication on every frame.
//
// That is deliberate, and it is the lesson go-widgets/window paid for on macOS:
// rebuilding and republishing the whole accessibility tree from inside the paint
// loop turned a scrolling window into a frozen machine. Pulling cannot do that:
// a tree is built only when something is actually reading it, and nothing reads
// it when no accessibility service is running.

// A11yElement is one element of the accessibility tree as the host sees it: a
// role it can turn into an Android class name, the text a screen reader reads,
// and the rectangle to focus, in surface pixels.
type A11yElement struct {
	// Class is the android.widget.* class name a screen reader expects for
	// this kind of element. Android has no notion of an ARIA role; it decides
	// almost everything from the class name of the node.
	Class string
	// Name is what a screen reader announces.
	Name string
	// Value is the element's current value, appended after the name for a
	// control that has one (a text field's content, a slider's reading).
	Value string
	// X, Y, W, H is the element's rectangle in surface pixels.
	X, Y, W, H int
	// Clickable reports whether activating the element does something, i.e.
	// whether the host should offer "double-tap to activate".
	Clickable bool
}

// androidClasses maps a toolkit role to the Android class name a screen reader
// expects. Android decides how to announce a node almost entirely from its
// class name — "Button" for a button, "double-tap to toggle" for a
// CompoundButton — so this mapping is what makes a go-widgets tree sound like
// an Android app rather than a wall of unlabelled views. It is this back-end's
// counterpart to internal/cocoa's AXRole and internal/atspi's role constants.
var androidClasses = map[toolkit.Role]string{
	toolkit.RoleButton:      "android.widget.Button",
	toolkit.RoleCheckbox:    "android.widget.CheckBox",
	toolkit.RoleRadio:       "android.widget.RadioButton",
	toolkit.RoleSwitch:      "android.widget.Switch",
	toolkit.RoleTextbox:     "android.widget.EditText",
	toolkit.RoleSearchbox:   "android.widget.EditText",
	toolkit.RoleSpinbutton:  "android.widget.EditText",
	toolkit.RoleCombobox:    "android.widget.Spinner",
	toolkit.RoleSlider:      "android.widget.SeekBar",
	toolkit.RoleProgressbar: "android.widget.ProgressBar",
	toolkit.RoleMeter:       "android.widget.ProgressBar",
	toolkit.RoleImg:         "android.widget.ImageView",
	toolkit.RoleList:        "android.widget.ListView",
	toolkit.RoleListbox:     "android.widget.ListView",
	toolkit.RoleTree:        "android.widget.ListView",
	toolkit.RoleGrid:        "android.widget.GridView",
	toolkit.RoleTablist:     "android.widget.TabWidget",
	toolkit.RoleToolbar:     "android.widget.Toolbar",
	toolkit.RoleDialog:      "android.view.ViewGroup",
	toolkit.RoleGroup:       "android.view.ViewGroup",
	toolkit.RoleNavigation:  "android.view.ViewGroup",
	toolkit.RoleBanner:      "android.view.ViewGroup",
	toolkit.RoleMenu:        "android.view.ViewGroup",
	toolkit.RoleMenuBar:     "android.view.ViewGroup",
	toolkit.RoleDocument:    "android.view.ViewGroup",
}

// clickableRoles are the roles whose elements do something when activated, so
// the host offers the screen reader's activation gesture on them and not on a
// label.
var clickableRoles = map[toolkit.Role]bool{
	toolkit.RoleButton:     true,
	toolkit.RoleCheckbox:   true,
	toolkit.RoleRadio:      true,
	toolkit.RoleSwitch:     true,
	toolkit.RoleCombobox:   true,
	toolkit.RoleTextbox:    true,
	toolkit.RoleSearchbox:  true,
	toolkit.RoleSpinbutton: true,
}

// AndroidClass returns the Android class name for a toolkit role. Anything with
// no more specific mapping is a TextView, which is what Android itself uses for
// a piece of readable content.
func AndroidClass(r toolkit.Role) string {
	if c, ok := androidClasses[r]; ok {
		return c
	}
	return "android.widget.TextView"
}

// A11yElements turns the widget tree into the flat element list the host
// serves. Elements with nothing to announce are dropped: an unnamed, valueless
// element would reach a screen reader as an anonymous stop the user has to swipe
// past, and a zero-area one cannot be focused at all.
func A11yElements(root toolkit.Widget) []A11yElement {
	if root == nil {
		return nil
	}
	nodes := toolkit.WalkA11y(root)
	out := make([]A11yElement, 0, len(nodes))
	for _, n := range nodes {
		if n.Rect.W <= 0 || n.Rect.H <= 0 {
			continue
		}
		if n.Name == "" && n.Value == "" {
			continue
		}
		out = append(out, A11yElement{
			Class:     AndroidClass(n.Role),
			Name:      n.Name,
			Value:     n.Value,
			X:         n.Rect.X,
			Y:         n.Rect.Y,
			W:         n.Rect.W,
			H:         n.Rect.H,
			Clickable: clickableRoles[n.Role],
		})
	}
	return out
}

// EncodeA11yTree builds a MsgA11yTree body: a count, then each element as
// three length-prefixed strings, four coordinates and a flag.
func EncodeA11yTree(els []A11yElement) []byte {
	b := appendInt32(nil, len(els))
	for _, e := range els {
		b = appendString(b, e.Class)
		b = appendString(b, e.Name)
		b = appendString(b, e.Value)
		b = appendInt32(b, e.X)
		b = appendInt32(b, e.Y)
		b = appendInt32(b, e.W)
		b = appendInt32(b, e.H)
		if e.Clickable {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
	}
	return b
}

// DecodeA11yTree parses a MsgA11yTree body. It exists for the round-trip tests
// and for any host written in Go; the shipped host is the Java one.
func DecodeA11yTree(b []byte) ([]A11yElement, error) {
	if len(b) < 4 {
		return nil, ErrShortPayload
	}
	n := int32At(b, 0)
	if n < 0 {
		return nil, ErrShortPayload
	}
	b = b[4:]
	els := make([]A11yElement, 0, min(n, 1024))
	for i := 0; i < n; i++ {
		var e A11yElement
		var err error
		if e.Class, b, err = takeString(b); err != nil {
			return nil, err
		}
		if e.Name, b, err = takeString(b); err != nil {
			return nil, err
		}
		if e.Value, b, err = takeString(b); err != nil {
			return nil, err
		}
		if len(b) < 17 {
			return nil, ErrShortPayload
		}
		e.X, e.Y, e.W, e.H = int32At(b, 0), int32At(b, 4), int32At(b, 8), int32At(b, 12)
		e.Clickable = b[16] != 0
		b = b[17:]
		els = append(els, e)
	}
	return els, nil
}

// DecodeA11yAction parses a MsgA11yAction body: the index of the element a
// screen reader activated.
func DecodeA11yAction(b []byte) (int, error) {
	if len(b) < 4 {
		return 0, ErrShortPayload
	}
	return int32At(b, 0), nil
}

// EncodeA11yAction builds a MsgA11yAction body.
func EncodeA11yAction(index int) []byte { return appendInt32(nil, index) }

// Center returns the point to replay an activation at: the middle of the
// element. A screen reader's activation becomes an ordinary click there, so
// every behaviour a click has is had by an accessibility action, with no second
// code path to drift from the first — the rule the AT-SPI bridge already
// follows.
func (e A11yElement) Center() (int, int) { return e.X + e.W/2, e.Y + e.H/2 }

// appendString appends a 4-byte length followed by the bytes of s.
func appendString(b []byte, s string) []byte {
	b = appendInt32(b, len(s))
	return append(b, s...)
}

// takeString reads a length-prefixed string, returning it and the rest.
func takeString(b []byte) (string, []byte, error) {
	if len(b) < 4 {
		return "", nil, ErrShortPayload
	}
	n := int(int32(binary.BigEndian.Uint32(b)))
	if n < 0 || 4+n > len(b) {
		return "", nil, ErrShortPayload
	}
	return string(b[4 : 4+n]), b[4+n:], nil
}
