#!/bin/bash
# Rebuild RootHelper dex + vwcli and push them to the phone.
set -e
cd "$(dirname "$0")/.."
export PATH=$HOME/toolchains/go/bin:$PATH
SDK=$HOME/toolchains/android-sdk
# adb serial of the test phone: tools/device.local (not in git), e.g. ANDROID_SERIAL=XXXX
[ -f tools/device.local ] && . tools/device.local
export ANDROID_SERIAL
rm -rf tools/build/helper && mkdir -p tools/build/helper
javac -nowarn --release 11 -cp $SDK/platforms/android-36/android.jar -d tools/build/helper/classes android/app/src/main/java/app/vowifi/root/RootHelper.java 2>&1 | grep -v "^warning\|^Note\|warning$" || true
$SDK/build-tools/36.1.0/d8 --min-api 30 --output tools/build/helper/vwhelper.jar $(find tools/build/helper/classes -name '*.class') --lib $SDK/platforms/android-36/android.jar
(cd core && GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -o ../tools/build/vwcli ./cmd/vwcli)
adb push tools/build/helper/vwhelper.jar tools/build/vwcli /data/local/tmp/ >/dev/null
adb shell chmod 755 /data/local/tmp/vwcli
echo pushed
