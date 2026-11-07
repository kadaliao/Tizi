#!/bin/sh
set -eu

VERSION="${1:-}"
if ! printf '%s\n' "$VERSION" | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { valid = 1 } END { exit !valid }'; then
	printf 'usage: %s vMAJOR.MINOR.PATCH\n' "$0" >&2
	exit 2
fi

root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$root"
if [ "$(uname -s)" != "Darwin" ] || ! command -v swift >/dev/null 2>&1; then
	printf 'error: 发布需要在装有 Xcode 的 macOS 上进行，以便同时构建桌面版\n' >&2
	exit 1
fi
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	printf 'error: 工作树有未提交的改动，发布二进制必须来自已提交的代码\n' >&2
	exit 1
fi
head_tag="$(git describe --tags --exact-match HEAD 2>/dev/null || true)"
if [ -n "$head_tag" ] && [ "$head_tag" != "$VERSION" ]; then
	printf 'error: HEAD 已打标签 %s，与请求的版本 %s 不一致\n' "$head_tag" "$VERSION" >&2
	exit 1
fi
if [ "${SKIP_VERSION_CHECK:-}" != "1" ]; then
	readme_version="$(sed -n 's/.*当前版本是 `\(v[0-9.]*\)`.*/\1/p' README.md | head -n 1)"
	if [ "$readme_version" != "$VERSION" ]; then
		printf 'error: README 声明的版本 %s 与 %s 不一致，请先执行版本准备提交\n' "${readme_version:-<无>}" "$VERSION" >&2
		exit 1
	fi
fi
dist="$root/dist"
rm -rf "$dist"
mkdir -p "$dist"

for platform in darwin:arm64 darwin:amd64 linux:amd64; do
	os="${platform%%:*}"
	arch="${platform#*:}"
	work="$dist/$os-$arch"
	mkdir -p "$work"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
		-trimpath \
		-ldflags "-s -w -X main.version=$VERSION" \
		-o "$work/tizi" \
		./cmd/tizi
	COPYFILE_DISABLE=1 tar --no-xattrs -czf "$dist/tizi-$os-$arch.tar.gz" -C "$work" tizi
	case "$os" in
	darwin) mv "$work/tizi" "$dist/engine-$arch" ;;
	esac
	rm -rf "$work"
done

# 桌面版：通用二进制 .app，内含通用引擎，首次打开时自动装到 ~/.local/bin。
# zip 由 install.sh 和 tizi update 安装，dmg 供直接下载安装。
lipo -create "$dist/engine-arm64" "$dist/engine-amd64" -output "$dist/engine-universal"
VERSION="$VERSION" OUT="$dist/desktop" ZIP=1 DMG=1 ENGINE_BIN="$dist/engine-universal" ./macos/scripts/build-app.sh
mv "$dist/desktop/TiziDesktop-macos.zip" "$dist/desktop/Tizi-$VERSION.dmg" "$dist/"
rm -rf "$dist/desktop" "$dist"/engine-*

(
	cd "$dist"
	shasum -a 256 tizi-*.tar.gz TiziDesktop-macos.zip "Tizi-$VERSION.dmg" >checksums.txt
)
printf 'release assets written to %s\n' "$dist"
