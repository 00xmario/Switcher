#!/bin/sh
# Builds Install Switcher.app around a built Switcher.app, and the disk image
# people download: Install-Switcher.dmg.
#
#   build/macos/installer/build.sh VERSION SWITCHER_APP OUT_DMG [OUT_APP]
#
# Runs on a Mac with the Xcode command line tools and Python 3.10 or later
# as python3. The installer carries Switcher.app unchanged: the app it
# installs is the one in Switcher_VERSION.dmg, byte for byte.
set -eu

[ $# -ge 3 ] || { echo "usage: $0 VERSION SWITCHER_APP OUT_DMG [OUT_APP]" >&2; exit 2; }
version=$1 switcher=$2 out=$3 out_app=${4:-}
case "$version" in *[!0-9.]* | "") echo "build.sh: version must be numeric, not $version" >&2; exit 2 ;; esac
here=$(cd "$(dirname "$0")" && pwd -P)
macos=$(dirname "$here")

[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$switcher/Contents/Info.plist")" = "$version" ] ||
	{ echo "build.sh: $switcher is not Switcher $version" >&2; exit 1; }
codesign --verify --deep --strict "$switcher"

work=$(mktemp -d "${TMPDIR:-/tmp}/install-switcher.XXXXXX")
[ -n "$work" ] && [ -d "$work" ] || { echo "build.sh: no work folder" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

app="$work/Install Switcher.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources/ui/logos"
sed "s/VERSION_TOKEN/$version/g" "$here/Info.plist" >"$app/Contents/Info.plist"
cp "$here/InstallerIcon.icns" "$app/Contents/Resources/"
cp "$here/ui/index.html" "$here/ui/installer.css" "$here/ui/installer.js" "$app/Contents/Resources/ui/"
for logo in claude codex gemini grok copilot opencode; do
	cp "$macos/logos/$logo.svg" "$app/Contents/Resources/ui/logos/"
done

# One installer for every Mac, back to macOS 13, like the app.
for target in arm64-apple-macos13.0 x86_64-apple-macos13.0; do
	swiftc -parse-as-library -O -target "$target" -o "$work/InstallSwitcher-$target" "$here/installer.swift"
done
lipo -create -output "$app/Contents/MacOS/InstallSwitcher" "$work"/InstallSwitcher-*-apple-macos13.0

ditto "$switcher" "$app/Contents/Resources/Switcher.app"
# Sign only the installer: its seal covers the Switcher.app inside as it is,
# and the inside keeps its own signature.
codesign --force -s - "$app"
codesign --verify --deep --strict "$app"
codesign --verify --deep --strict "$app/Contents/Resources/Switcher.app"

# The Finder window's background, drawn from background.html by the
# installer itself (WebKit), at 1x and 2x: the page is the only source.
"$app/Contents/MacOS/InstallSwitcher" --snapshot "$work/bg" "$here/dmg/background.html"
# Both pictures are the dark background, never a page caught before it drew.
for png in "$work/bg/background.png" "$work/bg/background@2x.png"; do
	w=$(sips -g pixelWidth "$png" | awk '/pixelWidth/ { print $2 }')
	h=$(sips -g pixelHeight "$png" | awk '/pixelHeight/ { print $2 }')
	bright=$(swift "$here/dmg/brightness.swift" "$png" 0 0 "$w" "$h")
	awk -v b="$bright" 'BEGIN { exit !(b < 0.35) }' || { echo "build.sh: $png is not the dark background ($bright)" >&2; exit 1; }
done
tiffutil -cathidpicheck "$work/bg/background.png" "$work/bg/background@2x.png" -out "$work/background.tiff" >/dev/null

# dmgbuild is the one third-party tool in the release: the exact wheels are
# pinned by hash, and it and pip run with an empty environment, so neither
# sees the job's tokens or credentials.
clean() { env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME="$work/home" TMPDIR="$work/tmp" LANG=en_US.UTF-8 "$@"; }
mkdir -p "$work/home" "$work/tmp"
py=$(command -v python3)
"$py" -c 'import sys; sys.exit(sys.version_info < (3, 10))' ||
	{ echo "build.sh: dmgbuild needs Python 3.10 or later; $py is older" >&2; exit 1; }
clean "$py" -m venv "$work/venv"
for attempt in 1 2 3; do
	clean "$work/venv/bin/pip" install --quiet --disable-pip-version-check --no-deps --only-binary=:all: \
		--require-hashes -r "$here/dmg/requirements.txt" && break
	[ "$attempt" -lt 3 ] || { echo "build.sh: could not install dmgbuild" >&2; exit 1; }
	sleep 5
done
rm -f "$out"
for attempt in 1 2 3; do
	clean "$work/venv/bin/dmgbuild" -s "$here/dmg/settings.py" \
		-D app="$app" -D icon="$here/InstallerIcon.icns" -D background="$work/background.tiff" \
		"Install Switcher" "$out" && break
	[ "$attempt" -lt 3 ] || { echo "build.sh: dmgbuild failed after 3 attempts" >&2; exit 1; }
	echo "dmgbuild attempt $attempt failed; retrying in 5s"
	sleep 5
done
hdiutil verify -quiet "$out"
if [ -n "$out_app" ]; then
	rm -rf "$out_app"
	ditto "$app" "$out_app"
fi
echo "Built $out"
