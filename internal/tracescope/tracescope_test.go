package tracescope

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseNodeMajor(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "v20.11.1\n", want: 20},
		{in: "v25.2.1", want: 25},
		{in: "6.0.0\n", want: 6},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "v", wantErr: true},
	} {
		got, err := parseNodeMajor(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("parseNodeMajor(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if err == nil && got != tc.want {
			t.Fatalf("parseNodeMajor(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// fakeToolbox 在临时目录写入模拟的 node/npm 可执行脚本，并置顶 PATH。
func fakeToolbox(t *testing.T, tools map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake 脚本仅适用于 Unix")
	}
	bin := t.TempDir()
	for name, script := range tools {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

func TestEnsureNodeAcceptsModernNode(t *testing.T) {
	fakeToolbox(t, map[string]string{
		"node": "echo v20.11.1",
		"npm":  "echo 10.9.2",
	})
	if err := ensureNode(io.Discard); err != nil {
		t.Fatalf("ensureNode = %v", err)
	}
}

func TestEnsureNodeRejectsOldNode(t *testing.T) {
	fakeToolbox(t, map[string]string{
		"node": "echo v18.20.4",
		"npm":  "echo 10.0.0",
	})
	err := ensureNode(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "20") {
		t.Fatalf("期望 Node 版本过低错误，得到 %v", err)
	}
}

func TestEnsureNodeMissing(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	err := ensureNode(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Node.js") {
		t.Fatalf("期望缺少 Node.js 错误，得到 %v", err)
	}
}

// fakeNpm 写入模拟 npm：ci/install 生成 node_modules 与完成标记，run build 生成 dist。
// 参数 partialInstall 为 true 时模拟被中断的安装（只有目录、没有完成标记）。
func fakeNpm(t *testing.T, log string, partialInstall bool) {
	t.Helper()
	install := "mkdir -p node_modules && touch node_modules/.package-lock.json"
	if partialInstall {
		install = "mkdir -p node_modules"
	}
	fakeToolbox(t, map[string]string{
		"npm": `printf '%s\n' "$*" >> ` + log + "\n" +
			`case "$1" in` + "\n" +
			`  ci) rm -rf node_modules; ` + install + ` ;;` + "\n" +
			`  install) ` + install + ` ;;` + "\n" +
			`  run) if [ "$2" = build ]; then mkdir -p dist; echo '<div id="root"></div>' > dist/index.html; fi ;;` + "\n" +
			`esac`,
	})
}

func npmCalls(t *testing.T, log, pattern string) int {
	t.Helper()
	data, _ := os.ReadFile(log)
	return strings.Count(string(data), pattern)
}

func TestEnsureDepsUsesCompletionMarker(t *testing.T) {
	log := filepath.Join(t.TempDir(), "npm.log")
	fakeNpm(t, log, false)
	dir := t.TempDir()
	ctx := context.Background()

	if err := ensureDeps(ctx, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, depsMarker)); err != nil {
		t.Fatalf("安装完成标记未创建: %v", err)
	}
	if err := ensureDeps(ctx, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := npmCalls(t, log, "ci "); got != 1 {
		t.Fatalf("依赖完整时仍重新安装: npm ci 执行 %d 次", got)
	}

	// 只有 node_modules 目录、没有完成标记：视为中断的安装，重新执行 npm ci。
	if err := os.Remove(filepath.Join(dir, depsMarker)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ensureDeps(ctx, dir, &out); err != nil {
		t.Fatal(err)
	}
	if got := npmCalls(t, log, "ci "); got != 2 {
		t.Fatalf("残缺 node_modules 未触发重装: npm ci 执行 %d 次", got)
	}
	if !strings.Contains(out.String(), "不完整") {
		t.Fatalf("重装提示缺失: %q", out.String())
	}
}

func TestEnsureDepsRejectsIncompleteInstall(t *testing.T) {
	log := filepath.Join(t.TempDir(), "npm.log")
	fakeNpm(t, log, true)
	err := ensureDeps(context.Background(), t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), depsMarker) {
		t.Fatalf("npm 未写完成标记时应报错，得到 %v", err)
	}
}

func TestEnsureBuildUsesDigestMarker(t *testing.T) {
	log := filepath.Join(t.TempDir(), "npm.log")
	fakeNpm(t, log, false)
	dir := t.TempDir()
	ctx := context.Background()

	// 缺少解包标记时无法判定源码版本，直接报错。
	if err := ensureBuild(ctx, dir, io.Discard); err == nil {
		t.Fatal("缺少 .extracted 标记时应报错")
	}
	if err := os.WriteFile(filepath.Join(dir, extractedMarker), []byte("digest-a"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 无构建标记：构建并写入标记。
	if err := ensureBuild(ctx, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dist", "index.html")); err != nil {
		t.Fatalf("dist 未构建: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, buildMarker)); string(data) != "digest-a" {
		t.Fatalf("构建标记 = %q, want digest-a", data)
	}
	if got := npmCalls(t, log, "run build"); got != 1 {
		t.Fatalf("build 执行次数 = %d, want 1", got)
	}

	// 标记与源码哈希一致：跳过（即便 dist 时间戳很旧）。
	past := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "dist", "index.html"), past, past)
	if err := ensureBuild(ctx, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := npmCalls(t, log, "run build"); got != 1 {
		t.Fatalf("标记一致时仍重建: %d 次", got)
	}

	// 源码哈希变化（新版本二进制解包了新源码）：重建。
	if err := os.WriteFile(filepath.Join(dir, extractedMarker), []byte("digest-b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureBuild(ctx, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := npmCalls(t, log, "run build"); got != 2 {
		t.Fatalf("源码更新后未重建: %d 次", got)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, buildMarker)); string(data) != "digest-b" {
		t.Fatalf("重建后标记 = %q, want digest-b", data)
	}

	// 标记缺失（上次构建被中断）：即便 dist/index.html 存在也重建。
	if err := os.Remove(filepath.Join(dir, buildMarker)); err != nil {
		t.Fatal(err)
	}
	if err := ensureBuild(ctx, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := npmCalls(t, log, "run build"); got != 3 {
		t.Fatalf("标记缺失时未重建: %d 次", got)
	}
}

func TestEnsureBuildFailureLeavesNoMarker(t *testing.T) {
	log := filepath.Join(t.TempDir(), "npm.log")
	fakeToolbox(t, map[string]string{
		"npm": `printf '%s\n' "$*" >> ` + log + "; exit 1",
	})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, extractedMarker), []byte("digest-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureBuild(context.Background(), dir, io.Discard); err == nil {
		t.Fatal("npm 失败时应返回错误")
	}
	if _, err := os.Stat(filepath.Join(dir, buildMarker)); !os.IsNotExist(err) {
		t.Fatalf("构建失败不应留下标记: %v", err)
	}
}

func TestExtractWebToCache(t *testing.T) {
	cache := t.TempDir()
	userCacheDir = func() (string, error) { return cache, nil }
	t.Cleanup(func() { userCacheDir = os.UserCacheDir })

	var out bytes.Buffer
	dir, err := ensureWeb(&out)
	if err != nil {
		t.Fatal(err)
	}
	version, err := vendorVersion()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cache, "tizi", "tracescope", "tracescope-"+version)
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	for _, f := range []string{"package.json", "package-lock.json", "server/index.mjs", "src/App.tsx", "public/countries-110m.json"} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("缺少解包文件 %s: %v", f, err)
		}
		// embed.FS 的权限是 0444；落盘必须可写，否则 npm 无法更新 lockfile。
		if got := info.Mode().Perm(); got != 0o644 {
			t.Fatalf("解包文件 %s 权限 = %o, want 644", f, got)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "web")); !os.IsNotExist(err) {
		t.Fatalf("解包目录不应包含多余的 web/ 子目录: %v", err)
	}
	if !strings.Contains(out.String(), "首次运行") {
		t.Fatalf("首次解包应输出提示: %q", out.String())
	}

	// 已解包时直接复用，不重复输出提示。
	out.Reset()
	dir2, err := ensureWeb(&out)
	if err != nil {
		t.Fatal(err)
	}
	if dir2 != dir {
		t.Fatalf("重复调用目录不一致: %q vs %q", dir2, dir)
	}
	if out.Len() != 0 {
		t.Fatalf("重复调用不应重新解包: %q", out.String())
	}

	// 伪造一个旧版本目录；标记损坏触发重建时应清理旧目录。
	root := filepath.Join(cache, "tizi", "tracescope")
	stale := filepath.Join(root, "tracescope-deadbeef")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, ".extracted")
	if err := os.WriteFile(marker, []byte("bogus"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	dir3, err := ensureWeb(&out)
	if err != nil {
		t.Fatal(err)
	}
	if dir3 != dir {
		t.Fatalf("重建后目录不一致: %q vs %q", dir3, dir)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("旧版本目录未被清理: %v", err)
	}
	digest, err := webDigest()
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(marker); string(data) != digest {
		t.Fatalf("重建后标记 = %q, want %q", data, digest)
	}
}
