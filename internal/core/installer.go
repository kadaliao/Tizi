package core

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kadaliao/tizi/internal/config"
)

const stableReleaseAPI = "https://api.github.com/repos/MetaCubeX/mihomo/releases/latest"
const alphaReleaseAPI = "https://api.github.com/repos/MetaCubeX/mihomo/releases/tags/Prerelease-Alpha"
const geoReleaseAPI = "https://api.github.com/repos/MetaCubeX/meta-rules-dat/releases/latest"

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

func Find(paths config.Paths) string {
	if info, err := os.Stat(paths.CoreBin); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
		return paths.CoreBin
	}
	if path, err := exec.LookPath("mihomo"); err == nil {
		return path
	}
	return ""
}

var (
	// versionTimeout 限制 `mihomo -v`；卡住的二进制不应拖死状态刷新。
	versionTimeout = 5 * time.Second
	// validateTimeout 限制 `mihomo -t`，配置校验会加载 GeoIP 数据库，给
	// 慢盘留一点余量。
	validateTimeout = 15 * time.Second
)

func Version(binary string) string {
	if binary == "" {
		return "未安装"
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "-v").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "不可用: 读取版本超时"
		}
		return "不可用: " + firstLine(string(output))
	}
	return firstLine(string(output))
}

func Validate(binary, configDir, configFile string) error {
	if binary == "" {
		return fmt.Errorf("未找到 Mihomo，请先安装核心")
	}
	ctx, cancel := context.WithTimeout(context.Background(), validateTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-t", "-d", configDir, "-f", configFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Mihomo 配置校验失败: 超过 %s 未完成", validateTimeout)
		}
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("Mihomo 配置校验失败: %s", detail)
	}
	return nil
}

// Phase 标识安装流程当前所处的阶段，供进度展示区分文案。
type Phase int32

const (
	PhaseResolve Phase = iota // 查询最新版本
	PhaseCore                 // 下载 Mihomo 核心
	PhaseGeoIP                // 下载 GeoIP 数据
)

// ProgressFunc 报告安装进度；total 为 0 表示长度未知。
type ProgressFunc func(phase Phase, downloaded, total int64)

type progressReader struct {
	reader     io.Reader
	phase      Phase
	total      int64
	downloaded int64
	report     ProgressFunc
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.downloaded += int64(n)
		if r.report != nil {
			r.report(r.phase, r.downloaded, r.total)
		}
	}
	return n, err
}

func Install(ctx context.Context, paths config.Paths, channel config.CoreChannel, onProgress ProgressFunc) (string, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return "", fmt.Errorf("当前安装器仅支持 macOS 和 Ubuntu")
	}
	if err := paths.Ensure(); err != nil {
		return "", err
	}
	if onProgress != nil {
		onProgress(PhaseResolve, 0, 0)
	}
	endpoint, err := channelReleaseAPI(channel)
	if err != nil {
		return "", err
	}
	response, err := getWithProxyFallback(ctx, endpoint, 12*time.Second)
	if err != nil {
		return "", fmt.Errorf("查询 Mihomo 最新版本: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("查询 Mihomo 最新版本: HTTP %s", response.Status)
	}
	var latest release
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&latest); err != nil {
		return "", fmt.Errorf("解析 Mihomo 版本信息: %w", err)
	}
	asset, version, err := selectCoreAsset(latest, runtime.GOOS, runtime.GOARCH, channel)
	if err != nil {
		return "", err
	}
	downloadURL, expectedDigest := asset.URL, asset.Digest
	if !strings.HasPrefix(expectedDigest, "sha256:") || len(expectedDigest) != len("sha256:")+64 {
		return "", fmt.Errorf("版本 %s 未提供有效的 SHA-256，拒绝安装", latest.TagName)
	}

	response, err = getWithProxyFallback(ctx, downloadURL, 12*time.Second)
	if err != nil {
		return "", fmt.Errorf("下载 Mihomo: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 Mihomo: HTTP %s", response.Status)
	}
	if onProgress != nil {
		onProgress(PhaseCore, 0, response.ContentLength)
	}
	hash := sha256.New()
	body := &progressReader{
		reader: io.LimitReader(response.Body, 100<<20),
		phase:  PhaseCore,
		total:  response.ContentLength,
		report: onProgress,
	}
	compressed := io.TeeReader(body, hash)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return "", fmt.Errorf("解压 Mihomo: %w", err)
	}
	defer gz.Close()
	tmp := paths.DownloadTmp
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return "", fmt.Errorf("创建 Mihomo 临时文件: %w", err)
	}
	_, copyErr := io.Copy(file, io.LimitReader(gz, 200<<20))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("写入 Mihomo: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("关闭 Mihomo 临时文件: %w", closeErr)
	}
	actualDigest := fmt.Sprintf("sha256:%x", hash.Sum(nil))
	if actualDigest != expectedDigest {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("Mihomo 下载校验失败: SHA-256 不匹配")
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("设置 Mihomo 权限: %w", err)
	}
	// Keep the currently installed core untouched until every network download
	// and integrity check needed by the new installation has succeeded.
	if err := EnsureGeoIP(ctx, paths, onProgress); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, paths.CoreBin); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("安装 Mihomo: %w", err)
	}
	return version, nil
}

func channelReleaseAPI(channel config.CoreChannel) (string, error) {
	switch channel {
	case config.CoreChannelStable:
		return stableReleaseAPI, nil
	case config.CoreChannelAlpha:
		return alphaReleaseAPI, nil
	default:
		return "", fmt.Errorf("不支持的 Mihomo 核心通道 %q", channel)
	}
}

func selectCoreAsset(latest release, goos, arch string, channel config.CoreChannel) (releaseAsset, string, error) {
	if goos != "darwin" && goos != "linux" {
		return releaseAsset{}, "", fmt.Errorf("不支持的平台 %s/%s", goos, arch)
	}
	if goos == "linux" && arch != "amd64" {
		return releaseAsset{}, "", fmt.Errorf("Ubuntu 首版仅支持 amd64")
	}
	assetPrefix := fmt.Sprintf("mihomo-%s-%s", goos, arch)
	assetKind := "标准"
	if goos == "linux" {
		assetPrefix += "-compatible"
		assetKind = "兼容"
	}
	if channel == config.CoreChannelStable {
		wanted := fmt.Sprintf("%s-%s.gz", assetPrefix, latest.TagName)
		for _, asset := range latest.Assets {
			if asset.Name == wanted {
				return asset, latest.TagName, nil
			}
		}
		return releaseAsset{}, "", fmt.Errorf("版本 %s 没有适用于 %s 的资源 %s", latest.TagName, arch, wanted)
	}
	if channel != config.CoreChannelAlpha {
		return releaseAsset{}, "", fmt.Errorf("不支持的 Mihomo 核心通道 %q", channel)
	}

	prefix := assetPrefix + "-alpha-"
	var matched *releaseAsset
	var matchedRevision string
	for i := range latest.Assets {
		asset := &latest.Assets[i]
		revision := strings.TrimSuffix(strings.TrimPrefix(asset.Name, prefix), ".gz")
		if strings.HasPrefix(asset.Name, prefix) && strings.HasSuffix(asset.Name, ".gz") && isHexRevision(revision) {
			if matched != nil {
				return releaseAsset{}, "", fmt.Errorf("Alpha 版本包含多个适用于 %s 的%s资源", arch, assetKind)
			}
			matched = asset
			matchedRevision = revision
		}
	}
	if matched == nil {
		return releaseAsset{}, "", fmt.Errorf("Alpha 版本没有适用于 %s 的%s资源", arch, assetKind)
	}
	return *matched, "alpha-" + matchedRevision, nil
}

func isHexRevision(value string) bool {
	if len(value) < 7 || len(value) > 40 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func EnsureGeoIP(ctx context.Context, paths config.Paths, onProgress ProgressFunc) error {
	target := filepath.Join(paths.ConfigDir, "geoip.metadb")
	digestFile := target + ".sha256"
	if expected, err := os.ReadFile(digestFile); err == nil {
		if actual, hashErr := fileDigest(target); hashErr == nil && actual == strings.TrimSpace(string(expected)) {
			return nil
		}
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	response, err := getWithProxyFallback(ctx, geoReleaseAPI, 12*time.Second)
	if err != nil {
		return fmt.Errorf("查询 GeoIP 最新版本: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("查询 GeoIP 最新版本: HTTP %s", response.Status)
	}
	var latest release
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&latest); err != nil {
		return fmt.Errorf("解析 GeoIP 版本信息: %w", err)
	}
	var downloadURL, expectedDigest string
	for _, asset := range latest.Assets {
		if asset.Name == "geoip.metadb" {
			downloadURL, expectedDigest = asset.URL, asset.Digest
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("GeoIP release %s 中没有 geoip.metadb", latest.TagName)
	}
	if !strings.HasPrefix(expectedDigest, "sha256:") || len(expectedDigest) != len("sha256:")+64 {
		return fmt.Errorf("GeoIP release %s 未提供有效的 SHA-256", latest.TagName)
	}
	response, err = getWithProxyFallback(ctx, downloadURL, 12*time.Second)
	if err != nil {
		return fmt.Errorf("下载 GeoIP: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 GeoIP: HTTP %s", response.Status)
	}
	tmp := target + ".download"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("创建 GeoIP 临时文件: %w", err)
	}
	if onProgress != nil {
		onProgress(PhaseGeoIP, 0, response.ContentLength)
	}
	body := &progressReader{
		reader: io.LimitReader(response.Body, 100<<20),
		phase:  PhaseGeoIP,
		total:  response.ContentLength,
		report: onProgress,
	}
	hash := sha256.New()
	_, copyErr := io.Copy(file, io.TeeReader(body, hash))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil {
			return fmt.Errorf("写入 GeoIP: %w", copyErr)
		}
		return fmt.Errorf("关闭 GeoIP 临时文件: %w", closeErr)
	}
	if actual := fmt.Sprintf("sha256:%x", hash.Sum(nil)); actual != expectedDigest {
		_ = os.Remove(tmp)
		return fmt.Errorf("GeoIP 下载校验失败: SHA-256 不匹配")
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("安装 GeoIP: %w", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(digestFile, []byte(expectedDigest+"\n"), 0o600); err != nil {
		return fmt.Errorf("保存 GeoIP 校验值: %w", err)
	}
	return os.Chmod(digestFile, 0o600)
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 100<<20)); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

// getWithProxyFallback 先直连（遵循 HTTPS_PROXY 等环境变量），失败后再经
// TIZI_HTTP_PROXY 指定的 HTTP 代理重试；那个代理可达时先用它。
func getWithProxyFallback(ctx context.Context, endpoint string, directTimeout time.Duration) (*http.Response, error) {
	type attempt struct {
		name    string
		proxy   func(*http.Request) (*url.URL, error)
		timeout time.Duration
	}
	direct := attempt{name: "直连", proxy: http.ProxyFromEnvironment, timeout: directTimeout}
	attempts := []attempt{direct}
	if proxyAddress := strings.TrimSpace(os.Getenv("TIZI_HTTP_PROXY")); proxyAddress != "" {
		proxyURL, err := url.Parse(proxyAddress)
		if err != nil || proxyURL.Host == "" {
			return nil, fmt.Errorf("TIZI_HTTP_PROXY 地址无效: %q", proxyAddress)
		}
		fallback := attempt{name: "代理 " + proxyURL.Host, proxy: http.ProxyURL(proxyURL), timeout: 30 * time.Second}
		attempts = append(attempts, fallback)
		if connection, dialErr := net.DialTimeout("tcp", proxyURL.Host, 500*time.Millisecond); dialErr == nil {
			_ = connection.Close()
			attempts = []attempt{fallback, direct}
		}
	}
	var failures []string
	for _, current := range attempts {
		response, requestErr := doGet(ctx, endpoint, current.proxy, current.timeout)
		if requestErr == nil && response.StatusCode >= 200 && response.StatusCode < 400 {
			return response, nil
		}
		if response != nil {
			failures = append(failures, fmt.Sprintf("%s返回 HTTP %s%s", current.name, response.Status, rateLimitDetail(response)))
			_ = response.Body.Close()
		} else {
			failures = append(failures, fmt.Sprintf("%s失败（%v）", current.name, requestErr))
		}
	}
	return nil, fmt.Errorf("%s", strings.Join(failures, "；"))
}

func doGet(ctx context.Context, endpoint string, proxy func(*http.Request) (*url.URL, error), timeout time.Duration) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "tizi")
	// 未认证的 api.github.com 每小时只有 60 次请求，共用出口 IP 时
	// 很容易触顶；用户设置 GITHUB_TOKEN 时带上认证以提升配额。下载地址
	// 会跳转到 CDN，不附加 token。
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" && request.URL.Host == "api.github.com" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = proxy
	// ResponseHeaderTimeout 只覆盖连接建立之后；被墙的直连会卡死在 TCP
	// 握手，必须单独限制拨号时间，否则回退到代理前要等系统级超时。
	transport.DialContext = (&net.Dialer{Timeout: timeout}).DialContext
	transport.ResponseHeaderTimeout = timeout
	client := &http.Client{Timeout: 5 * time.Minute, Transport: transport}
	return client.Do(request)
}

// rateLimitDetail 在 GitHub 返回 403/429 时补充配额信息，帮助区分
// 「被限流」和「真的没权限」。
func rateLimitDetail(response *http.Response) string {
	if response == nil || (response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusTooManyRequests) {
		return ""
	}
	remaining := strings.TrimSpace(response.Header.Get("X-RateLimit-Remaining"))
	reset := strings.TrimSpace(response.Header.Get("X-RateLimit-Reset"))
	if remaining == "" && reset == "" {
		return ""
	}
	detail := "（GitHub API 配额"
	if remaining != "" {
		detail += "剩余 " + remaining
	}
	if unix, err := strconv.ParseInt(reset, 10, 64); err == nil && unix > 0 {
		detail += "，" + time.Unix(unix, 0).Local().Format("15:04") + " 重置"
	}
	if remaining == "0" {
		detail += "；可设置 GITHUB_TOKEN 提升配额"
	}
	return detail + "）"
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index]
	}
	return value
}
