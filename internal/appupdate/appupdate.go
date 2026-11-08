package appupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	latestReleaseURL = "https://api.github.com/repos/kadaliao/Tizi/releases/latest"
	releaseTagURL    = "https://api.github.com/repos/kadaliao/Tizi/releases/tags/%s"
	maxArchiveSize   = 32 << 20
	maxBinarySize    = 64 << 20
	maxChecksumSize  = 1 << 20

	// DesktopAppName 是 macOS 桌面版的 bundle 名，desktopAsset 是 Release 中的安装包。
	DesktopAppName        = "TiziDesktop.app"
	desktopAsset          = "TiziDesktop-macos.zip"
	desktopBundleID       = "com.kadaliao.tizi.desktop"
	maxDesktopArchiveSize = 64 << 20
)

type Status struct {
	Current   string
	Latest    string
	Available bool
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

func Check(ctx context.Context, current string) (Status, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	return check(ctx, client, latestReleaseURL, current)
}

func check(ctx context.Context, client *http.Client, endpoint, current string) (Status, error) {
	latest, err := fetchRelease(ctx, client, endpoint)
	if err != nil {
		return Status{}, fmt.Errorf("查询 APP 最新版本: %w", err)
	}
	if _, ok := parseVersion(latest.TagName); !ok {
		return Status{}, fmt.Errorf("APP 最新版本标签无效: %q", latest.TagName)
	}
	return Status{
		Current:   displayVersion(current),
		Latest:    latest.TagName,
		Available: isNewer(latest.TagName, current),
	}, nil
}

// Result 描述一次更新实际替换了什么。
type Result struct {
	Version string
	// DesktopApp 是同步更新的桌面版位置；没有安装桌面版或 Release 未提供安装包时为空。
	DesktopApp string
	// DesktopError 非空表示命令行已更新，但桌面版没有更新成功，原来的桌面版保持不变。
	DesktopError string
}

// DesktopNote 返回给用户看的桌面版更新结果；没有安装桌面版时为空。
func (r Result) DesktopNote() string {
	switch {
	case r.DesktopApp != "":
		return "桌面版已同步更新，正在运行的桌面版需要退出后重新打开"
	case r.DesktopError != "":
		return "桌面版未能更新，保持原版本：" + r.DesktopError
	default:
		return ""
	}
}

// Install 把命令行程序更新到 version；如果装有桌面版，同时把它更新到同一版本，
// 让 TUI、命令行和桌面版始终使用同一套核心代码。
func Install(ctx context.Context, version string) (Result, error) {
	executable, err := ExecutablePath()
	if err != nil {
		return Result{}, err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	return install(ctx, version, fmt.Sprintf(releaseTagURL, version), executable, DesktopAppPath(), client)
}

// DesktopAppPath 返回已安装桌面版的真实路径：~/Applications 优先，其次
// /Applications；未安装或不是 macOS 时返回空。开发时从仓库 bin/ 运行的构建
// 不在这两个位置，不会被更新覆盖。
func DesktopAppPath() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Applications", DesktopAppName))
	}
	candidates = append(candidates, filepath.Join("/Applications", DesktopAppName))
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, "Contents", "Info.plist")); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			return resolved
		}
	}
	return ""
}

// ExecutablePath 返回 Install 替换的真实文件路径。更新完成后当前进程仍在
// 运行旧代码，需要新版本生效的操作应改为执行这个路径上的新二进制。
func ExecutablePath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位当前 APP: %w", err)
	}
	return resolveExecutable(executable)
}

// resolveExecutable 把当前程序路径解析成真实文件：用户可能通过软链把
// 仓库构建链接到 ~/.local/bin，直接 rename 会把软链替换成普通文件，
// 真正的目标仍是旧版本。
func resolveExecutable(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析当前 APP 路径: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("解析当前 APP 真实路径: %w", err)
	}
	return resolved, nil
}

// ErrHomebrew 表示命令行程序由 Homebrew 安装，要用 brew 更新。
var ErrHomebrew = errors.New("tizi 是用 Homebrew 安装的，请运行 brew upgrade tizi 更新")

// homebrewManaged 判断真实路径是否在 Homebrew 的 Cellar 里。自己替换这个文件
// 会让 brew 记录的版本和实际不一致，所以交给 brew upgrade。
func homebrewManaged(executable string) bool {
	return strings.Contains(filepath.ToSlash(executable), "/Cellar/tizi/")
}

// FromHomebrew 报告当前运行的程序是否由 Homebrew 安装。
func FromHomebrew() bool {
	executable, err := ExecutablePath()
	return err == nil && homebrewManaged(executable)
}

func install(ctx context.Context, version, releaseURL, executable, desktopApp string, client *http.Client) (Result, error) {
	if homebrewManaged(executable) {
		return Result{}, ErrHomebrew
	}
	if _, ok := parseVersion(version); !ok {
		return Result{}, fmt.Errorf("APP 更新版本标签无效: %q", version)
	}
	latest, err := fetchRelease(ctx, client, releaseURL)
	if err != nil {
		return Result{}, fmt.Errorf("查询 APP %s 发布资源: %w", version, err)
	}
	if latest.TagName != version {
		return Result{}, fmt.Errorf("APP 发布标签不匹配: 得到 %q，期望 %q", latest.TagName, version)
	}
	archiveAsset, checksumAsset, err := selectAssets(latest, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return Result{}, err
	}
	checksums, err := download(ctx, client, checksumAsset.URL, maxChecksumSize, "APP 校验文件")
	if err != nil {
		return Result{}, err
	}
	expectedDigest, err := checksumFor(checksums, archiveAsset.Name)
	if err != nil {
		return Result{}, err
	}
	archive, err := download(ctx, client, archiveAsset.URL, maxArchiveSize, "APP 安装包")
	if err != nil {
		return Result{}, err
	}
	actualDigest := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(actualDigest[:]), expectedDigest) {
		return Result{}, fmt.Errorf("APP 安装包校验失败: SHA-256 不匹配")
	}
	binary, err := extractBinary(archive)
	if err != nil {
		return Result{}, err
	}

	// 桌面版先下载、校验并解压到目标旁边，命令行替换成功后再原子换入；
	// 任何一步失败都只影响桌面版，命令行照常更新。
	result := Result{Version: version}
	stagedDesktop := ""
	if desktopApp != "" {
		if asset, ok := findAsset(latest, desktopAsset); ok {
			staged, cleanup, err := stageDesktop(ctx, client, asset, checksums, desktopApp)
			if err != nil {
				result.DesktopError = err.Error()
			} else {
				defer cleanup()
				stagedDesktop = staged
			}
		}
	}
	if err := replaceExecutable(ctx, version, executable, binary); err != nil {
		return Result{}, err
	}
	if stagedDesktop != "" {
		if err := replaceDesktop(stagedDesktop, desktopApp); err != nil {
			result.DesktopError = err.Error()
		} else {
			result.DesktopApp = desktopApp
		}
	}
	return result, nil
}

// replaceExecutable 原子替换命令行程序，并用新程序自报的版本确认替换成功，
// 否则回滚到更新前的文件。
func replaceExecutable(ctx context.Context, version, executable string, binary []byte) (returnErr error) {
	stage, err := os.CreateTemp(filepath.Dir(executable), ".tizi.update-*")
	if err != nil {
		return fmt.Errorf("创建 APP 更新文件: %w", err)
	}
	stagePath := stage.Name()
	defer os.Remove(stagePath)
	if err := stage.Chmod(0o755); err != nil {
		_ = stage.Close()
		return fmt.Errorf("设置 APP 更新文件权限: %w", err)
	}
	if _, err := stage.Write(binary); err != nil {
		_ = stage.Close()
		return fmt.Errorf("写入 APP 更新文件: %w", err)
	}
	if err := stage.Sync(); err != nil {
		_ = stage.Close()
		return fmt.Errorf("同步 APP 更新文件: %w", err)
	}
	if err := stage.Close(); err != nil {
		return fmt.Errorf("关闭 APP 更新文件: %w", err)
	}

	backupPath, err := backupExecutable(executable)
	if err != nil {
		return err
	}
	defer func() {
		if returnErr == nil {
			_ = os.Remove(backupPath)
			return
		}
		if rollbackErr := os.Rename(backupPath, executable); rollbackErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("恢复更新前 APP: %w", rollbackErr))
		}
	}()
	if err := os.Rename(stagePath, executable); err != nil {
		return fmt.Errorf("替换 APP: %w", err)
	}

	versionOutput, err := exec.CommandContext(ctx, executable, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("验证 APP %s: %s", version, commandDetail(string(versionOutput), err))
	}
	if !reportsVersion(string(versionOutput), version) {
		return fmt.Errorf("验证 APP %s: 新程序报告 %q", version, strings.TrimSpace(string(versionOutput)))
	}
	return nil
}

func findAsset(latest release, name string) (releaseAsset, bool) {
	for _, asset := range latest.Assets {
		if asset.Name == name && asset.URL != "" {
			return asset, true
		}
	}
	return releaseAsset{}, false
}

// stageDesktop 下载并校验桌面版安装包，解压到 target 同目录的隐藏暂存目录，
// 保证之后的替换是同一文件系统内的 rename。返回暂存的 .app 与清理函数。
func stageDesktop(ctx context.Context, client *http.Client, asset releaseAsset, checksums []byte, target string) (string, func(), error) {
	expected, err := checksumFor(checksums, asset.Name)
	if err != nil {
		return "", nil, err
	}
	archive, err := download(ctx, client, asset.URL, maxDesktopArchiveSize, "桌面版安装包")
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), expected) {
		return "", nil, fmt.Errorf("桌面版安装包校验失败: SHA-256 不匹配")
	}
	stageDir, err := os.MkdirTemp(filepath.Dir(target), ".TiziDesktop.update-*")
	if err != nil {
		return "", nil, fmt.Errorf("创建桌面版暂存目录: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stageDir) }
	zipPath := filepath.Join(stageDir, asset.Name)
	if err := os.WriteFile(zipPath, archive, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("写入桌面版安装包: %w", err)
	}
	// ditto 按 macOS 的方式还原 bundle 的权限和属性，与打包时的 ditto -c 对应。
	if output, err := exec.CommandContext(ctx, "ditto", "-x", "-k", zipPath, stageDir).CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("解压桌面版安装包: %s", commandDetail(string(output), err))
	}
	_ = os.Remove(zipPath)
	staged := filepath.Join(stageDir, DesktopAppName)
	if err := validateDesktopBundle(stageDir, staged); err != nil {
		cleanup()
		return "", nil, err
	}
	return staged, cleanup, nil
}

// validateDesktopBundle 确认安装包只包含预期的 .app，且主程序和 bundle 标识正确。
func validateDesktopBundle(stageDir, app string) error {
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		return fmt.Errorf("读取桌面版暂存目录: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != DesktopAppName || !entries[0].IsDir() {
		return fmt.Errorf("桌面版安装包内容无效")
	}
	info, err := os.Stat(filepath.Join(app, "Contents", "MacOS", "TiziDesktop"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("桌面版安装包缺少主程序")
	}
	plist, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil || !strings.Contains(string(plist), "<string>"+desktopBundleID+"</string>") {
		return fmt.Errorf("桌面版安装包的 bundle 标识无效")
	}
	return nil
}

// replaceDesktop 用暂存的 .app 替换 target，失败时恢复原来的桌面版。
func replaceDesktop(staged, target string) error {
	backupDir, err := os.MkdirTemp(filepath.Dir(target), ".TiziDesktop.backup-*")
	if err != nil {
		return fmt.Errorf("创建桌面版备份目录: %w", err)
	}
	backup := filepath.Join(backupDir, DesktopAppName)
	if err := os.Rename(target, backup); err != nil {
		_ = os.RemoveAll(backupDir)
		return fmt.Errorf("备份原桌面版: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		if rollbackErr := os.Rename(backup, target); rollbackErr != nil {
			return errors.Join(fmt.Errorf("替换桌面版: %w", err), fmt.Errorf("恢复原桌面版（备份在 %s）: %w", backup, rollbackErr))
		}
		_ = os.RemoveAll(backupDir)
		return fmt.Errorf("替换桌面版: %w", err)
	}
	_ = os.RemoveAll(backupDir)
	return nil
}

func fetchRelease(ctx context.Context, client *http.Client, endpoint string) (release, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "tizi")
	// 未认证的 api.github.com 每小时只有 60 次请求；设置了 GITHUB_TOKEN 时带上。
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" && request.URL.Host == "api.github.com" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return release{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("HTTP %s%s", response.Status, rateLimitDetail(response))
	}
	var result release
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return release{}, err
	}
	return result, nil
}

func selectAssets(latest release, goos, arch string) (releaseAsset, releaseAsset, error) {
	wanted := fmt.Sprintf("tizi-%s-%s.tar.gz", goos, arch)
	var archive, checksums releaseAsset
	for _, asset := range latest.Assets {
		switch asset.Name {
		case wanted:
			archive = asset
		case "checksums.txt":
			checksums = asset
		}
	}
	if archive.URL == "" {
		return releaseAsset{}, releaseAsset{}, fmt.Errorf("APP %s 没有适用于 %s/%s 的资源 %s", latest.TagName, goos, arch, wanted)
	}
	if checksums.URL == "" {
		return releaseAsset{}, releaseAsset{}, fmt.Errorf("APP %s 没有 checksums.txt", latest.TagName)
	}
	return archive, checksums, nil
}

func checksumFor(data []byte, name string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if len(fields[0]) != sha256.Size*2 {
			break
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			break
		}
		return strings.ToLower(fields[0]), nil
	}
	return "", fmt.Errorf("APP 校验文件中没有 %s 的有效 SHA-256", name)
}

func download(ctx context.Context, client *http.Client, endpoint string, limit int64, label string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "tizi")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("下载%s: %w", label, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载%s: HTTP %s", label, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取%s: %w", label, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("下载%s: 文件超过 %d MiB", label, limit>>20)
	}
	return data, nil
}

func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("解压 APP 安装包: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var binary []byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 APP 安装包: %w", err)
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		if header.Typeflag == tar.TypeDir && (name == "." || name == "./") {
			continue
		}
		if header.Typeflag != tar.TypeReg || name != "tizi" || binary != nil {
			return nil, fmt.Errorf("APP 安装包包含非预期条目 %q", header.Name)
		}
		if header.Size < 1 || header.Size > maxBinarySize {
			return nil, fmt.Errorf("APP 二进制大小无效: %d", header.Size)
		}
		binary, err = io.ReadAll(io.LimitReader(reader, maxBinarySize+1))
		if err != nil {
			return nil, fmt.Errorf("读取 APP 二进制: %w", err)
		}
		if int64(len(binary)) != header.Size {
			return nil, fmt.Errorf("APP 二进制内容不完整")
		}
	}
	if len(binary) == 0 {
		return nil, fmt.Errorf("APP 安装包中没有 tizi")
	}
	return binary, nil
}

func backupExecutable(executable string) (string, error) {
	source, err := os.Open(executable)
	if err != nil {
		return "", fmt.Errorf("备份更新前 APP: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return "", fmt.Errorf("读取更新前 APP: %w", err)
	}
	backup, err := os.CreateTemp(filepath.Dir(executable), ".tizi.backup-*")
	if err != nil {
		return "", fmt.Errorf("创建 APP 更新备份: %w", err)
	}
	backupPath := backup.Name()
	if err := backup.Chmod(info.Mode().Perm()); err != nil {
		_ = backup.Close()
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("设置 APP 更新备份权限: %w", err)
	}
	if _, err := io.Copy(backup, source); err != nil {
		_ = backup.Close()
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("写入 APP 更新备份: %w", err)
	}
	if err := backup.Sync(); err != nil {
		_ = backup.Close()
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("同步 APP 更新备份: %w", err)
	}
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("关闭 APP 更新备份: %w", err)
	}
	return backupPath, nil
}

// rateLimitDetail 在 GHE 返回 403/429 且带配额头时补充说明。
func rateLimitDetail(response *http.Response) string {
	if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusTooManyRequests {
		return ""
	}
	remaining := strings.TrimSpace(response.Header.Get("X-RateLimit-Remaining"))
	reset := strings.TrimSpace(response.Header.Get("X-RateLimit-Reset"))
	if remaining == "" && reset == "" {
		return ""
	}
	detail := "（API 配额"
	if remaining != "" {
		detail += "剩余 " + remaining
	}
	if unix, err := strconv.ParseInt(reset, 10, 64); err == nil && unix > 0 {
		detail += "，" + time.Unix(unix, 0).Local().Format("15:04") + " 重置"
	}
	return detail + "）"
}

func reportsVersion(output, version string) bool {
	for _, field := range strings.Fields(output) {
		if field == version {
			return true
		}
	}
	return false
}

func displayVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func isNewer(latest, current string) bool {
	latestParts, latestOK := parseVersion(latest)
	currentParts, currentOK := parseVersion(baseVersion(current))
	if !latestOK {
		return false
	}
	if !currentOK {
		return displayVersion(current) == "dev"
	}
	for index := range latestParts {
		if latestParts[index] != currentParts[index] {
			return latestParts[index] > currentParts[index]
		}
	}
	return false
}

// baseVersion 截取 git describe 版本串（如 v0.2.4-1-gabc123-dirty）或带
// 构建标记的版本串（如 v0.2.30+dns-privacy）的标签部分，让本地构建也能和
// 最新发布比较；本地构建领先于标签时视为不落后，不会被提示降级覆盖。
func baseVersion(version string) string {
	version = strings.TrimSpace(version)
	if index := strings.IndexAny(version, "-+"); index >= 0 {
		return version[:index]
	}
	return version
}

func parseVersion(version string) ([3]int, bool) {
	var result [3]int
	version = strings.TrimSpace(version)
	if !strings.HasPrefix(version, "v") {
		return result, false
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != len(result) {
		return result, false
	}
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || strconv.Itoa(value) != part {
			return [3]int{}, false
		}
		result[index] = value
	}
	return result, true
}

func commandDetail(output string, fallback error) string {
	detail := strings.TrimSpace(output)
	if detail == "" {
		return fallback.Error()
	}
	if len(detail) > 2000 {
		detail = detail[len(detail)-2000:]
	}
	return detail
}
