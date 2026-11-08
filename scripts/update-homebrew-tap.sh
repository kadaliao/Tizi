#!/bin/sh
# 把 Homebrew tap（kadaliao/homebrew-tap）里的 tizi（命令行）和 tizi-desktop
# （桌面版）更新到指定版本：从 GitHub Release 读取 checksums.txt，生成
# Formula/tizi.rb 和 Casks/tizi-desktop.rb，提交并推送。在 gh release create
# 之后执行。
#
# 用法：scripts/update-homebrew-tap.sh vMAJOR.MINOR.PATCH
# 环境变量：
#   TAP_DIR  本地的 tap 仓库；不设置时临时克隆一份，用完删除
#   PUSH=0   只提交，不推送（配合 TAP_DIR 先检查再手动推送）
set -eu

VERSION="${1:-}"
if ! printf '%s\n' "$VERSION" | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { valid = 1 } END { exit !valid }'; then
	printf 'usage: %s vMAJOR.MINOR.PATCH\n' "$0" >&2
	exit 2
fi
version="${VERSION#v}"
repo="kadaliao/Tizi"
tap_repo="kadaliao/homebrew-tap"

work="$(mktemp -d "${TMPDIR:-/tmp}/tizi-tap.XXXXXX")"
trap 'rm -rf "$work"' EXIT HUP INT TERM

gh release download "$VERSION" --repo "$repo" --pattern checksums.txt --output "$work/checksums.txt"
sha() {
	value="$(awk -v name="$1" '$2 == name || $2 == "*" name { print $1 }' "$work/checksums.txt")"
	if ! printf '%s\n' "$value" | awk '/^[0-9a-f]{64}$/ { valid = 1 } END { exit !valid }'; then
		printf 'error: %s 的 checksums.txt 里没有 %s\n' "$VERSION" "$1" >&2
		exit 1
	fi
	printf '%s' "$value"
}
darwin_arm64="$(sha tizi-darwin-arm64.tar.gz)"
darwin_amd64="$(sha tizi-darwin-amd64.tar.gz)"
linux_amd64="$(sha tizi-linux-amd64.tar.gz)"
desktop="$(sha TiziDesktop-macos.zip)"

tap="${TAP_DIR:-}"
if [ -z "$tap" ]; then
	tap="$work/tap"
	gh repo clone "$tap_repo" "$tap" -- --quiet --depth 1
fi
mkdir -p "$tap/Formula" "$tap/Casks"

# 与 install.sh、tizi update 用同一组 Release 文件；改名时三处一起改。
cat >"$tap/Formula/tizi.rb" <<EOF
class Tizi < Formula
  desc "填好机场订阅就能用：国内直连、海外走代理（基于 Mihomo）"
  homepage "https://github.com/$repo"
  # 默认是 Linux 的包，Mac 在 on_macos 里按芯片换成自己的。
  url "https://github.com/$repo/releases/download/v$version/tizi-linux-amd64.tar.gz"
  version "$version"
  sha256 "$linux_amd64"
  license "MIT"

  livecheck do
    url :stable
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/$repo/releases/download/v#{version}/tizi-darwin-arm64.tar.gz"
      sha256 "$darwin_arm64"
    end
    on_intel do
      url "https://github.com/$repo/releases/download/v#{version}/tizi-darwin-amd64.tar.gz"
      sha256 "$darwin_amd64"
    end
  end

  on_linux do
    depends_on arch: :x86_64
  end

  def install
    bin.install "tizi"
  end

  def caveats
    <<~EOS
      更新请用 brew upgrade tizi。
      Mac 上想要桌面版：brew install --cask kadaliao/tap/tizi-desktop
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/tizi version")
  end
end
EOF

cat >"$tap/Casks/tizi-desktop.rb" <<EOF
cask "tizi-desktop" do
  version "$version"
  sha256 "$desktop"

  url "https://github.com/$repo/releases/download/v#{version}/TiziDesktop-macos.zip"
  name "Tizi"
  desc "填好机场订阅点「连接」：国内直连、海外走代理（基于 Mihomo）"
  homepage "https://github.com/$repo"

  livecheck do
    url :url
    strategy :github_latest
  end

  # 应用内自己更新（连同 ~/.local/bin/tizi 一起），brew upgrade 默认跳过。
  auto_updates true
  depends_on macos: :ventura

  app "TiziDesktop.app"

  # 应用只做了 ad-hoc 签名、没有 Apple 公证。去掉 Homebrew 加的隔离属性，
  # 和 install.sh 装的一样能直接打开，不用去「隐私与安全性」点「仍要打开」。
  postflight_steps do
    run "/usr/bin/xattr",
        args:           ["-dr", "com.apple.quarantine", "{{appdir}}/TiziDesktop.app"],
        writable_paths: ["{{appdir}}/TiziDesktop.app"]
  end

  uninstall quit: "com.kadaliao.tizi.desktop"

  # 首次打开时应用会把自带的 tizi 装到 ~/.local/bin，一并删掉。
  zap trash: [
    "~/.config/tizi",
    "~/.local/bin/tizi",
    "~/.local/share/tizi",
    "~/Library/Preferences/com.kadaliao.tizi.desktop.plist",
  ]
end
EOF

git -C "$tap" add Formula/tizi.rb Casks/tizi-desktop.rb
if git -C "$tap" diff --cached --quiet; then
	printf 'tap 里已经是 %s，无需更新\n' "$VERSION"
	exit 0
fi
git -C "$tap" commit --quiet -m "tizi $version"
if [ "${PUSH:-1}" = "0" ]; then
	printf '已提交到 %s，未推送\n' "$tap"
	exit 0
fi
git -C "$tap" push --quiet
printf 'Homebrew tap 已更新到 %s\n' "$VERSION"
