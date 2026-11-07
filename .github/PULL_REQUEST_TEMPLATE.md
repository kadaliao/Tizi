## 改了什么

<!-- 简要说明改动和原因；有关联的 Issue 写 Closes #123 -->

## 怎么验证的

<!-- 跑了哪些测试，手动试了哪些场景（订阅 / 节点文件 / 内网 / Tailscale / 桌面版 / 终端版） -->

## 检查清单

- [ ] `gofmt -l cmd internal` 没有输出，`make lint` 和 `go test -race ./...` 通过
- [ ] 改了桌面版时 `cd macos && swift build` 通过
- [ ] 测试不启动 Mihomo、不修改系统网络、不需要 sudo
- [ ] 没有提交任何凭据（订阅地址、`.ovpn`、节点密码、Auth Key、`config.yaml`）
- [ ] 用户能感知的改动已记入 `CHANGELOG.md` 的 `[Unreleased]`，相关文档已更新
