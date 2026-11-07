package tracescope

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// vendorVersion 返回内嵌看板快照的上游提交版本号（web/TRACESCOPE_VERSION）。
func vendorVersion() (string, error) {
	data, err := fs.ReadFile(webFS, "web/TRACESCOPE_VERSION")
	if err != nil {
		return "", fmt.Errorf("读取内嵌看板版本: %w", err)
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("内嵌看板版本为空")
	}
	return v, nil
}

// extractedMarker 记录已解包源码的内容哈希；buildMarker 记录 dist 对应的源码哈希。
const (
	extractedMarker = ".extracted"
	buildMarker     = "dist/.built"
)

// extractedDigest 读取运行目录中已解包源码的内容哈希。
func extractedDigest(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, extractedMarker))
	if err != nil {
		return "", fmt.Errorf("读取解包标记: %w", err)
	}
	digest := strings.TrimSpace(string(data))
	if digest == "" {
		return "", fmt.Errorf("解包标记为空")
	}
	return digest, nil
}

// userCacheDir 可在测试中替换。
var userCacheDir = os.UserCacheDir

// runtimeRoot 返回看板运行目录的父目录（用户缓存目录下）。
func runtimeRoot() (string, error) {
	cache, err := userCacheDir()
	if err != nil {
		return "", fmt.Errorf("获取缓存目录: %w", err)
	}
	root := filepath.Join(cache, "tizi", "tracescope")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("创建看板缓存目录: %w", err)
	}
	return root, nil
}

// webDigest 对内嵌看板源码（webFS 下所有文件）计算内容哈希。
// 本地补丁只要改动任一文件，哈希即变化，从而让缓存失效并重新解包+构建，
// 避免沿用上一版本二进制留下的陈旧产物。
func webDigest() (string, error) {
	var files []string
	if err := fs.WalkDir(webFS, "web", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, path := range files {
		data, readErr := fs.ReadFile(webFS, path)
		if readErr != nil {
			return "", readErr
		}
		_, _ = hash.Write([]byte(path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ensureWeb 将内嵌看板源码解包到缓存目录（tracescope-<版本>）并返回该目录。
// 缓存有效性以内嵌 web 内容哈希（.extracted 标记）为准：版本号变化、内容
// 哈希变化（含本地补丁改动）或标记损坏时整体重建，并清理其他旧版本目录。
func ensureWeb(out io.Writer) (string, error) {
	version, err := vendorVersion()
	if err != nil {
		return "", err
	}
	digest, err := webDigest()
	if err != nil {
		return "", fmt.Errorf("计算看板内容哈希: %w", err)
	}
	root, err := runtimeRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "tracescope-"+version)
	if marker, err := os.ReadFile(filepath.Join(dir, extractedMarker)); err == nil && string(marker) == digest {
		return dir, nil
	}
	fmt.Fprintf(out, "首次运行需要准备 TraceScope 看板（版本 %s）...\n", version)

	// 清理其他版本目录与旧残留，只保留当前版本。
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "tracescope-") || entry.Name() == filepath.Base(dir) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
	_ = os.RemoveAll(dir)
	_ = os.RemoveAll(dir + ".tmp")
	if err := os.MkdirAll(dir+".tmp", 0o755); err != nil {
		return "", fmt.Errorf("创建看板解包目录: %w", err)
	}

	if err := fs.WalkDir(webFS, "web", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "web" {
			return nil
		}
		target := filepath.Join(dir+".tmp", strings.TrimPrefix(path, "web/"))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, readErr := fs.ReadFile(webFS, path)
		if readErr != nil {
			return readErr
		}
		// embed.FS 的文件权限固定为 0444，落盘时统一改为可写，
		// 否则 npm 无法更新 package-lock.json 等文件。
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		return "", fmt.Errorf("解包看板文件: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir+".tmp", extractedMarker), []byte(digest), 0o644); err != nil {
		return "", fmt.Errorf("写入解包标记: %w", err)
	}
	if err := os.Rename(dir+".tmp", dir); err != nil {
		return "", fmt.Errorf("落盘看板文件: %w", err)
	}
	return dir, nil
}
