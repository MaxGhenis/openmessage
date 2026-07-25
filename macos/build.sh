#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BUILD_DIR="$SCRIPT_DIR/build"
APP_NAME="OpenMessage"
APP_BUNDLE="$BUILD_DIR/$APP_NAME.app"
DMG_PATH="$BUILD_DIR/$APP_NAME.dmg"
APP_ENTITLEMENTS="$SCRIPT_DIR/OpenMessage.entitlements"
HELPER_ENTITLEMENTS="$SCRIPT_DIR/OpenMessageHelper.entitlements"

# ── Notarization config ──
# Set these env vars to enable code signing + notarization:
#   DEVELOPER_ID   - e.g. "Developer ID Application: Max Ghenis (TEAMID)"
#   APPLE_ID       - e.g. "mghenis@gmail.com"
#   APPLE_TEAM_ID  - e.g. "ABC123XYZ"
#   APP_PASSWORD    - app-specific password from appleid.apple.com
SIGN_IDENTITY="${DEVELOPER_ID:-}"

# ── Bundle identity: dev builds must never shadow the installed app ──
# Every OpenMessage .app that declares CFBundleIdentifier com.openmessage.app is
# a candidate when LaunchServices resolves "OpenMessage" (Spotlight, Dock,
# `open -a OpenMessage`, notification clicks). Stale build outputs sharing the
# release id have twice caused the wrong binary to launch — most recently
# 2026-07-25, when a build predating the Google self-heal OSID fix (PR #148)
# latched Google Messages in needs_repair for ~10.5h.
#
# So: plain `./macos/build.sh` produces a *dev* bundle id that can never win
# that resolution. Shippable builds must opt in explicitly:
#
#     RELEASE=1 ./macos/build.sh
#
# Note the data dir is NOT bundle-id-scoped (BackendManager.swift hardcodes
# ~/Library/Application Support/OpenMessage), so a dev build still reads the
# same store. What IS id-scoped: UserDefaults (the `defaults write
# com.openmessage.app V2Primary` lever), the notification grant, and the
# sandbox container.
RELEASE_BUILD="${RELEASE:-0}"
if [ "$RELEASE_BUILD" = "1" ]; then
    BUNDLE_ID="com.openmessage.app"
    BUNDLE_DISPLAY_NAME="OpenMessage"
else
    BUNDLE_ID="com.openmessage.app.dev"
    BUNDLE_DISPLAY_NAME="OpenMessage (dev)"
    DMG_PATH="$BUILD_DIR/$APP_NAME-dev.dmg"
fi

# Detect version from git tag (or VERSION env override). Falls back to "dev".
VERSION="${VERSION:-$(git -C "$ROOT_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)}"
echo "==> Version: $VERSION"

echo "==> Building Go backend..."
cd "$ROOT_DIR"
GO_LDFLAGS="-s -w -X main.version=${VERSION}"
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="${GO_LDFLAGS}" -o "$SCRIPT_DIR/build/openmessage-arm64" .
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="${GO_LDFLAGS}" -o "$SCRIPT_DIR/build/openmessage-amd64" .
lipo -create -output "$SCRIPT_DIR/build/openmessage" \
    "$SCRIPT_DIR/build/openmessage-arm64" \
    "$SCRIPT_DIR/build/openmessage-amd64"
echo "   Universal binary: $(du -h "$SCRIPT_DIR/build/openmessage" | cut -f1)"

echo "==> Building Swift app..."
cd "$SCRIPT_DIR/OpenMessage"
# Build, but preserve full output on failure for debugging
SWIFT_BUILD_LOG=$(mktemp)
trap 'rm -f "$SWIFT_BUILD_LOG"' EXIT
if ! swift build -c release --arch arm64 --arch x86_64 > "$SWIFT_BUILD_LOG" 2>&1; then
    echo "Swift build failed. Full log:" >&2
    cat "$SWIFT_BUILD_LOG" >&2
    exit 1
fi
tail -5 "$SWIFT_BUILD_LOG"

# Find the built executable
SWIFT_BIN=$(swift build -c release --arch arm64 --arch x86_64 --show-bin-path 2>/dev/null)/"$APP_NAME"
if [ ! -f "$SWIFT_BIN" ]; then
    echo "ERROR: Swift binary not found at $SWIFT_BIN"
    echo "Searching..."
    find .build -name "$APP_NAME" -type f 2>/dev/null
    exit 1
fi

echo "==> Assembling $APP_NAME.app..."
rm -rf "$APP_BUNDLE"
mkdir -p "$APP_BUNDLE/Contents/MacOS"
mkdir -p "$APP_BUNDLE/Contents/Resources"

# Copy Swift executable
cp "$SWIFT_BIN" "$APP_BUNDLE/Contents/MacOS/$APP_NAME"

# Copy Go backend binary into Resources
cp "$SCRIPT_DIR/build/openmessage" "$APP_BUNDLE/Contents/Resources/openmessage"
chmod +x "$APP_BUNDLE/Contents/Resources/openmessage"

# Copy Info.plist, then stamp the bundle identity for this build mode.
cp "$SCRIPT_DIR/OpenMessage/Sources/Info.plist" "$APP_BUNDLE/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleIdentifier $BUNDLE_ID" "$APP_BUNDLE/Contents/Info.plist"
if /usr/libexec/PlistBuddy -c "Print :CFBundleDisplayName" "$APP_BUNDLE/Contents/Info.plist" >/dev/null 2>&1; then
    /usr/libexec/PlistBuddy -c "Set :CFBundleDisplayName $BUNDLE_DISPLAY_NAME" "$APP_BUNDLE/Contents/Info.plist"
else
    /usr/libexec/PlistBuddy -c "Add :CFBundleDisplayName string $BUNDLE_DISPLAY_NAME" "$APP_BUNDLE/Contents/Info.plist"
fi
echo "   Bundle id: $BUNDLE_ID"

# Generate and copy app icon
ICON_SRC="$SCRIPT_DIR/OpenMessage/Sources/Assets.xcassets/AppIcon.appiconset"
if [ -f "$ICON_SRC/icon_512x512.png" ]; then
    ICONSET="$BUILD_DIR/AppIcon.iconset"
    rm -rf "$ICONSET"
    mkdir -p "$ICONSET"
    sips -z 16 16     "$ICON_SRC/icon_512x512.png" --out "$ICONSET/icon_16x16.png"      >/dev/null 2>&1
    sips -z 32 32     "$ICON_SRC/icon_512x512.png" --out "$ICONSET/icon_16x16@2x.png"   >/dev/null 2>&1
    sips -z 32 32     "$ICON_SRC/icon_512x512.png" --out "$ICONSET/icon_32x32.png"      >/dev/null 2>&1
    sips -z 64 64     "$ICON_SRC/icon_512x512.png" --out "$ICONSET/icon_32x32@2x.png"   >/dev/null 2>&1
    sips -z 128 128   "$ICON_SRC/icon_512x512.png" --out "$ICONSET/icon_128x128.png"    >/dev/null 2>&1
    sips -z 256 256   "$ICON_SRC/icon_512x512.png" --out "$ICONSET/icon_128x128@2x.png" >/dev/null 2>&1
    sips -z 256 256   "$ICON_SRC/icon_512x512.png" --out "$ICONSET/icon_256x256.png"    >/dev/null 2>&1
    cp "$ICON_SRC/icon_512x512.png"    "$ICONSET/icon_256x256@2x.png"
    cp "$ICON_SRC/icon_512x512.png"    "$ICONSET/icon_512x512.png"
    cp "$ICON_SRC/icon_512x512@2x.png" "$ICONSET/icon_512x512@2x.png"
    iconutil -c icns "$ICONSET" -o "$APP_BUNDLE/Contents/Resources/AppIcon.icns"
    rm -rf "$ICONSET"
    echo "   App icon: $(du -h "$APP_BUNDLE/Contents/Resources/AppIcon.icns" | cut -f1)"
fi

# Create PkgInfo
echo -n "APPL????" > "$APP_BUNDLE/Contents/PkgInfo"

# ── Code signing ──
echo "==> Code signing..."
if [ -n "$SIGN_IDENTITY" ]; then
    echo "   Signing with: $SIGN_IDENTITY"
    # Sign the Go binary first
    codesign --force --options runtime \
        --entitlements "$HELPER_ENTITLEMENTS" \
        --sign "$SIGN_IDENTITY" \
        "$APP_BUNDLE/Contents/Resources/openmessage"
    # Sign the main app
    codesign --force --options runtime \
        --entitlements "$APP_ENTITLEMENTS" \
        --sign "$SIGN_IDENTITY" \
        "$APP_BUNDLE"
else
    echo "   No DEVELOPER_ID set — using ad-hoc signature"
    codesign --force --deep --sign - "$APP_BUNDLE"
fi

# Remove quarantine attribute
xattr -cr "$APP_BUNDLE"

echo "==> Built: $APP_BUNDLE"
echo "   Size: $(du -sh "$APP_BUNDLE" | cut -f1)"

# ── Create DMG ──
echo "==> Creating DMG..."
rm -f "$DMG_PATH"
hdiutil create -volname "$APP_NAME" -srcfolder "$APP_BUNDLE" -ov -format UDZO "$DMG_PATH" 2>&1 | tail -1
echo "   DMG: $(du -h "$DMG_PATH" | cut -f1)"

# ── Sign the DMG itself ──
# Gatekeeper checks the signature on the .dmg container before mounting it.
# Without this, spctl --assess --type install reports "no usable signature".
if [ -n "$SIGN_IDENTITY" ]; then
    echo "==> Signing DMG..."
    codesign --force --sign "$SIGN_IDENTITY" --timestamp "$DMG_PATH"
fi

# ── Notarize ──
# Two modes:
#   1. Local: AC_USERNAME / AC_PASSWORD / AC_TEAM_ID set explicitly (CI path)
#   2. Local: NOTARY_KEYCHAIN_PROFILE points at a stored xcrun notarytool profile
# If neither is set we skip notarization but still produce a signed DMG.
if [ -n "$SIGN_IDENTITY" ]; then
    if [ -n "${AC_USERNAME:-}" ] && [ -n "${AC_PASSWORD:-}" ] && [ -n "${AC_TEAM_ID:-}" ]; then
        echo "==> Submitting for notarization (Apple ID credentials)..."
        xcrun notarytool submit "$DMG_PATH" \
            --apple-id "$AC_USERNAME" \
            --password "$AC_PASSWORD" \
            --team-id "$AC_TEAM_ID" \
            --wait

        echo "==> Stapling notarization ticket..."
        xcrun stapler staple "$DMG_PATH"
        echo "   Notarized and stapled!"
    elif xcrun notarytool history --keychain-profile "${NOTARY_KEYCHAIN_PROFILE:-OpenMessage}" >/dev/null 2>&1; then
        NOTARY_PROFILE="${NOTARY_KEYCHAIN_PROFILE:-OpenMessage}"
        echo "==> Submitting for notarization (keychain profile: $NOTARY_PROFILE)..."
        xcrun notarytool submit "$DMG_PATH" \
            --keychain-profile "$NOTARY_PROFILE" \
            --wait

        echo "==> Stapling notarization ticket..."
        xcrun stapler staple "$DMG_PATH"
        echo "   Notarized and stapled!"
    else
        echo ""
        echo "   Signed but NOT notarized (set AC_USERNAME / AC_PASSWORD / AC_TEAM_ID"
        echo "   or store a keychain profile via 'xcrun notarytool store-credentials')."
    fi
else
    echo ""
    echo "   To sign + notarize, set: DEVELOPER_ID (e.g. \"Developer ID Application: Max Ghenis (8VB5UKQZC6)\")"
fi

# ── Keep the build output out of LaunchServices ──
# Belt-and-braces alongside the dev bundle id. Note this alone is NOT durable:
# any LaunchServices rescan re-registers the bundle, so it narrows the window
# rather than closing it. The dev bundle id above is what actually closes it.
LSREGISTER=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
if [ -x "$LSREGISTER" ]; then
    "$LSREGISTER" -u "$APP_BUNDLE" >/dev/null 2>&1 || true
fi

echo ""
echo "==> Done!"
echo "   App: $APP_BUNDLE"
echo "   DMG: $DMG_PATH"
echo "   Bundle id: $BUNDLE_ID"
if [ "$RELEASE_BUILD" != "1" ]; then
    echo ""
    echo "   ┌─────────────────────────────────────────────────────────────┐"
    echo "   │  DEV BUILD — bundle id com.openmessage.app.dev              │"
    echo "   │  Deliberately cannot shadow /Applications/OpenMessage.app.  │"
    echo "   │  Do NOT ship this or copy it over the installed app.        │"
    echo "   │  For a shippable build:  RELEASE=1 ./macos/build.sh         │"
    echo "   └─────────────────────────────────────────────────────────────┘"
fi
echo ""
echo "To run:  open $APP_BUNDLE"
if [ "$RELEASE_BUILD" = "1" ]; then
    echo "To install: cp -R $APP_BUNDLE /Applications/"
else
    echo "To install: rebuild with RELEASE=1 first — installing this dev-id"
    echo "            bundle would orphan the V2Primary lever + notification grant."
fi
