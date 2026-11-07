#!/bin/sh
set -eu

VERSION="${VERSION:-}"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
# macOS 桌面版安装位置；INSTALL_DESKTOP=0 只安装终端版。
APP_DIR="${APP_DIR:-$HOME/Applications}"
INSTALL_DESKTOP="${INSTALL_DESKTOP:-1}"
DESKTOP_ASSET="TiziDesktop-macos.zip"
DESKTOP_APP="TiziDesktop.app"
RELEASE_LATEST_URL="${RELEASE_LATEST_URL:-https://github.com/kadaliao/Tizi/releases/latest}"
RELEASE_DOWNLOAD_ROOT="${RELEASE_DOWNLOAD_ROOT:-https://github.com/kadaliao/Tizi/releases/download}"

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
tmp_dir=""
install_tmp=""
desktop_stage=""
source_build=""
cleanup() {
	[ -z "$tmp_dir" ] || rm -rf "$tmp_dir"
	[ -z "$install_tmp" ] || rm -f "$install_tmp"
	if [ -n "$desktop_stage" ]; then
		# 在两次 mv 之间被中断时，先把原来的桌面版放回去再清理暂存目录。
		if [ -e "$desktop_stage/previous.app" ] && [ ! -e "$APP_DIR/$DESKTOP_APP" ]; then
			mv "$desktop_stage/previous.app" "$APP_DIR/$DESKTOP_APP" || true
		fi
		rm -rf "$desktop_stage"
	fi
}
trap cleanup EXIT HUP INT TERM

case "$(uname -s)" in
Darwin) os="darwin" ;;
Linux)
	os="linux"
	if [ ! -r /etc/os-release ] ||
		! grep -Eq '^ID="?ubuntu"?$' /etc/os-release ||
		! grep -Eq '^VERSION_ID="?(22\.04|24\.04)"?$' /etc/os-release; then
		printf 'error: Linux 首版仅支持 Ubuntu 22.04/24.04\n' >&2
		exit 1
	fi
	;;
*)
	printf 'error: 不支持的系统 %s\n' "$(uname -s)" >&2
	exit 1
	;;
esac
case "$os:$(uname -m)" in
darwin:arm64) arch="arm64" ;;
darwin:x86_64 | linux:x86_64) arch="amd64" ;;
*)
	printf 'error: 不支持的平台架构 %s/%s\n' "$os" "$(uname -m)" >&2
	exit 1
	;;
esac

mkdir -p "$BIN_DIR"
install_tmp="$BIN_DIR/.tizi.$$"

if [ -f "$script_dir/go.mod" ] && [ -d "$script_dir/cmd/tizi" ]; then
	source_build=1
	command -v go >/dev/null 2>&1 || {
		printf 'error: 从源码安装需要 Go 1.26 或更高版本\n' >&2
		exit 1
	}
	go_version="$(go env GOVERSION)"
	go_version="${go_version#go}"
	go_major="${go_version%%.*}"
	go_rest="${go_version#*.}"
	go_minor="${go_rest%%.*}"
	# go1.26rc1 / go1.26beta1 这类预发布工具链没有 patch 段，minor 后紧跟后缀。
	go_minor="${go_minor%%[!0-9]*}"
	case "$go_rest" in
	*.*) go_patch="${go_rest#*.}" ;;
	*) go_patch="0" ;;
	esac
	go_patch="${go_patch%%[!0-9]*}"
	go_patch="${go_patch:-0}"
	case "$go_major:$go_minor:$go_patch" in
	*[!0-9:]*)
		printf 'error: 无法识别 Go 版本 %s\n' "$go_version" >&2
		exit 1
		;;
	esac
	if [ "$go_major" -lt 1 ] || { [ "$go_major" -eq 1 ] && [ "$go_minor" -lt 26 ]; }; then
		printf 'error: 从源码安装需要 Go 1.26 或更高版本，当前为 %s\n' "$go_version" >&2
		exit 1
	fi
	if [ -z "$VERSION" ]; then
		VERSION="$(git -C "$script_dir" describe --tags --dirty 2>/dev/null || printf dev)"
	fi
	(
		cd "$script_dir"
		GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$install_tmp" ./cmd/tizi
	)
else
	for command_name in curl tar; do
		command -v "$command_name" >/dev/null 2>&1 || {
			printf 'error: 远程安装需要 %s\n' "$command_name" >&2
			exit 1
		}
	done
	if command -v shasum >/dev/null 2>&1; then
		sha256_command="shasum -a 256"
	elif command -v sha256sum >/dev/null 2>&1; then
		sha256_command="sha256sum"
	else
		printf 'error: 远程安装需要 shasum 或 sha256sum\n' >&2
		exit 1
	fi
	if [ -z "$VERSION" ]; then
		latest_url="$(curl -fsSL -o /dev/null -w '%{url_effective}' "$RELEASE_LATEST_URL")"
		VERSION="${latest_url##*/}"
	fi
	if ! printf '%s\n' "$VERSION" | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { valid = 1 } END { exit !valid }'; then
		printf 'error: 无法识别 APP 版本 %s\n' "$VERSION" >&2
		exit 1
	fi
	asset="tizi-$os-$arch.tar.gz"
	base_url="$RELEASE_DOWNLOAD_ROOT/$VERSION"
	tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/tizi.XXXXXX")"
	printf '正在下载 APP %s (%s)...\n' "$VERSION" "$arch"
	asset_status="$(curl -sSL -o "$tmp_dir/$asset" -w '%{http_code}' "$base_url/$asset" || true)"
	case "$asset_status" in
	200)
		curl -fsSL "$base_url/checksums.txt" -o "$tmp_dir/checksums.txt"
		expected="$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1 }' "$tmp_dir/checksums.txt")"
		case "$expected" in
		[0-9a-fA-F][0-9a-fA-F]*) ;;
		*)
			printf 'error: checksums.txt 中没有 %s\n' "$asset" >&2
			exit 1
			;;
		esac
		actual="$($sha256_command "$tmp_dir/$asset" | awk '{print $1}')"
		if [ "$actual" != "$expected" ]; then
			printf 'error: APP 安装包 SHA-256 校验失败\n' >&2
			exit 1
		fi
		entries="$(tar -tzf "$tmp_dir/$asset")"
		if [ "$entries" != "tizi" ]; then
			printf 'error: APP 安装包内容无效\n' >&2
			exit 1
		fi
		tar -xzf "$tmp_dir/$asset" -C "$tmp_dir"
		cp "$tmp_dir/tizi" "$install_tmp"
		;;
	404)
		rm -f "$tmp_dir/$asset"
		if [ "$os" = "linux" ]; then
			printf 'error: APP %s 尚未提供 Ubuntu 安装包，请安装支持 Ubuntu 的新版本\n' "$VERSION" >&2
			exit 1
		fi
		command -v go >/dev/null 2>&1 || {
			printf 'error: APP %s 是旧版 Release，缺少预编译安装包；兼容安装需要 Go\n' "$VERSION" >&2
			exit 1
		}
		printf 'APP %s 尚未提供预编译包，回退到源码构建...\n' "$VERSION"
		source_url="https://github.com/kadaliao/Tizi/archive/refs/tags/$VERSION.tar.gz"
		curl -fsSL "$source_url" -o "$tmp_dir/source.tar.gz"
		mkdir -p "$tmp_dir/source"
		tar -xzf "$tmp_dir/source.tar.gz" -C "$tmp_dir/source" --strip-components=1
		(
			cd "$tmp_dir/source"
			GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$install_tmp" ./cmd/tizi
		)
		;;
	*)
		printf 'error: 下载 APP 安装包失败（HTTP %s）\n' "$asset_status" >&2
		exit 1
		;;
	esac
fi

chmod 755 "$install_tmp"
reported_version="$("$install_tmp" version 2>/dev/null || true)"
case " $reported_version " in
*" $VERSION "*) ;;
*)
	printf 'error: 新程序版本校验失败: %s\n' "$reported_version" >&2
	exit 1
	;;
esac
"$install_tmp" init >/dev/null
mv -f "$install_tmp" "$BIN_DIR/tizi"
install_tmp=""

printf '安装完成: %s/tizi (%s)\n' "$BIN_DIR" "$VERSION"

# install_desktop 下载并校验 Release 中的桌面版，解压到 APP_DIR 下的隐藏暂存
# 目录后整包替换。它在 || 中调用，set -e 不生效，因此每一步都显式检查。
install_desktop() {
	desktop_zip="$tmp_dir/$DESKTOP_ASSET"
	curl -fsSL "$base_url/$DESKTOP_ASSET" -o "$desktop_zip" || return 1
	expected="$(awk -v name="$DESKTOP_ASSET" '$2 == name || $2 == "*" name { print $1 }' "$tmp_dir/checksums.txt")"
	actual="$($sha256_command "$desktop_zip" | awk '{print $1}')"
	if [ -z "$expected" ] || [ "$actual" != "$expected" ]; then
		printf 'error: 桌面版安装包 SHA-256 校验失败\n' >&2
		return 1
	fi
	mkdir -p "$APP_DIR" || return 1
	desktop_stage="$(mktemp -d "$APP_DIR/.TiziDesktop.install.XXXXXX")" || return 1
	ditto -x -k "$desktop_zip" "$desktop_stage" || return 1
	if [ ! -x "$desktop_stage/$DESKTOP_APP/Contents/MacOS/TiziDesktop" ]; then
		printf 'error: 桌面版安装包内容无效\n' >&2
		return 1
	fi
	target="$APP_DIR/$DESKTOP_APP"
	if [ -e "$target" ]; then
		mv "$target" "$desktop_stage/previous.app" || return 1
	fi
	if ! mv "$desktop_stage/$DESKTOP_APP" "$target"; then
		[ ! -e "$desktop_stage/previous.app" ] || mv "$desktop_stage/previous.app" "$target"
		return 1
	fi
	rm -rf "$desktop_stage"
	desktop_stage=""
	printf '桌面版已安装: %s\n' "$target"
	if pgrep -f "$target/Contents/MacOS/TiziDesktop" >/dev/null 2>&1; then
		printf '桌面版正在运行，请退出后重新打开以使用新版本\n'
	else
		printf '打开桌面版: open "%s"\n' "$target"
	fi
}

if [ "$os" = "darwin" ] && [ "$INSTALL_DESKTOP" != "0" ]; then
	if [ -n "$source_build" ]; then
		printf '从源码安装桌面版：在项目目录执行 make install-gui（需要 Xcode）\n'
	elif [ -f "$tmp_dir/checksums.txt" ] &&
		awk -v name="$DESKTOP_ASSET" '$2 == name || $2 == "*" name { found = 1 } END { exit !found }' "$tmp_dir/checksums.txt"; then
		install_desktop || printf 'warning: 桌面版安装失败，终端版已安装完成；可稍后重新运行安装命令\n' >&2
	fi
fi
case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*) printf '请将 %s 加入 PATH\n' "$BIN_DIR" ;;
esac
