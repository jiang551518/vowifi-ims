#!/bin/sh
# Builds dist/vowifi-ims-module.zip from module/ and the debug APK.
set -e
ROOT=$(cd "$(dirname "$0")/.." && pwd)
APK="$ROOT/android/app/build/outputs/apk/debug/app-debug.apk"
[ -f "$APK" ] || { echo "build the APK first (./gradlew assembleDebug)"; exit 1; }
mkdir -p "$ROOT/module/system/priv-app/VoWiFi"
cp "$APK" "$ROOT/module/system/priv-app/VoWiFi/VoWiFi.apk"
mkdir -p "$ROOT/dist"
rm -f "$ROOT/dist/vowifi-ims-module.zip"
cd "$ROOT/module"
zip -qr "$ROOT/dist/vowifi-ims-module.zip" README.md module.prop customize.sh service.sh uninstall.sh action.sh webroot META-INF system
echo "$ROOT/dist/vowifi-ims-module.zip"
