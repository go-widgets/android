#!/bin/sh
# Copyright (c) the go-widgets/android authors. All rights reserved.
# SPDX-License-Identifier: BSD-3-Clause
#
# Builds the demo APK without Gradle: the host is a handful of Java files and
# the application is a Go binary, so the SDK's own tools (aapt2, javac, d8,
# zipalign, apksigner) are the whole tool chain.
#
# Requires ANDROID_HOME and JAVA_HOME. Both come from pkgx:
#   pkgx +android.com/cmdline-tools +openjdk.org
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
out=${OUT:-$here/out}
# A pre-built application binary to package instead of building this repo's demo.
# It must be a CGO-free android binary for $abi -- see the APP_BIN note below.
appbin=${APP_BIN:-}
# Package id, launcher label and output name. Overriding the id is what lets an
# application coexist on a device with the demo, and with other applications
# packaged by this same script.
pkgid=${PACKAGE:-org.gowidgets.androidhost}
label=${LABEL:-go-widgets}
apkname=${APK:-gwhost}
# Arguments handed to the packaged application, substituted into the manifest.
appargs=${ARGS:-}
# Space-separated permission names the packaged application needs, e.g.
# "android.permission.INTERNET". Empty by default: least privilege is the point.
appperms=${PERMISSIONS:-}
abi=${ABI:-arm64-v8a}
goarch=${GOARCH:-arm64}

: "${ANDROID_HOME:?set ANDROID_HOME}"
: "${JAVA_HOME:?set JAVA_HOME}"

# Pin nothing the SDK can be asked: a CI image and a workstation rarely carry
# the same platform and build-tools revisions, and the newest installed is the
# right answer on both.
api=${API:-$(ls "$ANDROID_HOME/platforms" | sed -n 's/^android-\([0-9]\{1,\}\)$/\1/p' | sort -n | tail -1)}
buildtools=${BUILD_TOOLS:-$(ls "$ANDROID_HOME/build-tools" | sort -V | tail -1)}
echo "==> SDK: platform android-$api, build-tools $buildtools"

bt="$ANDROID_HOME/build-tools/$buildtools"
androidjar="$ANDROID_HOME/platforms/android-$api/android.jar"
PATH="$JAVA_HOME/bin:$bt:$PATH"
export PATH

rm -rf "$out"
mkdir -p "$out/classes" "$out/lib/$abi"

if [ -n "$appbin" ]; then
    # $APP_BIN packages an application from ANOTHER module. The host does not
    # care which Go program it spawns -- it owns the Activity and the surface,
    # and the application owns the widgets -- so any CGO-free go-widgets binary
    # is a valid payload. Building it is that module's business, not this
    # script's, which cannot reach across a module boundary to do it.
    echo "==> packaging pre-built $appbin"
    cp "$appbin" "$out/lib/$abi/libgwapp.so"
else
    echo "==> go build (CGO_ENABLED=0 GOOS=android GOARCH=$goarch)"
    (cd "$root" && GOWORK=off CGO_ENABLED=0 GOOS=android GOARCH="$goarch" \
        go build -trimpath -ldflags=-s -o "$out/lib/$abi/libgwapp.so" ${APP:-./cmd/gwapp})
fi

echo "==> javac"
find "$here/java" -name '*.java' > "$out/sources.txt"
javac -source 17 -target 17 -nowarn -classpath "$androidjar" \
    -d "$out/classes" @"$out/sources.txt"

echo "==> d8"
find "$out/classes" -name '*.class' > "$out/classes.txt"
d8 --lib "$androidjar" --min-api 26 --output "$out" @"$out/classes.txt"

echo "==> aapt2 link"
# The package id and the launcher label are substituted into a copy rather than
# passed as flags: aapt2 can rename a package but has no say over the label, and
# an application packaged here should be able to name itself. The copy lives
# under $out so the checked-in manifest stays the demo's.
permxml=""
for p in $appperms; do
    permxml="$permxml<uses-permission android:name=\"$p\" />"
done

sed -e "s|<!-- GW_PERMISSIONS -->|$permxml|" \
    -e "s|package=\"org.gowidgets.androidhost\"|package=\"$pkgid\"|" \
    -e "s|android:label=\"go-widgets\"|android:label=\"$label\"|" \
    -e "s|android:name=\"org.gowidgets.args\" android:value=\"\"|android:name=\"org.gowidgets.args\" android:value=\"$appargs\"|" \
    "$here/AndroidManifest.xml" > "$out/AndroidManifest.xml"
aapt2 link -I "$androidjar" \
    --manifest "$out/AndroidManifest.xml" \
    --min-sdk-version 26 --target-sdk-version "$api" \
    -o "$out/base.apk"

echo "==> package"
(cd "$out" && zip -q base.apk classes.dex "lib/$abi/libgwapp.so")

echo "==> sign"
# The keystore lives beside the sources, NOT under $out: a fresh key per build
# would change the signing certificate, and Android refuses to update an
# installed app whose signature changed (INSTALL_FAILED_UPDATE_INCOMPATIBLE).
keystore=${KEYSTORE:-$here/debug.keystore}
if [ ! -f "$keystore" ]; then
    keytool -genkeypair -keystore "$keystore" -storepass android -keypass android \
        -alias gwhost -keyalg RSA -keysize 2048 -validity 10000 \
        -dname "CN=go-widgets android host" >/dev/null 2>&1
fi
zipalign -p -f 4 "$out/base.apk" "$out/$apkname.apk"
apksigner sign --ks "$keystore" --ks-pass pass:android --key-pass pass:android \
    --min-sdk-version 26 "$out/$apkname.apk"

echo "==> $out/$apkname.apk"
apksigner verify --print-certs "$out/$apkname.apk" | head -2
