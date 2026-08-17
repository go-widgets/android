// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"errors"
	"slices"
	"testing"

	"github.com/go-widgets/toolkit"
)

func TestAndroidClass(t *testing.T) {
	cases := []struct {
		role toolkit.Role
		want string
	}{
		{toolkit.RoleButton, "android.widget.Button"},
		{toolkit.RoleCheckbox, "android.widget.CheckBox"},
		{toolkit.RoleSlider, "android.widget.SeekBar"},
		{toolkit.RoleImg, "android.widget.ImageView"},
		{toolkit.RoleCombobox, "android.widget.Spinner"},
		{toolkit.RoleGroup, "android.view.ViewGroup"},
		// A role with no more specific mapping is readable content, which is
		// what Android itself calls a TextView.
		{toolkit.RoleText, "android.widget.TextView"},
		{toolkit.RoleAlert, "android.widget.TextView"},
		{toolkit.Role("something-new"), "android.widget.TextView"},
	}
	for _, c := range cases {
		if got := AndroidClass(c.role); got != c.want {
			t.Errorf("AndroidClass(%q) = %q, want %q", c.role, got, c.want)
		}
	}
}

func TestA11yElements(t *testing.T) {
	if got := A11yElements(nil); got != nil {
		t.Fatalf("A11yElements(nil) = %v, want nil", got)
	}
	box := toolkit.NewVBox()
	box.Append(toolkit.NewLabel("Hello"))
	box.Append(toolkit.NewButton("Click me", func() {}))
	box.Append(toolkit.NewLabel("")) // nothing to announce: dropped
	box.SetBounds(toolkit.Rect{W: 200, H: 300})

	els := A11yElements(box)
	names := make([]string, len(els))
	for i, e := range els {
		names[i] = e.Name
	}
	if !slices.Equal(names, []string{"Hello", "Click me"}) {
		t.Fatalf("elements = %q, want the label and the button only", names)
	}
	if els[0].Class != "android.widget.TextView" || els[0].Clickable {
		t.Errorf("label = %+v, want a non-clickable TextView", els[0])
	}
	if els[1].Class != "android.widget.Button" || !els[1].Clickable {
		t.Errorf("button = %+v, want a clickable Button", els[1])
	}
	for _, e := range els {
		if e.W <= 0 || e.H <= 0 {
			t.Errorf("element %q has no area: %+v", e.Name, e)
		}
	}
}

func TestA11yElementsDropsZeroArea(t *testing.T) {
	// A named element with no area cannot be focused, so it is not published:
	// it would be a stop a screen reader user has to swipe past for nothing.
	box := toolkit.NewVBox()
	box.Append(toolkit.NewLabel("invisible"))
	box.SetBounds(toolkit.Rect{})
	if got := A11yElements(box); len(got) != 0 {
		t.Fatalf("elements = %+v, want none from a zero-area tree", got)
	}
}

func TestA11yTreeRoundTrip(t *testing.T) {
	want := []A11yElement{
		{Class: "android.widget.TextView", Name: "Hello", X: 1, Y: 2, W: 3, H: 4},
		{Class: "android.widget.Button", Name: "Click me", Value: "pressed",
			X: 5, Y: 6, W: 7, H: 8, Clickable: true},
		{Class: "android.widget.TextView"}, // every string empty
	}
	got, err := DecodeA11yTree(EncodeA11yTree(want))
	if err != nil {
		t.Fatalf("DecodeA11yTree: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	// An empty tree is legal: it is what an app with nothing to announce says.
	if got, err := DecodeA11yTree(EncodeA11yTree(nil)); err != nil || len(got) != 0 {
		t.Fatalf("empty tree = %+v err=%v", got, err)
	}
}

func TestA11yTreeDecodeErrors(t *testing.T) {
	full := EncodeA11yTree([]A11yElement{{Class: "c", Name: "n", Value: "v", W: 1, H: 1}})
	cases := []struct {
		name string
		body []byte
	}{
		{"no count", full[:3]},
		{"truncated in the first string", full[:6]},
		{"truncated in the second string", full[:11]},
		{"truncated in the third string", full[:16]},
		{"truncated in the coordinates", full[:len(full)-1]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeA11yTree(c.body); !errors.Is(err, ErrShortPayload) {
				t.Fatalf("err = %v, want ErrShortPayload", err)
			}
		})
	}
	// A negative count, and a negative string length, are refused rather than
	// used to slice.
	neg := append([]byte{0xff, 0xff, 0xff, 0xff}, full[4:]...)
	if _, err := DecodeA11yTree(neg); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("negative count err = %v, want ErrShortPayload", err)
	}
	badStr := slices.Clone(full)
	badStr[4] = 0xff
	if _, err := DecodeA11yTree(badStr); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("negative string length err = %v, want ErrShortPayload", err)
	}
}

func TestA11yActionRoundTrip(t *testing.T) {
	i, err := DecodeA11yAction(EncodeA11yAction(7))
	if err != nil || i != 7 {
		t.Fatalf("round trip = %d err=%v, want 7", i, err)
	}
	if _, err := DecodeA11yAction([]byte{1, 2, 3}); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("short err = %v, want ErrShortPayload", err)
	}
}

func TestA11yElementCenter(t *testing.T) {
	x, y := A11yElement{X: 10, Y: 20, W: 100, H: 40}.Center()
	if x != 60 || y != 40 {
		t.Fatalf("Center = (%d,%d), want (60,40)", x, y)
	}
}
