// Package tracescope 将 vendored 的 TraceScope 看板
// （https://github.com/kadaliao/tracescope）内嵌进 tizi 二进制，
// 提供 `tizi tracescope` 子命令：一行命令在本机启动 3D 网络可视化
// 看板并自动打开浏览器。
package tracescope

import "embed"

//go:embed all:web
var webFS embed.FS
