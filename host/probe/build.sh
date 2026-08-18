#!/bin/sh
# Copyright (c) the go-widgets/android authors. All rights reserved.
# SPDX-License-Identifier: BSD-3-Clause
#
# Builds the accessibility probe APK — a TEST INSTRUMENT, never part of the
# shipped demo. Same toolchain as host/build.sh: javac, d8, aapt2, zipalign,
# apksigner. It has resources (the service config), so aapt2 compiles those too.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
out=${OUT:-$here/out}

: "${ANDROID_HOME:?set ANDROID_HOME}"
: "${JAVA_HOME:?set JAVA_HOME}"

api=${API:-$(ls "$ANDROID_HOME/platforms" | sed -n 's/^android-\([0-9]\{1,\}\)$/\1/p' | sort -n | tail -1)}
buildtools=${BUILD_TOOLS:-$(ls "$ANDROID_HOME/build-tools" | sort -V | tail -1)}
bt="$ANDROID_HOME/build-tools/$buildtools"
androidjar="$ANDROID_HOME/platforms/android-$api/android.jar"
PATH="$JAVA_HOME/bin:$bt:$PATH"
export PATH

rm -rf "$out"
mkdir -p "$out/classes" "$out/res"

echo "==> aapt2 compile (resources)"
aapt2 compile --dir "$here/res" -o "$out/res.zip"

echo "==> aapt2 link"
aapt2 link -I "$androidjar" \
    --manifest "$here/AndroidManifest.xml" \
    --min-sdk-version 26 --target-sdk-version "$api" \
    -R "$out/res.zip" --auto-add-overlay \
    -o "$out/base.apk"

echo "==> javac"
find "$here/java" -name '*.java' > "$out/sources.txt"
javac -source 17 -target 17 -nowarn -classpath "$androidjar" \
    -d "$out/classes" @"$out/sources.txt"

echo "==> d8"
find "$out/classes" -name '*.class' > "$out/classes.txt"
d8 --lib "$androidjar" --min-api 26 --output "$out" @"$out/classes.txt"

echo "==> package + sign"
(cd "$out" && zip -q base.apk classes.dex)
keystore=${KEYSTORE:-$here/../debug.keystore}
zipalign -p -f 4 "$out/base.apk" "$out/gwprobe.apk"
apksigner sign --ks "$keystore" --ks-pass pass:android --key-pass pass:android \
    --min-sdk-version 26 "$out/gwprobe.apk"
echo "==> $out/gwprobe.apk"
