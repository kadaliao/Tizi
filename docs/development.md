# 开发与发布

## 环境

- Go 1.26+（以 `go.mod` 为准）
- 桌面版：Xcode（Swift 5.9+），最低运行系统 macOS 13。架构和引擎协议见 [macos/README.md](../macos/README.md)。

## 构建和测试

```sh
make test                  # go test ./...
go test -race ./...
make lint                  # go vet + staticcheck
make build                 # bin/tizi
cd macos && swift build    # 桌面版调试构建
make install install-gui   # 安装到本机 ~/.local/bin 和 ~/Applications
```

测试和诊断不会启动 Mihomo，也不会接管系统网络；需要系统命令的地方都用假的命令执行器。新增测试也要遵守这一点。

## 目录结构

| 路径 | 内容 |
| --- | --- |
| `cmd/tizi` | 命令行入口 |
| `internal/config` | 设置、路径和校验 |
| `internal/generator` | 生成 Mihomo 配置（策略组、规则、DNS） |
| `internal/ovpn`、`internal/nodes`、`internal/claude` | 解析 `.ovpn`、节点文件、住宅节点 |
| `internal/manager`、`internal/core` | 启停 Mihomo、隧道恢复、下载核心 |
| `internal/system` | DNS、IPv6、TUN 等系统设置的接管和恢复 |
| `internal/engine` | 桌面版使用的 JSON 行协议引擎 |
| `internal/ui` | 终端版界面 |
| `internal/claudeguard`、`internal/claudeblock` | Claude 访问监测、直连保护 |
| `internal/tracescope` | 内嵌的 TraceScope 看板（`web/` 是 vendored 快照，见 `VENDOR.md`） |
| `macos/` | SwiftUI 桌面版 |
| `scripts/release.sh` | 本地发布构建 |

## 发布

发布在装有 Xcode 的 Mac 上本地构建（桌面版需要 Xcode），用 GitHub CLI 上传：

1. 把 `CHANGELOG.md` 的 `[Unreleased]` 改成新版本号和日期，并把 `README.md` 里「当前版本是」后面的版本号改成新版本，提交 `chore(release): prepare vX.Y.Z`。
2. 构建并发布：

```sh
VERSION=v1.0.0
go test -race ./...
make lint
make release VERSION="$VERSION"   # 产物在 dist/
git tag -a "$VERSION" -m "$VERSION"
git push --atomic origin main "refs/tags/$VERSION"
gh release create "$VERSION" dist/* --verify-tag --title "$VERSION" --notes-file /path/to/release-notes.md
```

`make release` 要求工作树干净、README 里的版本号与 `VERSION` 一致，产出 macOS（arm64、amd64）和 Linux amd64 的命令行包、桌面版 zip 和 dmg，以及 `checksums.txt`。`install.sh` 和 `tizi update` 按这些文件名下载，改名时要同步修改。

## 第三方组件

- [Mihomo](https://github.com/MetaCubeX/mihomo)（GPL-3.0）：不随 Tizi 分发，首次连接时从其官方 GitHub Release 下载并校验 SHA-256。
- GeoIP 数据来自 [MetaCubeX/meta-rules-dat](https://github.com/MetaCubeX/meta-rules-dat)，同样在运行时下载。
- `internal/tracescope/web/` 是 [TraceScope](https://github.com/kadaliao/tracescope) 的 vendored 快照，许可证见其目录下的 `LICENSE`（ISC）和 `THIRD_PARTY_NOTICES.md`。
- Go 依赖见 `go.mod`。
