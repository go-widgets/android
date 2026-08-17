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
import android.os.Bundle;
import android.util.DisplayMetrics;
import android.util.Log;
import android.view.KeyEvent;
import android.view.MotionEvent;
import android.view.SurfaceHolder;
import android.view.SurfaceView;
import android.view.WindowManager;

import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.RandomAccessFile;
import java.nio.ByteBuffer;
import java.nio.channels.FileChannel;

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
 * <p>The pixels travel through a file in the app's own storage that both
 * processes map. The Go side writes RGBA_8888, which is byte-for-byte what
 * Android's ARGB_8888 Bitmap holds in memory, so the blit is a copy with no
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
    private static final int MSG_READY = 0x81;
    private static final int MSG_FRAME = 0x82;
    private static final int MSG_TITLE = 0x83;
    private static final int MSG_BYE = 0x84;

    private static final int TOUCH_DOWN = 0, TOUCH_UP = 1, TOUCH_MOVE = 2;
    private static final int KEY_DOWN = 0, KEY_UP = 1;
    private static final int LIFECYCLE_PAUSE = 0, LIFECYCLE_RESUME = 1;

    private SurfaceView view;
    private LocalServerSocket server;
    private LocalSocket peer;
    private DataOutputStream out;
    private Process app;

    private File bufFile;
    private ByteBuffer pixels;
    private Bitmap bitmap;

    private int surfaceW, surfaceH, density;
    private volatile boolean running;

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        view = new SurfaceView(this);
        view.getHolder().addCallback(this);
        setContentView(view);

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
        try (FileInputStream fis = new FileInputStream(bufFile);
             FileChannel ch = fis.getChannel()) {
            pixels = ch.map(FileChannel.MapMode.READ_ONLY, 0, (long) w * h * 4);
        }
        if (bitmap != null) {
            bitmap.recycle();
        }
        bitmap = Bitmap.createBitmap(w, h, Bitmap.Config.ARGB_8888);
    }

    /**
     * Copies the shared framebuffer into the Bitmap and draws the damaged
     * rectangle onto the Surface.
     */
    private synchronized void blit(int x, int y, int w, int h) {
        if (pixels == null || bitmap == null) {
            return;
        }
        SurfaceHolder holder = view.getHolder();
        Rect damage = new Rect(x, y, x + w, y + h);
        Canvas canvas = holder.lockCanvas(damage);
        if (canvas == null) {
            return;
        }
        try {
            pixels.rewind();
            bitmap.copyPixelsFromBuffer(pixels);
            canvas.drawBitmap(bitmap, damage, damage, null);
        } finally {
            holder.unlockCanvasAndPost(canvas);
        }
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

    @Override
    public boolean onTouchEvent(MotionEvent e) {
        int action;
        switch (e.getActionMasked()) {
            case MotionEvent.ACTION_DOWN:
                action = TOUCH_DOWN;
                break;
            case MotionEvent.ACTION_UP:
            case MotionEvent.ACTION_CANCEL:
                action = TOUCH_UP;
                break;
            case MotionEvent.ACTION_MOVE:
                action = TOUCH_MOVE;
                break;
            default:
                return true;
        }
        byte[] body = new byte[13];
        body[0] = (byte) action;
        putInt(body, 1, Math.round(e.getX()));
        putInt(body, 5, Math.round(e.getY()));
        putInt(body, 9, e.getPointerId(e.getActionIndex()));
        send(MSG_TOUCH, body);
        return true;
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
