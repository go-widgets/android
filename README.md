# go-widgets/android

[![ci](https://github.com/go-widgets/android/actions/workflows/ci.yml/badge.svg)](https://github.com/go-widgets/android/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-widgets/android.svg)](https://pkg.go.dev/github.com/go-widgets/android)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-widgets/android)](https://goreportcard.com/report/github.com/go-widgets/android)
[![coverage 100%](https://img.shields.io/badge/coverage-100%25-brightgreen)](#testing)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD_3--Clause-blue.svg)](LICENSE)

A **pure-Go, CGO-free** Android back-end for the
[go-widgets](https://github.com/go-widgets) toolkit: a real, installable
Android app whose entire user interface is laid out and painted by a Go
process built with `CGO_ENABLED=0`.

![a go-widgets tree running as an Android app](docs/apk-portrait.png)

## Why it is split in two

Android hands no drawable surface to a process that is not the app. Every path
to one — `ANativeWindow`, `Surface`, `NativeActivity` — is behind JNI, and JNI
needs cgo. `purego` does not rescue it either: its `dlfcn_android.go` routes
`Dlopen` through `internal/cgo`, unlike its darwin and non-cgo linux paths. And
there is no wire-protocol back door the way X11 and Wayland have one:
SurfaceFlinger sits behind Binder, and a `Surface` only ever comes from the
WindowManager against an Activity token.

So the app is two processes:

| | |
|---|---|
| **Java host** (`host/`, ~360 lines) | owns the Activity, the `SurfaceView`, touch, keys and the lifecycle. Blits pixels. Knows nothing about widgets. |
| **Go application** (`cmd/gwapp`) | an ordinary `CGO_ENABLED=0 GOOS=android` executable. Owns layout, widgets, theme, hit-testing and focus — unchanged from every other back-end. |

This is the split the Linux back-ends already live with — a socket protocol
plus a shared pixel buffer — with the Java host standing exactly where the X
server or the Wayland compositor stands.

```
  MotionEvent ─► LocalSocket ─► android.Client ─► toolkit widget tree
                                      │
  Surface ◄── Bitmap ◄── mmap'd file ◄─┘  painter.PixelPainter
                    ▲
                    └── MsgFrame{x,y,w,h}: which rectangle changed
```

Pixels travel through a **memfd** the application creates and hands to the host
as an ancillary descriptor on the socket, so they live in memory and never
dirty page cache the kernel writes to storage. (A file in the app's own storage
is the fallback where `memfd_create` is missing.) The Go side writes RGBA_8888,
which is byte-for-byte what Android's ARGB_8888 `Bitmap` holds in memory, so
the blit is a copy with no conversion — and only the damaged rectangle is
copied, measured on an Android 15 arm64 device:

| damage on a 1080×2400 surface | whole-surface copy | damage-only copy |
|---|---|---|
| 400×300 (a widget) | 883 µs | **328 µs** |
| full surface (a plain tree) | 3335 µs | **1957 µs** |

(median of 41 and 21 blits; the full-surface case gets faster too because the
gathered tile is drawn with an offset blit rather than a src/dst rect one.)

The memfd is worth the same kind of measurement — `/proc/meminfo` Dirty, idle
versus painting, same session and same taps:

| framebuffer | idle | painting | delta |
|---|---|---|---|
| file in app storage | 192 kB | 10188 kB | **+9996 kB** |
| memfd | 188 kB | 140 kB | **−48 kB** |

Ten megabytes of dirty page cache per painting session, written out to flash
half a minute later, for pixels that are pure scratch — gone.

## arm64 only, and why

`android/arm64` is the only Android target Go links **CGO-free**:

```console
$ CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build ./cmd/gwapp   # fine
$ CGO_ENABLED=0 GOOS=android GOARCH=arm   go build ./cmd/gwapp
android/arm requires external (cgo) linking, but cgo is not enabled
```

`android/amd64` and `android/386` answer the same. So the premise this back-end
rests on — a sovereign application binary with no C tool chain — holds on
64-bit ARM alone, which is every Android phone and tablet shipped for years,
but not the x86 emulator images. CI asserts both halves of that, so the day Go
lifts the restriction is a red build rather than a silent one.

## Usage

```go
c, err := android.Dial("my app", nil) // nil theme = toolkit.DefaultDark()
if errors.Is(err, android.ErrUnsupported) {
    // Not running under a host: the module still builds and vets everywhere.
    return nil
}
defer c.Close()
return c.Run(myWidgetTree()) // blocks until the Activity goes away
```

`Client` satisfies go-widgets/window's `Backend` (`Run`/`Close`/`Size`/
`String`) and its `Repainter`, so an application moves between this back-end
and X11, Wayland, Cocoa, Win32 or wasmbox without changing a line above the
window.

### Accessibility

The application paints pixels, so without help a screen reader sees the
`SurfaceView` as one opaque rectangle. The host gives it a virtual view
hierarchy instead: one node per accessible element of the widget tree, with the
`android.widget.*` class name Android decides its announcements from, the text
to read, screen bounds and an activation action.

The elements are **pulled, never pushed**. A provider method is only ever called
when something is reading the tree, so an app with no accessibility service
attached never builds one — which is also what keeps this from repeating
go-widgets/window's macOS mistake of rebuilding the whole tree inside the paint
loop and freezing the machine.

Measured, again with process CPU over ten-second windows: **0 ticks** with nobody
reading, and **0 ticks** across ten full reads — because one read is far below
what a 100 Hz counter can see. `BenchmarkA11yElements`, run on the device, says
what it actually costs:

```
BenchmarkA11yElements-4   237154   10052 ns/op   20944 B/op   9 allocs/op
```

Ten microseconds for a 41-widget tree. That is the number behind "too cheap to
measure", and it is why pulling on demand costs nothing worth avoiding.

An activation comes back as an ordinary click at the element's centre, so an
accessibility action goes through the very code a touch does, with no second
path to drift from the first — the rule the AT-SPI bridge already follows.

### Touch

Each pointer sample reaches the widget tree as **two** events: the touch event
first, then a mouse event.

go-widgets models touch directly — `EventTouchStart`/`Move`/`End` carry a
pointer id in `Event.Code`, and `toolkit.GestureRecognizer` turns them into
taps, long presses and swipes. A back-end emitting only mouse events would
leave every gesture-aware widget deaf on the one kind of device gestures are
for. The compatibility mouse event follows because most widgets listen for
`EventClick`; a browser does exactly this, for exactly this reason.

### Animations, and the frame loop that drives them

The toolkit's animated widgets — a spinner, a progress bar, a skeleton, a
coasting scroll view — advance on a host's frame tick: `toolkit.TickTree` walks
the tree calling `Tick(dt)`, and `toolkit.TreeAnimating` reports whether
anything still wants frames. **This back-end had no such loop**, so it painted
only in response to input: a released drag never coasted and every animated
widget was frozen.

The loop is demand-driven. It starts when something begins animating and stops
the moment nothing is, which is exactly what `TreeAnimating` exists to allow.
That is measured rather than asserted — CPU time of the application process,
from `/proc/<pid>/stat`, over ten-second windows:

| window | CPU ticks |
|---|---|
| ten seconds idle, untouched | **0** |
| a fling, then ten seconds | 30 |
| ten seconds after it settled | **0** |

So the loop really does not run when nothing is animating, and really does stop
afterwards. (The 30 ticks — 300 ms of CPU — cover the gesture and the coast on
a software-rendered emulator, where every frame is a full-surface repaint and
blit; a device with a GPU-composited surface does less.)

The widget tree is not goroutine-safe and two goroutines now reach it, one
dispatching input and one ticking, so they are serialised on the same lock the
paint holds. Socket writes take a **separate** lock: a write blocks when the
host is slower than the frames being posted, and holding the state lock across
that would stall the reader that dispatches input.

Measured on device, with a scrollable region of 40 rows (the accessibility tree
reports each row shifted by `ScrollView.ChildOffset`, so "where is row 00" *is*
the scroll position, readable from outside the process):

| gesture | content moved |
|---|---|
| 120 px drag over 1200 ms | 188 px |
| the same 120 px in 120 ms | **796 px** |

Same finger distance, four times the travel: the difference is the coast. A pull
past the top springs back to exactly the top — the rubber band itself is too
brief to catch with a screen dump, which takes hundreds of milliseconds.

### Wheels and trackpads

A finger on the glass is never a scroll — it is a drag, and arrives as touch.
But a Chromebook, a DeX desktop or a tablet with a mouse has a real pointing
device, and its notches reach a view through `onGenericMotionEvent`, not
`onTouchEvent`. Those are forwarded too, on both axes.

Android reports a wheel detent as `+1` for UP, the opposite of the toolkit's
convention where a positive `Delta` scrolls toward the END of the content, so
the vertical axis is negated. The horizontal one is not: `AXIS_HSCROLL` is
already positive to the right, which is what `Event.DeltaX` means. A notch with
no movement on either axis produces nothing at all, so a device reporting an
idle scroll wakes no widget.

Measured on device: `input mouse scroll ... --axis VSCROLL,1` and
`--axis HSCROLL,-1` both reach the host with the axis values intact.

### The soft keyboard

Only the host can raise a keyboard — the keyboard is a window, and an
application that owns no windows cannot raise one — so an application asks with
`Client.SetSoftKeyboard(true)`.

What comes back is **not keystrokes**. A soft keyboard commits finished text
through an `InputConnection`, sometimes several characters at once (a word
completion, an emoji, a paste), and spells backspace as "delete n characters
before the cursor". This back-end turns each committed rune into the
`EventKeyDown` + `EventChar` pair a printable key produces, and each deletion
into that many `Backspace` key-downs — which is exactly what the X11 and wasmbox
back-ends emit. **Every toolkit text widget therefore works with no
Android-specific code**: the demo's `Entry` has none.

Composition is deliberately not modelled: the application is told only about
committed text, so a half-typed word never reaches the widget tree and then has
to be taken back.

Measured on device, with LatinIME:

```
tap "keyboard"  → dumpsys input_method: mInputShown=true, mIsInputViewShown=true
type "hello"    → the Entry reports text="hello" class="android.widget.EditText"
backspace ×2    → text="hel"
```

### System bars

An Android window is edge-to-edge from API 35: the surface really is the whole
screen, and the status bar, the navigation bar, a display cutout and the soft
keyboard are painted **on top of it** rather than shrinking it. The tree is
therefore laid out inside what they leave, so its first and last rows are not
hidden; the margins are still painted in the theme background, so the bars sit
on the app's own colour.

`Client.Insets()` reports those four edges, and `Client.SetFullBleed(true)`
opts back out to the whole surface — for a root that means to reach under the
bars (a photo, a map, a video) and takes responsibility for keeping anything
readable out of the way.

## Layout

    protocol.go       the sovereign codec — wire messages, framing, and the
                      input→toolkit.Event mapping. No syscall, no net: it
                      builds, and is tested, on every GOOS.
    client.go         the transport — dials the host, maps the framebuffer,
                      drives the widget tree. //go:build linux
    client_other.go   the same surface reporting ErrUnsupported, so an
                      application still cross-builds off Android.
    cmd/gwapp/        the demo application.
    host/             the Java host, its manifest, and build.sh.

## Building the APK

No Gradle and no Kotlin: the host is a handful of Java files and the
application is a Go binary, so the SDK's own tools are the whole tool chain.

```sh
export ANDROID_HOME=... JAVA_HOME=...
sdkmanager --install "platforms;android-35" "build-tools;35.0.0"
host/build.sh                        # → host/out/gwhost.apk
adb install host/out/gwhost.apk
```

`build.sh` runs `go build`, `javac`, `d8`, `aapt2 link`, `zipalign` and
`apksigner`, in that order, and picks the newest platform and build-tools the
SDK has installed. `APP=./cmd/myapp host/build.sh` packages your own
application instead of the demo. Two things it does that are worth knowing:

- the Go executable ships as `lib/<abi>/libgwapp.so` with
  `extractNativeLibs="true"`, because `nativeLibraryDir` is the one place an
  Android app may execute from. It is a plain PIE executable; nothing ever
  `dlopen`s it;
- the debug keystore lives beside the sources, never under the build output. A
  fresh key per build changes the signing certificate, and Android then refuses
  to update an installed app (`INSTALL_FAILED_UPDATE_INCOMPATIBLE`).

### Packaging an application from another module

`APP=./cmd/myapp` builds a package of *this* repo. An application living in its
own module is packaged from a binary it built itself, since a build script
cannot reach across a module boundary:

```sh
GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/myapp ./cmd/myapp

APP_BIN=/tmp/myapp \
PACKAGE=org.example.myapp \
LABEL="My App" \
APK=myapp \
ARGS="-window -sub reddit:golang" \
PERMISSIONS="android.permission.INTERNET android.permission.ACCESS_NETWORK_STATE" \
host/build.sh
```

| Variable | Effect |
| --- | --- |
| `APP_BIN` | package this pre-built binary instead of running `go build` |
| `PACKAGE` | application id, so it coexists with the demo and with other packaged apps |
| `LABEL` | launcher name |
| `APK` | output file name |
| `ARGS` | arguments handed to the application (manifest `meta-data`, split on whitespace) |
| `PERMISSIONS` | `uses-permission` names; **empty by default**, so no app asks for more than it uses |

The host owns the `Activity` and the surface and does not care which Go program
it spawns, so any CGO-free go-widgets binary is a valid payload.

### What the host hands the application

Android gives a spawned process almost nothing a Unix program expects, and two
of those gaps stop real applications dead. Both are the host's to fill, and it
fills them:

- **`HOME`** (and `XDG_CACHE_HOME`, `TMPDIR`) — an app process inherits no home
  directory, so `os.UserConfigDir` and `os.UserHomeDir` both fail outright. The
  first real application packaged this way died on exactly that, before drawing
  a pixel. `HOME` is the app's `getFilesDir()`, which is persistent; the cache
  variables point at `getCacheDir()`, which is what the system reclaims under
  pressure — so caches land where Android expects to be able to delete them.
- **`GW_ANDROID_DNS`** — Android has **no `/etc/resolv.conf`**, and its resolver
  lives behind libc, which is behind cgo. A CGO-free binary therefore cannot
  resolve a single name: Go finds no nameserver, falls back to localhost and
  every lookup dies with a connection refused. The host reads the active
  network's servers through `ConnectivityManager` and passes them here, and
  `Dial` installs a `net.Resolver` that talks to them directly. Reading them
  needs `ACCESS_NETWORK_STATE`, so an application that does not want the network
  is never made to ask for it — it simply gets no resolver.

## Testing

The transport is Linux, and Android *is* Linux: the abstract socket it dials
and the shared mapping it paints into are ordinary Linux facilities. So the
suite runs against a **fake host over a real socket and a real mmap**, not a
mock — on the CI Linux runner under `-race`, and on the device itself:

```sh
go test -c -cover -coverpkg=. -o android.test .
adb push android.test /data/local/tmp/ && adb shell /data/local/tmp/android.test
```

`100.0%` statement coverage, gated in CI, covering every decode error, every
framebuffer failure, the lifecycle pause and the damage-rectangle path.
`-race` runs on the Linux lane only: the race detector needs cgo, which is the
very thing an Android application binary must not have.

## Proven on device

Android 15 / arm64 unless stated otherwise:

- **a real application runs**, not only the demo: `go-news-reader/reader`,
  packaged with `APP_BIN`, draws its own interface on the device — its
  accessibility tree reports `News`, `Toggle sidebar`, `All Sources`,
  `Settings`. Getting there is what found the two environment gaps above;
- **the declared floor is measured, not asserted.** `minSdkVersion` claims 26,
  so the suite is not evidence unless something actually runs there: on
  **Android 8.0 / API 26 / arm64** the app launches, the Go process comes up,
  the accessibility tree is complete (54 nodes, the same widgets as on 15) and
  two taps on the button leave `clicks: 1` then `2`;

- the app installs and launches; the whole window is the go-widgets tree;
- a touch reaches the widget — three taps on the button leave `clicks: 3`, and
  a pixel diff bounds the repaint to the button and its label alone;
- a live rotation is survived in-process: the Activity keeps `configChanges`,
  the surface is remapped at 2400×1080, the tree is laid out again, and taps
  still land ([landscape](docs/apk-landscape.png));
- the surface geometry and display density cross the socket — the demo reports
  `surface: 1080x2400 px, density 263`, the panel's true 2.625×;
- the accessibility tree is real: `adb shell uiautomator dump`, which reads
  through the same framework a screen reader does, sees a virtual node per
  element — `android.widget.TextView` for each label, `android.widget.Button`
  for each button — each with its text and screen bounds;
- **the touch-density floor works end to end.** The demo's last row holds a
  deliberately tiny 20x20 button, because nothing visual can show this axis:
  `toolkit.TouchTarget` clamps a control's HIT rectangle up to the density
  minimum and centres it over UNCHANGED pixels. With the button at
  `[40,2085][60,2105]` and its hit rect at `[28,2073][72,2117]`, a tap at
  `(10,2095)` — outside both — does nothing, and a tap at `(30,2095)` — outside
  the pixels, inside the hit rect — activates it. That is a fingertip landing
  beside a 20-pixel target and still hitting it;
- the system bars do not hide anything: with the device reporting
  `statusBars top=128` and `navigationBars bottom=126`, the tree's first text
  row moves from y=234 to y=337 and its last from y=2160 to y=2063 — the +103
  and −97 a five-child box redistributed over the safe area gives, to the pixel
  ([without insets](docs/apk-portrait.png) vs [with](docs/apk-insets.png)).

## Known gaps

Deliberate, and none of them protocol-deep:

- **single touch** — the protocol carries a pointer id, the host forwards one.
  Multi-touch, fling and inertial scroll are toolkit work, not host work;

### The accessibility path, measured with a real client

`adb shell uiautomator dump` reports `clickable="false"` on the button. That
attribute is an artefact of the dump, not a property of the node: asked the way
a screen reader asks, the framework returns something else entirely.

`host/probe/` is a real `AccessibilityService` — a test instrument, in its own
package, never part of the shipped app — that queries the node and activates it.
Against the live demo it reports:

```
PROBE found text=Click me class=android.widget.Button clickable=true
      actions=[ACTION_CLICK, ACTION_ACCESSIBILITY_FOCUS, ACTION_CLEAR_ACCESSIBILITY_FOCUS]
      bounds=[0,1418][1080,1844]
PROBE performAction(ACTION_CLICK) returned true
PROBE found text=Click me, pressed ...
```

So the node a screen reader sees IS clickable, it DOES carry `ACTION_CLICK`, and
performing that action reaches the Go widget: two activations left the demo
reading `clicks: 2`, and the button's own value changed to `pressed` between the
two reads. What TalkBack decides "double-tap to activate" from is the action
list, which is present.

Run it with:

```sh
host/probe/build.sh && adb install -r host/probe/out/gwprobe.apk
adb shell settings put secure enabled_accessibility_services \
    org.gowidgets.a11yprobe/org.gowidgets.a11yprobe.GwProbeService
adb shell settings put secure accessibility_enabled 1
adb logcat -s gw-a11y-probe
```
#### What is and is not proven about multi-touch

The application half is proven deterministically and on the device: a real
`toolkit.MultiTouchRecognizer`, fed this back-end's own output for two contacts,
engages and reports a pinch out and a pinch in.

The **host** half — forwarding every contact of a `MotionEvent` — is not proven
on a device. `adb shell input` injects a single pointer, and kernel-level
multi-touch injection through `/dev/input` does not reach the app on this
emulator, because `input` uses Android's injection API rather than the input
devices. A single contact is verified end to end; the second-contact path is
reviewed code, not measured behaviour, until it runs on real hardware.

## License

BSD-3-Clause. See [LICENSE](LICENSE).

