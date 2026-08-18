// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.gowidgets.androidhost;

import android.app.Activity;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.Rect;
import android.net.LocalServerSocket;
import android.net.LocalSocket;
import android.os.Build;
import android.os.Bundle;
import android.util.DisplayMetrics;
import android.util.Log;
import android.text.InputType;
import android.view.KeyEvent;
import android.view.MotionEvent;
import android.view.SurfaceHolder;
import android.view.SurfaceView;
import android.view.WindowInsets;
import android.view.View;
import android.view.WindowManager;
import android.view.accessibility.AccessibilityNodeProvider;
import android.view.inputmethod.BaseInputConnection;
import android.view.inputmethod.EditorInfo;
import android.view.inputmethod.InputConnection;
import android.view.inputmethod.InputMethodManager;

import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.File;
import java.io.FileDescriptor;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.RandomAccessFile;
import java.nio.ByteBuffer;
import java.nio.channels.FileChannel;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicReference;

/**
 * The Java half of the go-widgets Android host.
 *
 * <p>Android hands no drawable surface to a process that is not the app, so the
 * go-widgets application cannot be the process that owns the window. This
 * Activity owns it instead: it holds the SurfaceView, spawns the CGO-free Go
 * executable shipped in the APK, and then does exactly two things forever —
 * forward input to it, and blit the pixels it paints. Everything above the
 * pixels (layout, widgets, theme, hit-testing, focus) is Go, unchanged from
 * every other back-end.
 *
 * <p>The pixels travel through a memfd the application creates and hands over as
 * an ancillary descriptor on the socket, so they live in memory rather than
 * dirtying page cache the kernel writes to storage; a file in the app's own
 * storage is the fallback. The Go side writes RGBA_8888, which is byte-for-byte
 * what Android's ARGB_8888 Bitmap holds in memory, so the blit is a copy with no
 * conversion.
 */
public final class GwHostActivity extends Activity implements SurfaceHolder.Callback {
    private static final String TAG = "gw-host";

    /** Environment variable naming the abstract socket, matching androidhost.EnvSocket. */
    private static final String ENV_SOCKET = "GW_ANDROID_SOCKET";

    // Message types, matching androidhost/protocol.go.
    private static final int MSG_CONFIG = 0x01;
    private static final int MSG_TOUCH = 0x02;
    private static final int MSG_KEY = 0x03;
    private static final int MSG_LIFECYCLE = 0x04;
    private static final int MSG_CLOSE = 0x05;
    private static final int MSG_INSETS = 0x06;
    private static final int MSG_A11Y_REQUEST = 0x07;
    private static final int MSG_A11Y_ACTION = 0x08;
    private static final int MSG_TEXT = 0x09;
    private static final int MSG_TEXT_DELETE = 0x0a;
    private static final int MSG_SCROLL = 0x0b;
    private static final int MSG_READY = 0x81;
    private static final int MSG_FRAME = 0x82;
    private static final int MSG_TITLE = 0x83;
    private static final int MSG_BYE = 0x84;
    private static final int MSG_A11Y_TREE = 0x85;
    private static final int MSG_KEYBOARD = 0x86;

    private static final int TOUCH_DOWN = 0, TOUCH_UP = 1, TOUCH_MOVE = 2;
    private static final int KEY_DOWN = 0, KEY_UP = 1;
    private static final int LIFECYCLE_PAUSE = 0, LIFECYCLE_RESUME = 1;

    private SurfaceView view;
    private LocalServerSocket server;
    private LocalSocket peer;
    private DataOutputStream out;
    private Process app;

    private File bufFile;
    private ByteBuffer pixels;      // the shared framebuffer, mapped read-only
    private int surfaceStride;      // bytes per framebuffer row
    private ByteBuffer staging;     // contiguous scratch for the damaged rows
    private Bitmap tile;            // upload target, damage-sized

    private int surfaceW, surfaceH, density;
    private int insetL, insetT, insetR, insetB;
    private GwA11yProvider a11y;
    private final AtomicReference<CountDownLatch> a11yArrived = new AtomicReference<>();
    private static final long A11Y_TIMEOUT_MS = 400;
    private volatile boolean running;

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        view = new SurfaceView(this) {
            // onCreateInputConnection belongs to the VIEW, not the Activity: the
            // input method attaches to whatever holds focus.
            @Override
            public InputConnection onCreateInputConnection(EditorInfo out) {
                return newInputConnection(out);
            }

            @Override
            public boolean onCheckIsTextEditor() {
                return true;
            }
        };
        view.getHolder().addCallback(this);
        setContentView(view);

        // From API 35 the window is edge-to-edge: the surface is the whole
        // screen and the bars are painted over it. Ask for the insets so the
        // application can keep its tree out from under them, and re-send them
        // whenever they change — a bar auto-hiding, or later a soft keyboard,
        // moves them without resizing anything.
        view.setOnApplyWindowInsetsListener((v, windowInsets) -> {
            readInsets(windowInsets);
            sendInsets();
            return windowInsets;
        });

        // Give the SurfaceView a virtual view hierarchy: the application paints
        // pixels, so without this a screen reader sees one opaque rectangle.
        a11y = new GwA11yProvider(view, this);
        view.setAccessibilityDelegate(new View.AccessibilityDelegate() {
            @Override
            public AccessibilityNodeProvider getAccessibilityNodeProvider(View host) {
                return a11y;
            }
        });
        view.setImportantForAccessibility(View.IMPORTANT_FOR_ACCESSIBILITY_YES);
        // A view must be focusable in touch mode to hold an input connection.
        view.setFocusable(true);
        view.setFocusableInTouchMode(true);

        DisplayMetrics dm = getResources().getDisplayMetrics();
        density = Math.round(dm.density * 100f);
        bufFile = new File(getCacheDir(), "gw-surface.rgba");
    }

    @Override
    public void surfaceCreated(SurfaceHolder holder) {
        if (running) {
            return;
        }
        running = true;
        try {
            startApp();
        } catch (IOException e) {
            Log.e(TAG, "cannot start the go-widgets application", e);
            finish();
        }
    }

    @Override
    public void surfaceChanged(SurfaceHolder holder, int format, int width, int height) {
        surfaceW = width;
        surfaceH = height;
        sendConfig();
    }

    @Override
    public void surfaceDestroyed(SurfaceHolder holder) {
        send(MSG_LIFECYCLE, new byte[] {(byte) LIFECYCLE_PAUSE});
    }

    /**
     * Spawns the Go executable and waits for it to dial back.
     *
     * <p>The executable ships as {@code lib/arm64-v8a/libgwapp.so} so the
     * installer extracts it into the app's native library directory, the one
     * place an app may execute from. It is a plain PIE executable, not a shared
     * library: nothing ever dlopens it.
     */
    private void startApp() throws IOException {
        String name = "gw-" + android.os.Process.myPid();
        server = new LocalServerSocket(name);

        String exe = getApplicationInfo().nativeLibraryDir + "/libgwapp.so";
        ProcessBuilder pb = new ProcessBuilder(exe);
        pb.environment().put(ENV_SOCKET, name);
        pb.environment().put("TMPDIR", getCacheDir().getAbsolutePath());
        pb.redirectErrorStream(true);
        app = pb.start();
        drainTo(TAG + "-app", app.getInputStream());

        new Thread(this::accept, "gw-host-accept").start();
    }

    /** Accepts the application's connection and pumps its messages until it ends. */
    private void accept() {
        try {
            peer = server.accept();
            out = new DataOutputStream(peer.getOutputStream());
            sendConfig();
            sendInsets();
            pump(new DataInputStream(peer.getInputStream()));
        } catch (IOException e) {
            Log.w(TAG, "host connection ended: " + e);
        } finally {
            runOnUiThread(this::finish);
        }
    }

    /** Reads framed application messages forever. */
    private void pump(DataInputStream in) throws IOException {
        while (true) {
            int len = in.readInt();
            if (len < 1 || len > (1 << 16)) {
                throw new IOException("message length " + len + " out of range");
            }
            int type = in.readUnsignedByte();
            byte[] body = new byte[len - 1];
            in.readFully(body);
            switch (type) {
                case MSG_READY:
                    mapSurface(readInt(body, 0), readInt(body, 4));
                    break;
                case MSG_FRAME:
                    blit(readInt(body, 0), readInt(body, 4), readInt(body, 8), readInt(body, 12));
                    break;
                case MSG_TITLE:
                    final String title = new String(body, "UTF-8");
                    runOnUiThread(() -> setTitle(title));
                    break;
                case MSG_KEYBOARD:
                    setSoftKeyboard(body.length > 0 && body[0] != 0);
                    break;
                case MSG_A11Y_TREE:
                    a11y.setElements(parseA11yTree(body));
                    CountDownLatch waiting = a11yArrived.getAndSet(null);
                    if (waiting != null) {
                        waiting.countDown();
                    }
                    break;
                case MSG_BYE:
                    return;
                default:
                    Log.w(TAG, "unknown message type " + type);
            }
        }
    }

    /**
     * Maps the framebuffer the application announced. The mapping is read-only
     * on this side: the application is the only writer, and a shared file
     * mapping makes its writes visible here with no copy and no message.
     */
    private synchronized void mapSurface(int w, int h) throws IOException {
        long size = (long) w * h * 4;
        // The application hands its framebuffer over as an ancillary descriptor
        // attached to the very message that announced it — a memfd, so the
        // pixels live in memory and never dirty page cache the kernel writes to
        // flash. getAncillaryFileDescriptors() returns the most recent set and
        // then null, so it is read once, here.
        FileDescriptor[] fds = peer.getAncillaryFileDescriptors();
        try (FileInputStream fis = fds != null && fds.length > 0
                ? new FileInputStream(fds[0])
                : new FileInputStream(bufFile);
             FileChannel ch = fis.getChannel()) {
            pixels = ch.map(FileChannel.MapMode.READ_ONLY, 0, size);
            Log.i(TAG, "surface " + w + "x" + h + " mapped from "
                    + (fds != null && fds.length > 0 ? "a shared descriptor" : bufFile));
        }
        surfaceStride = w * 4;
        if (tile != null) {
            tile.recycle();
            tile = null;
        }
        staging = null;
    }

    /**
     * Copies the damaged rectangle out of the shared framebuffer and draws it
     * onto the Surface.
     *
     * <p>Only the damage is touched. The application already says which
     * rectangle changed, so copying the whole surface — 8.4 MB on a 1080×2400
     * panel — to repaint one button was throwing away the one piece of
     * information the protocol carries. The damaged rows are gathered into a
     * contiguous staging buffer (the rows are strided in the framebuffer, and
     * {@code copyPixelsFromBuffer} reads contiguously), then uploaded into a
     * tile Bitmap of exactly that size and drawn at the damage's origin.
     *
     * <p>The tile and the staging buffer are reused across frames and grow
     * only when a larger damage arrives, so a steady repaint allocates
     * nothing. Content outside the dirty rectangle is preserved by
     * {@link SurfaceHolder#lockCanvas(Rect)} itself.
     */
    private synchronized void blit(int x, int y, int w, int h) {
        if (pixels == null || w <= 0 || h <= 0) {
            return;
        }
        SurfaceHolder holder = view.getHolder();
        Rect damage = new Rect(x, y, x + w, y + h);
        Canvas canvas = holder.lockCanvas(damage);
        if (canvas == null) {
            return;
        }
        try {
            gather(x, y, w, h);
            canvas.drawBitmap(tile, x, y, null);
        } finally {
            holder.unlockCanvasAndPost(canvas);
        }
    }

    /**
     * Gathers the w×h rectangle at (x, y) out of the strided framebuffer into
     * the tile Bitmap, growing the tile and the staging buffer only when the
     * damage outgrows them.
     */
    private void gather(int x, int y, int w, int h) {
        int need = w * h * 4;
        if (tile == null || tile.getAllocationByteCount() < need) {
            if (tile != null) {
                tile.recycle();
            }
            tile = Bitmap.createBitmap(w, h, Bitmap.Config.ARGB_8888);
        } else if (tile.getWidth() != w || tile.getHeight() != h) {
            // A reused tile can be bigger than this damage, and a Bitmap upload
            // wants exactly its own pixel count: reshape it in place.
            tile.reconfigure(w, h, Bitmap.Config.ARGB_8888);
        }
        int rowBytes = w * 4;
        int off = y * surfaceStride + x * 4;
        if (rowBytes == surfaceStride) {
            // Full-width damage — including the whole surface, which is what a
            // plain widget tree posts — is ALREADY contiguous in the
            // framebuffer. Uploading it straight from the mapping avoids a
            // second copy through staging: gathering row by row here would make
            // the common case slower than copying everything used to be.
            pixels.limit(off + need).position(off);
            tile.copyPixelsFromBuffer(pixels);
            pixels.clear();
            return;
        }
        if (staging == null || staging.capacity() < need) {
            staging = ByteBuffer.allocateDirect(need);
        }
        staging.clear();
        staging.limit(need);
        for (int row = 0; row < h; row++, off += surfaceStride) {
            pixels.limit(off + rowBytes).position(off);
            staging.put(pixels);
        }
        pixels.clear();
        staging.rewind();
        tile.copyPixelsFromBuffer(staging);
    }

    /**
     * Reads the system-bar, cutout and keyboard insets out of a WindowInsets.
     *
     * <p>{@code WindowInsets.Type} arrived in API 30; below it the deprecated
     * system-window accessors are the only ones there are, and they report the
     * same edges for the bars.
     */
    @SuppressWarnings("deprecation")
    private void readInsets(WindowInsets w) {
        if (Build.VERSION.SDK_INT >= 30) {
            android.graphics.Insets in = w.getInsets(
                    WindowInsets.Type.systemBars()
                            | WindowInsets.Type.displayCutout()
                            | WindowInsets.Type.ime());
            insetL = in.left;
            insetT = in.top;
            insetR = in.right;
            insetB = in.bottom;
            return;
        }
        insetL = w.getSystemWindowInsetLeft();
        insetT = w.getSystemWindowInsetTop();
        insetR = w.getSystemWindowInsetRight();
        insetB = w.getSystemWindowInsetBottom();
    }

    /** Tells the application which edges of its surface the system draws over. */
    private void sendInsets() {
        byte[] body = new byte[16];
        putInt(body, 0, insetL);
        putInt(body, 4, insetT);
        putInt(body, 8, insetR);
        putInt(body, 12, insetB);
        send(MSG_INSETS, body);
    }

    /**
     * Asks the application for its accessibility tree and waits, briefly, for
     * the answer.
     *
     * <p>Called from the provider, i.e. only when something is actually reading
     * the tree: an app with no accessibility service attached never builds one.
     * The wait is what makes a PULL work at all — the answer crosses a socket
     * and arrives on the reader thread, so returning immediately would hand the
     * screen reader the previous tree, or on the first call an empty one.
     * Bounded, because a wedged application must not wedge the accessibility
     * framework with it.
     */
    void requestA11yTree() {
        CountDownLatch arrived = new CountDownLatch(1);
        a11yArrived.set(arrived);
        send(MSG_A11Y_REQUEST, new byte[0]);
        try {
            arrived.await(A11Y_TIMEOUT_MS, TimeUnit.MILLISECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    /** Tells the application a screen reader activated the element at index. */
    void sendA11yAction(int index) {
        byte[] body = new byte[4];
        putInt(body, 0, index);
        send(MSG_A11Y_ACTION, body);
    }

    /** Parses a MsgA11yTree body into the provider's elements. */
    private static List<GwA11yProvider.Element> parseA11yTree(byte[] b) {
        List<GwA11yProvider.Element> els = new ArrayList<>();
        int n = readInt(b, 0), off = 4;
        for (int i = 0; i < n && off + 4 <= b.length; i++) {
            GwA11yProvider.Element e = new GwA11yProvider.Element();
            int[] cursor = {off};
            e.className = readString(b, cursor);
            e.name = readString(b, cursor);
            e.value = readString(b, cursor);
            off = cursor[0];
            if (off + 17 > b.length) {
                break;
            }
            e.x = readInt(b, off);
            e.y = readInt(b, off + 4);
            e.w = readInt(b, off + 8);
            e.h = readInt(b, off + 12);
            e.clickable = b[off + 16] != 0;
            off += 17;
            els.add(e);
        }
        return els;
    }

    /** Reads a length-prefixed string, advancing the cursor past it. */
    private static String readString(byte[] b, int[] cursor) {
        int off = cursor[0];
        if (off + 4 > b.length) {
            cursor[0] = b.length;
            return "";
        }
        int n = readInt(b, off);
        off += 4;
        if (n < 0 || off + n > b.length) {
            cursor[0] = b.length;
            return "";
        }
        cursor[0] = off + n;
        return new String(b, off, n, java.nio.charset.StandardCharsets.UTF_8);
    }

    /** Announces the current geometry and shared-buffer path to the application. */
    private void sendConfig() {
        if (out == null || surfaceW <= 0 || surfaceH <= 0) {
            return;
        }
        byte[] path = bufFile.getAbsolutePath().getBytes();
        byte[] body = new byte[16 + path.length];
        putInt(body, 0, surfaceW);
        putInt(body, 4, surfaceH);
        putInt(body, 8, density);
        putInt(body, 12, path.length);
        System.arraycopy(path, 0, body, 16, path.length);
        send(MSG_CONFIG, body);
    }

    /**
     * Forwards one MotionEvent, every contact of it.
     *
     * <p>Android batches multi-touch: ACTION_POINTER_DOWN and ACTION_POINTER_UP
     * announce the second and later fingers, and a single ACTION_MOVE carries a
     * NEW POSITION FOR EVERY CONTACT AT ONCE. Forwarding only
     * {@code getX()/getY()} — pointer index 0 — meant a second finger never
     * reached the application at all, so the toolkit's MultiTouchRecognizer
     * (pinch, rotate, two-finger pan) could never engage on Android.
     */
    @Override
    public boolean onTouchEvent(MotionEvent e) {
        switch (e.getActionMasked()) {
            case MotionEvent.ACTION_DOWN:
            case MotionEvent.ACTION_POINTER_DOWN:
                sendTouch(TOUCH_DOWN, e, e.getActionIndex());
                return true;
            case MotionEvent.ACTION_UP:
            case MotionEvent.ACTION_POINTER_UP:
                sendTouch(TOUCH_UP, e, e.getActionIndex());
                return true;
            case MotionEvent.ACTION_CANCEL:
                // The gesture was taken away from us: lift every contact, or
                // the application would believe fingers are still down.
                for (int i = 0; i < e.getPointerCount(); i++) {
                    sendTouch(TOUCH_UP, e, i);
                }
                return true;
            case MotionEvent.ACTION_MOVE:
                for (int i = 0; i < e.getPointerCount(); i++) {
                    sendTouch(TOUCH_MOVE, e, i);
                }
                return true;
            default:
                return true;
        }
    }

    /**
     * Forwards a scroll notch from a pointing device.
     *
     * <p>A finger on the glass is never a scroll — it is a drag, and arrives
     * through onTouchEvent. This is the wheel or trackpad a Chromebook, a DeX
     * desktop or a tablet with a mouse has, and it reaches a view through
     * onGenericMotionEvent rather than onTouchEvent. Without it those devices
     * could not scroll a go-widgets surface at all.
     */
    @Override
    public boolean onGenericMotionEvent(MotionEvent e) {
        if (e.getActionMasked() != MotionEvent.ACTION_SCROLL) {
            return super.onGenericMotionEvent(e);
        }
        byte[] body = new byte[16];
        putInt(body, 0, Math.round(e.getX()));
        putInt(body, 4, Math.round(e.getY()));
        putInt(body, 8, Math.round(e.getAxisValue(MotionEvent.AXIS_HSCROLL)));
        putInt(body, 12, Math.round(e.getAxisValue(MotionEvent.AXIS_VSCROLL)));
        send(MSG_SCROLL, body);
        return true;
    }

    /**
     * Gives the surface an input connection, which is how a soft keyboard talks
     * to an application at all.
     *
     * <p>A soft keyboard does not send keystrokes. It commits finished text
     * through an InputConnection — sometimes several characters at once, for a
     * word completion, an emoji or a paste — and spells backspace as "delete n
     * characters before the cursor". Both are forwarded as their own messages;
     * a hardware key still arrives through onKeyDown as before.
     */
    /** Builds the connection the SurfaceView hands the input method. */
    private InputConnection newInputConnection(EditorInfo out) {
        out.inputType = InputType.TYPE_CLASS_TEXT;
        out.imeOptions = EditorInfo.IME_ACTION_DONE | EditorInfo.IME_FLAG_NO_EXTRACT_UI;
        // fullEditor=false: this view keeps no editable buffer of its own, the
        // application owns the text. The connection is a pipe, not a model.
        return new BaseInputConnection(view, false) {
            @Override
            public boolean commitText(CharSequence text, int newCursorPosition) {
                if (text != null && text.length() > 0) {
                    send(MSG_TEXT, text.toString().getBytes(StandardCharsets.UTF_8));
                }
                return true;
            }

            @Override
            public boolean setComposingText(CharSequence text, int newCursorPosition) {
                // Composition is not modelled: the application is told only
                // about committed text, so a half-typed word does not reach the
                // widget tree and then have to be taken back.
                return true;
            }

            @Override
            public boolean deleteSurroundingText(int beforeLength, int afterLength) {
                if (beforeLength > 0) {
                    byte[] body = new byte[4];
                    putInt(body, 0, beforeLength);
                    send(MSG_TEXT_DELETE, body);
                }
                return true;
            }

            @Override
            public boolean sendKeyEvent(KeyEvent event) {
                // Some keyboards send the delete key rather than asking for a
                // deletion; route it through the ordinary key path.
                if (event.getAction() == KeyEvent.ACTION_DOWN) {
                    sendKey(KEY_DOWN, event.getKeyCode(), event);
                } else if (event.getAction() == KeyEvent.ACTION_UP) {
                    sendKey(KEY_UP, event.getKeyCode(), event);
                }
                return true;
            }
        };
    }

    /** Shows or hides the soft keyboard, on the UI thread as the manager requires. */
    private void setSoftKeyboard(boolean show) {
        runOnUiThread(() -> {
            InputMethodManager imm =
                    (InputMethodManager) getSystemService(INPUT_METHOD_SERVICE);
            if (imm == null) {
                return;
            }
            if (show) {
                view.requestFocus();
                imm.showSoftInput(view, InputMethodManager.SHOW_IMPLICIT);
                return;
            }
            imm.hideSoftInputFromWindow(view.getWindowToken(), 0);
        });
    }

    /** Sends one contact of a MotionEvent, named by its POINTER INDEX. */
    private void sendTouch(int action, MotionEvent e, int index) {
        byte[] body = new byte[13];
        body[0] = (byte) action;
        putInt(body, 1, Math.round(e.getX(index)));
        putInt(body, 5, Math.round(e.getY(index)));
        putInt(body, 9, e.getPointerId(index));
        send(MSG_TOUCH, body);
    }

    @Override
    public boolean onKeyDown(int code, KeyEvent e) {
        sendKey(KEY_DOWN, code, e);
        return code != KeyEvent.KEYCODE_BACK || super.onKeyDown(code, e);
    }

    @Override
    public boolean onKeyUp(int code, KeyEvent e) {
        sendKey(KEY_UP, code, e);
        return code != KeyEvent.KEYCODE_BACK || super.onKeyUp(code, e);
    }

    /** Forwards one key event with the character its key-character map produced. */
    private void sendKey(int action, int code, KeyEvent e) {
        byte[] body = new byte[9];
        body[0] = (byte) action;
        putInt(body, 1, code);
        putInt(body, 5, e.getUnicodeChar(e.getMetaState()));
        send(MSG_KEY, body);
    }

    @Override
    protected void onResume() {
        super.onResume();
        send(MSG_LIFECYCLE, new byte[] {(byte) LIFECYCLE_RESUME});
    }

    @Override
    protected void onPause() {
        super.onPause();
        send(MSG_LIFECYCLE, new byte[] {(byte) LIFECYCLE_PAUSE});
    }

    @Override
    protected void onDestroy() {
        super.onDestroy();
        running = false;
        send(MSG_CLOSE, new byte[0]);
        closeQuietly();
    }

    /** Writes one framed message; a dead connection ends the Activity. */
    private synchronized void send(int type, byte[] body) {
        if (out == null) {
            return;
        }
        try {
            out.writeInt(body.length + 1);
            out.writeByte(type);
            out.write(body);
            out.flush();
        } catch (IOException e) {
            Log.w(TAG, "send failed: " + e);
            out = null;
        }
    }

    private void closeQuietly() {
        try {
            if (peer != null) peer.close();
            if (server != null) server.close();
        } catch (IOException ignored) {
            // The Activity is going away; nothing left to report to.
        }
        if (app != null) {
            app.destroy();
        }
    }

    /** Copies a child stream into logcat so the Go side's stderr is visible. */
    private static void drainTo(String tag, java.io.InputStream in) {
        new Thread(() -> {
            try (java.io.BufferedReader r =
                         new java.io.BufferedReader(new java.io.InputStreamReader(in))) {
                for (String line = r.readLine(); line != null; line = r.readLine()) {
                    Log.i(tag, line);
                }
            } catch (IOException ignored) {
                // The child exited; its output ends with it.
            }
        }, "gw-host-drain").start();
    }

    private static void putInt(byte[] b, int off, int v) {
        b[off] = (byte) (v >>> 24);
        b[off + 1] = (byte) (v >>> 16);
        b[off + 2] = (byte) (v >>> 8);
        b[off + 3] = (byte) v;
    }

    private static int readInt(byte[] b, int off) {
        return ((b[off] & 0xff) << 24) | ((b[off + 1] & 0xff) << 16)
                | ((b[off + 2] & 0xff) << 8) | (b[off + 3] & 0xff);
    }
}
