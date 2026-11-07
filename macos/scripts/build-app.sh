#!/bin/sh
# 构建Tizi 桌面版 TiziDesktop.app，并做 ad-hoc 签名。
#
# 环境变量：
#   VERSION  写入 Info.plist 的版本，默认取 git describe
#   OUT      输出目录，默认仓库的 bin/
#   ARCHS    目标架构，默认 "arm64 x86_64"（通用二进制）；本机调试可设为 arm64
#   ZIP=1    另外输出 TiziDesktop-macos.zip（发布用）
#   DMG=1    另外输出 Tizi-<版本>.dmg（可以直接下载安装）
#   ENGINE_BIN  tizi 引擎程序；设置后打进 .app，首次打开时自动装到
#            ~/.local/bin。发布构建必须设置，否则 .app 单独分发时无法使用
set -eu

macos_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
repo="$(dirname "$macos_dir")"
VERSION="${VERSION:-$(git -C "$repo" describe --tags --dirty 2>/dev/null || printf dev)}"
OUT="${OUT:-$repo/bin}"
ARCHS="${ARCHS:-arm64 x86_64}"
bundle_id="com.kadaliao.tizi.desktop"

# CFBundleShortVersionString 只接受数字段：v0.2.20-3-gabc-dirty -> 0.2.20。
short_version="$(printf '%s\n' "$VERSION" | sed -n 's/^v\{0,1\}\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p')"
short_version="${short_version:-0.0.0}"

# SwiftPM 默认把 SDK 版本记成部署目标（13.0），新系统会按旧 SDK 的兼容模式
# 运行界面；显式写入实际 SDK 版本，最低系统要求仍是 macOS 13。
sdk_version="$(xcrun --show-sdk-version)"
link_flags="-Xlinker -platform_version -Xlinker macos -Xlinker 13.0 -Xlinker $sdk_version"

work="$(mktemp -d "${TMPDIR:-/tmp}/tizi-desktop.XXXXXX")"
trap 'rm -rf "$work"' EXIT HUP INT TERM

cd "$macos_dir"
# 逐个架构构建再用 lipo 合并：一次传多个 --arch 会改走 XCBuild，它把 -Xlinker
# 后的参数直接交给 clang，Xcode 26 起链接会失败。每个架构用独立的 scratch
# 目录：Swift 6.4 的默认后端把各架构产物写进同一个目录，后一次会覆盖前一次，
# lipo 就只会拿到重复的同一个架构。
for arch in $ARCHS; do
	# shellcheck disable=SC2086
	swift build -c release --arch "$arch" --scratch-path ".build/$arch" $link_flags
done
products="$work/products"
mkdir -p "$products"
for product in TiziDesktop TiziAskPass; do
	set --
	for arch in $ARCHS; do
		# shellcheck disable=SC2086
		set -- "$@" "$(swift build -c release --arch "$arch" --scratch-path ".build/$arch" $link_flags --show-bin-path)/$product"
	done
	lipo -create "$@" -output "$products/$product"
done

# 图标：先渲染 1024 像素 PNG，再生成 iconset 与 icns。
swift "$macos_dir/scripts/make-icon.swift" "$work/icon.png"
iconset="$work/AppIcon.iconset"
mkdir -p "$iconset"
for size in 16 32 128 256 512; do
	sips -z "$size" "$size" "$work/icon.png" --out "$iconset/icon_${size}x${size}.png" >/dev/null
	double=$((size * 2))
	sips -z "$double" "$double" "$work/icon.png" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$work/AppIcon.icns"

write_plist() {
	# $1 路径 $2 可执行文件 $3 bundle id $4 显示名
	cat >"$1" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key>
	<string>zh_CN</string>
	<key>CFBundleExecutable</key>
	<string>$2</string>
	<key>CFBundleIdentifier</key>
	<string>$3</string>
	<key>CFBundleName</key>
	<string>$4</string>
	<key>CFBundleDisplayName</key>
	<string>$4</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleIconFile</key>
	<string>AppIcon</string>
	<key>CFBundleShortVersionString</key>
	<string>$short_version</string>
	<key>CFBundleVersion</key>
	<string>$short_version</string>
	<key>TiziBuildVersion</key>
	<string>$VERSION</string>
	<key>LSMinimumSystemVersion</key>
	<string>13.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>LSApplicationCategoryType</key>
	<string>public.app-category.utilities</string>
	<key>NSHighResolutionCapable</key>
	<true/>
</dict>
</plist>
EOF
}

app="$work/TiziDesktop.app"
helper="$app/Contents/Helpers/TiziAskPass.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" "$helper/Contents/MacOS" "$helper/Contents/Resources"
cp "$products/TiziDesktop" "$app/Contents/MacOS/TiziDesktop"
cp "$products/TiziAskPass" "$helper/Contents/MacOS/TiziAskPass"
cp "$work/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
cp "$work/AppIcon.icns" "$helper/Contents/Resources/AppIcon.icns"
write_plist "$app/Contents/Info.plist" TiziDesktop "$bundle_id" "Tizi"
write_plist "$helper/Contents/Info.plist" TiziAskPass "$bundle_id.askpass" "Tizi 授权"

if [ -n "${ENGINE_BIN:-}" ]; then
	mkdir -p "$app/Contents/Helpers"
	cp "$ENGINE_BIN" "$app/Contents/Helpers/tizi"
	chmod 755 "$app/Contents/Helpers/tizi"
	codesign --force --sign - --timestamp=none "$app/Contents/Helpers/tizi"
fi

# 先签内层 helper，再签外层 app；不使用 --deep。
codesign --force --sign - --timestamp=none "$helper"
codesign --force --sign - --timestamp=none "$app"
codesign --verify --strict "$app"

mkdir -p "$OUT"
rm -rf "$OUT/TiziDesktop.app"
ditto "$app" "$OUT/TiziDesktop.app"
if [ "${ZIP:-}" = "1" ]; then
	rm -f "$OUT/TiziDesktop-macos.zip"
	# 不带扩展属性：构建机上的 provenance 等属性对安装方没有意义。
	ditto -c -k --norsrc --noextattr --keepParent "$app" "$OUT/TiziDesktop-macos.zip"
fi
if [ "${DMG:-}" = "1" ]; then
	# 磁盘映像：应用 + Applications 快捷方式 + 首次打开说明。
	dmg_name="Tizi-${VERSION}.dmg"
	stage="$work/dmg"
	mkdir -p "$stage"
	ditto "$app" "$stage/TiziDesktop.app"
	ln -s /Applications "$stage/Applications"
	cp "$macos_dir/scripts/dmg-readme.txt" "$stage/首次打开请先看这里.txt"
	rm -f "$OUT/$dmg_name"
	hdiutil create -quiet -volname "Tizi" -srcfolder "$stage" -fs HFS+ -format UDZO "$OUT/$dmg_name"
	printf '已生成 %s\n' "$OUT/$dmg_name"
fi
printf '已生成 %s（%s，%s）\n' "$OUT/TiziDesktop.app" "$VERSION" "$ARCHS"
