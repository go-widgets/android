// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.gowidgets.a11yprobe;

import android.accessibilityservice.AccessibilityService;
import android.util.Log;
import android.view.accessibility.AccessibilityEvent;
import android.view.accessibility.AccessibilityNodeInfo;

import java.util.List;

/**
 * A real accessibility service, used as an ON-DEVICE PROBE of the host's
 * accessibility tree.
 *
 * <p>It exists because reading `uiautomator dump` is not the same as being a
 * screen reader. The dump reports a node's `clickable` attribute; what actually
 * decides whether TalkBack offers "double-tap to activate" is the node's ACTION
 * LIST. This service asks the framework for the node the way a screen reader
 * does, reports what it really got, and performs ACTION_CLICK on it — so the
 * activation path can be measured instead of assumed.
 *
 * <p>It is a test instrument, not part of the shipped app: it is built into its
 * own package and installed only when probing.
 */
public final class GwProbeService extends AccessibilityService {
    private static final String TAG = "gw-a11y-probe";
    /** The label of the node to look for and activate. */
    private static final String TARGET = "Click me";

    @Override
    protected void onServiceConnected() {
        Log.i(TAG, "PROBE connected");
    }

    @Override
    public void onAccessibilityEvent(AccessibilityEvent event) {
        // Any event will do as a cue that a window is up; the probe is driven
        // from adb, one shot per broadcast of a window-state change.
        if (event.getEventType() != AccessibilityEvent.TYPE_WINDOW_STATE_CHANGED
                && event.getEventType() != AccessibilityEvent.TYPE_WINDOW_CONTENT_CHANGED) {
            return;
        }
        AccessibilityNodeInfo root = getRootInActiveWindow();
        if (root == null) {
            return;
        }
        List<AccessibilityNodeInfo> found = root.findAccessibilityNodeInfosByText(TARGET);
        if (found == null || found.isEmpty()) {
            return;
        }
        AccessibilityNodeInfo node = found.get(0);
        Log.i(TAG, "PROBE found text=" + node.getText()
                + " class=" + node.getClassName()
                + " clickable=" + node.isClickable()
                + " actions=" + node.getActionList()
                + " bounds=" + boundsOf(node));
        boolean ok = node.performAction(AccessibilityNodeInfo.ACTION_CLICK);
        Log.i(TAG, "PROBE performAction(ACTION_CLICK) returned " + ok);
    }

    private static String boundsOf(AccessibilityNodeInfo n) {
        android.graphics.Rect r = new android.graphics.Rect();
        n.getBoundsInScreen(r);
        return r.toShortString();
    }

    @Override
    public void onInterrupt() {
        // Nothing to interrupt: the probe holds no long-running work.
    }
}
