#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BUILD_DIR="$SCRIPT_DIR/build"
APP_NAME="OpenMessages"
APP_BUNDLE="$BUILD_DIR/$APP_NAME.app"

echo "==> Building Go backend..."
cd "$ROOT_DIR"
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o "$SCRIPT_DIR/build/openmessages-arm64" .
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o "$SCRIPT_DIR/build/openmessages-amd64" .
lipo -create -output "$SCRIPT_DIR/build/openmessages" \
    "$SCRIPT_DIR/build/openmessages-arm64" \
    "$SCRIPT_DIR/build/openmessages-amd64"
echo "   Universal binary: $(du -h "$SCRIPT_DIR/build/openmessages" | cut -f1)"

echo "==> Building Swift app..."
cd "$SCRIPT_DIR/OpenMessages"
swift build -c release --arch arm64 --arch x86_64 2>&1 | tail -5

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
cp "$SCRIPT_DIR/build/openmessages" "$APP_BUNDLE/Contents/Resources/openmessages"
chmod +x "$APP_BUNDLE/Contents/Resources/openmessages"

# Copy Info.plist
cp "$SCRIPT_DIR/OpenMessages/Sources/Info.plist" "$APP_BUNDLE/Contents/Info.plist"

# Create PkgInfo
echo -n "APPL????" > "$APP_BUNDLE/Contents/PkgInfo"

# Ad-hoc code sign (avoids "unidentified developer" for local builds)
echo "==> Code signing..."
codesign --force --deep --sign - "$APP_BUNDLE"

# Remove quarantine attribute
xattr -cr "$APP_BUNDLE"

echo "==> Built: $APP_BUNDLE"
echo "   Size: $(du -sh "$APP_BUNDLE" | cut -f1)"
echo ""
echo "To run:  open $APP_BUNDLE"
echo "To install: cp -R $APP_BUNDLE /Applications/"
