// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command gwapp is the go-widgets application half of the Android host demo.
// It is an ordinary CGO-free Go executable: the Java host ships it inside the
// APK, spawns it, and it paints its widget tree into the surface the host owns.
// The scene mirrors go-widgets/window's cmd/windowdemo, so the same tree can be
// compared side by side against the X11, Wayland, Cocoa and Win32 back-ends.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/go-widgets/android"
	"github.com/go-widgets/toolkit"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gwapp:", err)
		os.Exit(1)
	}
}

// run dials the host and drives the widget tree until the Activity goes away.
// Off Android there is no host to dial, so it reports that and succeeds — the
// command cross-builds and runs everywhere, as window's cmd/windowdemo does.
func run() error {
	// Select the touch density before any widget is built: on Android every
	// widget sizes for the finger; off Android this is a no-op and the compact
	// desktop default stands. See density_android.go / density_other.go.
	applyDensity()

	c, err := android.Dial("go-widgets on Android", nil)
	if errors.Is(err, android.ErrUnsupported) {
		fmt.Println("gwapp:", err)
		return nil
	}
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Run(scene(c))
}

// scene composes the demonstration widget tree. The surface line reports what
// the host announced, so a screenshot proves the geometry and density crossed
// the socket rather than being guessed.
//
// The last row exists to demonstrate the touch-density floor, which nothing
// visual can show: toolkit.TouchTarget clamps a control's HIT rectangle up to
// the density minimum and centres it over UNCHANGED pixels. So the row holds a
// deliberately tiny button — far smaller than a fingertip — inside a container
// that leaves it at the size it is given. Tapping beside its pixels, inside its
// hit rect, is the only way to observe the floor at all.
func scene(c *android.Client) toolkit.Widget {
	w, h := c.Size()
	box := toolkit.NewVBox()
	box.Append(toolkit.NewLabel("Hello from go-widgets/window"))
	box.Append(toolkit.NewLabel("Pure-Go Android host — no Xlib, no cgo"))
	box.Append(toolkit.NewLabel(fmt.Sprintf("surface: %dx%d px, density %d", w, h, c.Density())))
	clicks := 0
	label := toolkit.NewLabel("clicks: 0")
	btn := toolkit.NewButton("Click me", func() {
		clicks++
		label.Text = fmt.Sprintf("clicks: %d", clicks)
	})
	box.Append(btn)
	box.Append(label)
	box.Append(newTouchFloorRow())
	return box
}

// tinyPx is the side of the tiny button, in pixels: small enough that the
// density floor must enlarge its hit rect for a finger to have any chance.
const tinyPx = 20

// touchFloorRow is the touch-floor row: a tiny button and the label reporting
// how often it was hit.
//
// It exists because bounds in this toolkit are SURFACE-absolute, so a child
// cannot be placed until its parent knows where it is. Embedding Container
// gives the row its dispatch (which asks each child's HitTest, so the density
// floor applies); SetBounds is what positions the children, relative to
// wherever the enclosing box put the row.
type touchFloorRow struct {
	*toolkit.Container
	tiny   *toolkit.Button
	report *toolkit.Label
}

// SetBounds places the row, then its children inside it.
func (r *touchFloorRow) SetBounds(b toolkit.Rect) {
	r.Container.SetBounds(b)
	y := b.Y + (b.H-tinyPx)/2
	r.tiny.SetBounds(toolkit.Rect{X: b.X + 40, Y: y, W: tinyPx, H: tinyPx})
	r.report.SetBounds(toolkit.Rect{X: b.X + 200, Y: y - 20, W: 600, H: 60})
}

// newTouchFloorRow builds the row.
func newTouchFloorRow() *touchFloorRow {
	hits := 0
	report := toolkit.NewLabel("tiny: 0")
	tiny := toolkit.NewButton("·", func() {
		hits++
		report.Text = fmt.Sprintf("tiny: %d", hits)
	})
	c := toolkit.NewContainer(nil)
	c.AddWidget(tiny)
	c.AddWidget(report)
	return &touchFloorRow{Container: c, tiny: tiny, report: report}
}
