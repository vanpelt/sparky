#!/bin/zsh
# Build Reframe.app (release) from the Swift package and open it.
#   ./bundle.sh          build + open
#   ./bundle.sh --no-open
set -euo pipefail
cd "$(dirname "$0")"
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"

swift build -c release --product Reframe
app=.build/Reframe.app
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp .build/release/Reframe "$app/Contents/MacOS/Reframe"

# App icon: Resources/AppIcon.png (1024², artwork on the 824² macOS icon grid) -> .icns
iconset=.build/AppIcon.iconset
rm -rf "$iconset"; mkdir -p "$iconset"
for sz in 16 32 128 256 512; do
  sips -z $sz $sz Resources/AppIcon.png --out "$iconset/icon_${sz}x${sz}.png" >/dev/null
  sips -z $((sz * 2)) $((sz * 2)) Resources/AppIcon.png --out "$iconset/icon_${sz}x${sz}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app/Contents/Resources/AppIcon.icns"
cat > "$app/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Reframe</string>
  <key>CFBundleDisplayName</key><string>Reframe</string>
  <key>CFBundleIdentifier</key><string>dev.sparky.reframe</string>
  <key>CFBundleExecutable</key><string>Reframe</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>LSMinimumSystemVersion</key><string>15.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSPrincipalClass</key><string>NSApplication</string>
</dict>
</plist>
PLIST
codesign --force --sign - "$app" >/dev/null 2>&1 || true
echo "built $app"
[[ "${1:-}" == "--no-open" ]] || open "$app"
