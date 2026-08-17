// Copyright (c) the go-widgets/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.gowidgets.androidhost;

import android.graphics.Rect;
import android.os.Bundle;
import android.view.View;
import android.view.accessibility.AccessibilityEvent;
import android.view.accessibility.AccessibilityNodeInfo;
import android.view.accessibility.AccessibilityNodeProvider;

import java.util.ArrayList;
import java.util.List;

/**
 * Exposes the go-widgets tree to Android's accessibility layer.
 *
 * <p>The application paints pixels, so to a screen reader the SurfaceView is one
 * opaque rectangle with nothing in it. This provider gives that rectangle a
 * virtual view hierarchy: one node per accessible element of the widget tree,
 * each with the class name, text and bounds Android needs, and an activation
 * action that goes back to the application as an ordinary click.
 *
 * <p>The elements are PULLED, never pushed. A provider method is only ever
 * called when something is reading the tree, so an app with no accessibility
 * service attached never builds one — which is also why this cannot repeat
 * go-widgets/window's macOS mistake of rebuilding the whole tree inside the
 * paint loop.
 */
final class GwA11yProvider extends AccessibilityNodeProvider {
    /** One element as the application described it. */
    static final class Element {
        String className, name, value;
        int x, y, w, h;
        boolean clickable;

        /** The text a screen reader reads: the name, then the value if any. */
        String text() {
            if (value == null || value.isEmpty()) {
                return name;
            }
            if (name == null || name.isEmpty()) {
                return value;
            }
            return name + ", " + value;
        }
    }

    private final View view;
    private final GwHostActivity host;
    private List<Element> elements = new ArrayList<>();

    GwA11yProvider(View view, GwHostActivity host) {
        this.view = view;
        this.host = host;
    }

    /** Adopts a freshly received tree. */
    synchronized void setElements(List<Element> els) {
        elements = els;
    }

    private synchronized Element at(int i) {
        return i >= 0 && i < elements.size() ? elements.get(i) : null;
    }

    private synchronized int count() {
        return elements.size();
    }

    @Override
    public AccessibilityNodeInfo createAccessibilityNodeInfo(int virtualViewId) {
        // Asking for the host node is the entry point: make sure the tree the
        // children come from is the one the application has now.
        if (virtualViewId == View.NO_ID) {
            host.requestA11yTree();
            AccessibilityNodeInfo root = AccessibilityNodeInfo.obtain(view);
            view.onInitializeAccessibilityNodeInfo(root);
            for (int i = 0, n = count(); i < n; i++) {
                root.addChild(view, i);
            }
            return root;
        }
        Element e = at(virtualViewId);
        if (e == null) {
            return null;
        }
        AccessibilityNodeInfo node = AccessibilityNodeInfo.obtain(view, virtualViewId);
        node.setPackageName(view.getContext().getPackageName());
        node.setClassName(e.className);
        node.setText(e.text());
        node.setContentDescription(e.text());
        node.setVisibleToUser(true);
        node.setEnabled(true);
        node.setFocusable(true);

        // Android wants SCREEN coordinates; the application reports its own
        // surface, so offset by where the surface actually is.
        int[] origin = new int[2];
        view.getLocationOnScreen(origin);
        node.setBoundsInScreen(new Rect(
                origin[0] + e.x, origin[1] + e.y,
                origin[0] + e.x + e.w, origin[1] + e.y + e.h));

        node.addAction(AccessibilityNodeInfo.ACTION_ACCESSIBILITY_FOCUS);
        node.addAction(AccessibilityNodeInfo.ACTION_CLEAR_ACCESSIBILITY_FOCUS);
        if (e.clickable) {
            node.setClickable(true);
            node.addAction(AccessibilityNodeInfo.ACTION_CLICK);
        }
        node.setParent(view);
        return node;
    }

    @Override
    public boolean performAction(int virtualViewId, int action, Bundle arguments) {
        if (virtualViewId == View.NO_ID) {
            return view.performAccessibilityAction(action, arguments);
        }
        Element e = at(virtualViewId);
        if (e == null) {
            return false;
        }
        switch (action) {
            case AccessibilityNodeInfo.ACTION_CLICK:
                if (!e.clickable) {
                    return false;
                }
                // The application replays it as a click at the element's
                // centre, so an accessibility action goes through the very
                // code an ordinary touch does.
                host.sendA11yAction(virtualViewId);
                return true;
            case AccessibilityNodeInfo.ACTION_ACCESSIBILITY_FOCUS:
                sendEvent(virtualViewId, AccessibilityEvent.TYPE_VIEW_ACCESSIBILITY_FOCUSED);
                return true;
            case AccessibilityNodeInfo.ACTION_CLEAR_ACCESSIBILITY_FOCUS:
                sendEvent(virtualViewId, AccessibilityEvent.TYPE_VIEW_ACCESSIBILITY_FOCUS_CLEARED);
                return true;
            default:
                return false;
        }
    }

    @Override
    public List<AccessibilityNodeInfo> findAccessibilityNodeInfosByText(
            String text, int virtualViewId) {
        List<AccessibilityNodeInfo> found = new ArrayList<>();
        if (text == null) {
            return found;
        }
        String needle = text.toLowerCase();
        for (int i = 0, n = count(); i < n; i++) {
            Element e = at(i);
            if (e != null && e.text() != null && e.text().toLowerCase().contains(needle)) {
                found.add(createAccessibilityNodeInfo(i));
            }
        }
        return found;
    }

    /** Announces a node-level accessibility event to whoever is listening. */
    private void sendEvent(int virtualViewId, int type) {
        AccessibilityEvent ev = AccessibilityEvent.obtain(type);
        ev.setPackageName(view.getContext().getPackageName());
        ev.setSource(view, virtualViewId);
        view.getParent().requestSendAccessibilityEvent(view, ev);
    }
}
