package tracescope

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// parseNodeMajor 从 `node --version` 输出解析主版本号，如 "v20.11.1" -> 20。
func parseNodeMajor(version string) (int, error) {
	v := strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if v == "" {
		return 0, fmt.Errorf("无法解析 Node 版本 %q", version)
	}
	parts := strings.SplitN(v, ".", 2)
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("无法解析 Node 版本 %q", version)
	}
	return major, nil
}

// nodeMajorVersion 运行 `node --version` 并返回主版本号。
func nodeMajorVersion() (int, error) {
	cmd := exec.Command("node", "--version")
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return parseNodeMajor(string(output))
}

// ensureNode 检查 Node.js（>=20）与 npm 是否可用，缺失时给出安装提示。
func ensureNode(out io.Writer) error {
	major, err := nodeMajorVersion()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || os.IsNotExist(err) {
			return fmt.Errorf("未找到 Node.js：TraceScope 看板需要 Node.js 20+。macOS 可执行 `brew install node`；Ubuntu 建议使用 NodeSource 或 nvm 安装 Node.js 20+")
		}
		return fmt.Errorf("检查 Node.js 版本: %w", err)
	}
	if major < 20 {
		return fmt.Errorf("TraceScope 看板需要 Node.js 20+，当前为 %d。请升级 Node.js 后重试", major)
	}
	if _, err := exec.LookPath("npm"); err != nil {
		return fmt.Errorf("未找到 npm：请安装随 Node.js 分发的 npm 后重试")
	}
	return nil
}

// runNpm 在 dir 下执行 npm 子命令，输出透传到终端。
func runNpm(ctx context.Context, dir string, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "npm", args...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// depsMarker 是 npm 在依赖安装（reify）完成时才写入的文件；只看
// node_modules 目录存在会把被中断的安装误判为已完成。
const depsMarker = "node_modules/.package-lock.json"

// ensureDeps 在依赖未完整安装时执行 `npm ci`（失败回退 `npm install`）。
func ensureDeps(ctx context.Context, dir string, out io.Writer) error {
	if _, err := os.Stat(filepath.Join(dir, depsMarker)); err == nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		fmt.Fprintln(out, "看板依赖不完整（上次安装可能被中断），重新安装...")
	} else {
		fmt.Fprintln(out, "安装看板依赖（首次运行，视网络约 1–2 分钟）...")
	}
	if err := runNpm(ctx, dir, out, "ci", "--no-audit", "--no-fund"); err != nil {
		fmt.Fprintln(out, "npm ci 失败，回退 npm install ...")
		if err2 := runNpm(ctx, dir, out, "install", "--no-audit", "--no-fund"); err2 != nil {
			return fmt.Errorf("安装看板依赖失败: %v（可稍后重试 tizi tracescope）", err2)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, depsMarker)); err != nil {
		return fmt.Errorf("安装看板依赖后未生成 %s，安装可能不完整（可稍后重试 tizi tracescope）", depsMarker)
	}
	return nil
}

// ensureBuild 在缺少构建产物或构建产物对应的源码哈希与当前解包源码不一致时
// 执行 `npm run build`。构建成功后才写入 dist/.built 标记；被中断的构建不会
// 留下标记，下次运行自动重建。
func ensureBuild(ctx context.Context, dir string, out io.Writer) error {
	digest, err := extractedDigest(dir)
	if err != nil {
		return err
	}
	markerPath := filepath.Join(dir, buildMarker)
	if data, err := os.ReadFile(markerPath); err == nil && strings.TrimSpace(string(data)) == digest {
		if _, err := os.Stat(filepath.Join(dir, "dist", "index.html")); err == nil {
			return nil
		}
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理旧构建标记: %w", err)
	}
	fmt.Fprintln(out, "构建看板前端（首次运行或源码已更新）...")
	if err := runNpm(ctx, dir, out, "run", "build"); err != nil {
		return fmt.Errorf("构建看板前端失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dist", "index.html")); err != nil {
		return fmt.Errorf("构建看板前端后缺少 dist/index.html: %v", err)
	}
	if err := os.WriteFile(markerPath, []byte(digest), 0o644); err != nil {
		return fmt.Errorf("写入构建标记: %w", err)
	}
	return nil
}
