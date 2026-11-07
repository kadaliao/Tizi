package ovpn

import (
	"os"
	"path/filepath"
)

// Scan 在下载、桌面和当前 OVPN 所在目录中查找 .ovpn 文件，当前文件排在最前，
// 结果为去重后的绝对路径。TUI 与 GUI 的「扫描」共用这一份规则。
func Scan(current string) []string {
	home, _ := os.UserHomeDir()
	dirs := []string{filepath.Join(home, "Downloads"), filepath.Join(home, "Desktop"), filepath.Dir(current)}
	seen := map[string]bool{}
	var matches []string
	if current != "" {
		if abs, err := filepath.Abs(current); err == nil {
			seen[abs] = true
			matches = append(matches, abs)
		}
	}
	for _, dir := range dirs {
		if dir == "." || dir == "" {
			continue
		}
		found, _ := filepath.Glob(filepath.Join(dir, "*.ovpn"))
		for _, path := range found {
			abs, err := filepath.Abs(path)
			if err == nil && !seen[abs] {
				seen[abs] = true
				matches = append(matches, abs)
			}
		}
	}
	return matches
}
