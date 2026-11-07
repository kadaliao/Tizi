# 参与贡献

感谢你愿意改进 Tizi。提 Issue、修文档、修 bug、加功能都欢迎。

## 开始之前

- 报 bug 或提需求请用 [Issue 模板](https://github.com/kadaliao/Tizi/issues/new/choose)。贴日志和诊断输出前，先删掉订阅地址、节点地址和密码、Auth Key 和你的公网 IP。
- 改动较大（新功能、改变默认分流、改配置格式）时，先开 Issue 讨论方案，再写代码。
- 安全问题不要公开提 Issue，按 [SECURITY.md](SECURITY.md) 私下报告。

## 开发环境

- Go 1.26+（以 `go.mod` 为准）
- 桌面版需要 Xcode（Swift 5.9+）

```sh
make test               # go test ./...
go test -race ./...
make lint               # go vet + staticcheck
gofmt -l cmd internal   # 应该没有输出
cd macos && swift build # 改了桌面版时
```

更多说明见 [docs/development.md](docs/development.md) 和 [macos/README.md](macos/README.md)。

## 几条约定

- **测试不能碰真实网络环境**：不能启动 Mihomo、不能改系统 DNS / 路由 / 防火墙、不能需要 sudo。系统命令都通过可替换的执行器调用，测试里用假的实现。
- **不要提交任何凭据**：`.ovpn`、订阅地址、节点密码、Tailscale Auth Key、生成的 `config.yaml`。测试数据用 `example.com`、`203.0.113.0/24` 这类文档保留的域名和地址。
- **界面文字**用简体中文，写给没有网络基础的用户看：说清楚发生了什么、该怎么做，少用术语。
- 改了引擎协议（`internal/engine/protocol.go`）时，同步修改桌面版的 `Models.swift`；不兼容的改动要递增 `ProtocolVersion`。
- 改了用户能感知的行为，在 `CHANGELOG.md` 的 `[Unreleased]` 里记一笔。

## 提交和 PR

- 提交信息使用 [Conventional Commits](https://www.conventionalcommits.org/)：`fix(dns): ...`、`feat(tailscale): ...`、`docs: ...`、`chore(release): prepare v1.1.0`。
- 一个 PR 只做一件事，说明改了什么、为什么改、怎么验证的。
- CI（gofmt、vet、测试、staticcheck、桌面版构建）通过后再请求合并。
