# Vendored TraceScope（内部说明，非上游文件）

本目录是 [kadaliao/tracescope](https://github.com/kadaliao/tracescope) 的
vendored 快照，由 `go:embed` 打进 `tizi` 二进制，用于
`tizi tracescope` 子命令（本机 3D 网络可视化看板）。

- 上游版本：`6d9c9715ec12cccd0533d6a65b1033936049965a`（见 `TRACESCOPE_VERSION`）
- 许可：上游为 ISC（见 `LICENSE`、`THIRD_PARTY_NOTICES.md`）

## 相对上游的本地改动（均为标注 `vendored patch` 的小补丁）

1. `server/index.mjs`
   - 托管 `dist/` 构建产物并做 SPA fallback：让单个 Node 进程同时提供
     页面、`/health` 与 `/ws`，一行命令即可访问。
   - Origin 白名单由固定 `4173|5173` 改为「服务端口（`PORT`）+ 4173 + 5173」，
     保证自托管页面能连 WebSocket。
   - 父进程看护：设置了 `TRACESCOPE_PARENT_PID` 时每 2 秒检查该进程是否存活，
     父进程（`tizi tracescope`）被强杀后自行退出，避免孤儿进程占用端口。
2. `src/useLiveFeed.ts`
   - WebSocket 地址从硬编码 `:8788` 改为 `window.location.host`（同源），
     使 `--port` / `TRACESCOPE_PORT` 生效时前端仍能连接。

> 注：历史上还曾在本目录改动前端（修复放大时城市标签消失、线路可点击、
> 城市名不截断、移除演示模式、中英双语）。这些现均已随上游
> `feat: clickable routes, guaranteed city labels, bilingual UI, remove demo mode`
> 合入（上游 `6d9c971`），本次重同步后不再作为本地补丁维护。

## 同步上游新版本

```sh
# 1) 取上游最新提交
UPSTREAM=$(git ls-remote https://github.com/kadaliao/tracescope.git HEAD | cut -f1)
# 2) 克隆并覆盖本目录（保留 VENDOR.md 与 TRACESCOPE_VERSION）
tmp=$(mktemp -d)
git clone --depth 1 https://github.com/kadaliao/tracescope.git "$tmp"
#    必须排除 .git：否则克隆出的嵌套仓库会被整份拷进 web/（不会进入
#    go:embed，但会污染工作树、干扰 git 与代码遍历工具）
rsync -a --delete \
  --exclude .git --exclude VENDOR.md --exclude TRACESCOPE_VERSION \
  "$tmp"/ internal/tracescope/web/
rm -rf "$tmp"
# 3) 重新应用上文「相对上游的本地改动」中列出的所有 `vendored patch`
#     （以注释与这里的小结为准，全部改在 `web/src`、`web/server`）
# 4) 更新 TRACESCOPE_VERSION 并提交
printf '%s\n' "$UPSTREAM" > internal/tracescope/web/TRACESCOPE_VERSION
```

## 注意

- **不要**在本目录执行 `npm install` / `npm run build` / `go build`：
  `go:embed all:web` 会原样打包目录内容，任何 `node_modules`/`dist`
  残留都会膨胀二进制。依赖安装与构建发生在运行时的缓存目录
  （`~/.cache/tizi/tracescope/tracescope-<version>/`）。
- 运行时缓存以标记文件判定有效：`.extracted`（解包源码哈希）、
  `node_modules/.package-lock.json`（npm 安装完成）、`dist/.built`
  （构建对应的源码哈希）。任一缺失或不一致都会自动重做对应步骤。
