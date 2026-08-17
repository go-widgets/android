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

echo "==> go build (CGO_ENABLED=0 GOOS=android GOARCH=$goarch)"
(cd "$root" && GOWORK=off CGO_ENABLED=0 GOOS=android GOARCH="$goarch" \
    go build -trimpath -ldflags=-s -o "$out/lib/$abi/libgwapp.so" ./cmd/gwapp)

echo "==> javac"
find "$here/java" -name '*.java' > "$out/sources.txt"
javac -source 17 -target 17 -nowarn -classpath "$androidjar" \
    -d "$out/classes" @"$out/sources.txt"

echo "==> d8"
find "$out/classes" -name '*.class' > "$out/classes.txt"
d8 --lib "$androidjar" --min-api 26 --output "$out" @"$out/classes.txt"

echo "==> aapt2 link"
aapt2 link -I "$androidjar" \
    --manifest "$here/AndroidManifest.xml" \
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
zipalign -p -f 4 "$out/base.apk" "$out/gwhost.apk"
apksigner sign --ks "$keystore" --ks-pass pass:android --key-pass pass:android \
    --min-sdk-version 26 "$out/gwhost.apk"

echo "==> $out/gwhost.apk"
apksigner verify --print-certs "$out/gwhost.apk" | head -2
