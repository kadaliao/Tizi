// Package nodes 读取配置目录下的节点文件 proxies.yaml。
//
// 支持两种写法：
//
//   - Clash/Mihomo 节点 YAML：带 proxies: 的片段、节点列表或单个节点。
//     节点直接写进生成的配置，可以在策略组里逐个选择。
//   - 分享链接：每行一个 ss://、vmess://、trojan://、vless://、hysteria2://
//     等链接（也可以整体 Base64 编码）。交给 Mihomo 的文件 proxy-provider
//     解析。
package nodes

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// File 是解析后的节点文件。
type File struct {
	// Proxies 是 YAML 写法里的节点，已检查 name、type。
	Proxies []map[string]any
	// Links 是分享链接写法里的链接数；大于 0 时 Proxies 为空。
	Links int
}

// Count 返回节点数。
func (f File) Count() int {
	return len(f.Proxies) + f.Links
}

// Names 返回 YAML 节点的名字，顺序与文件一致。
func (f File) Names() []string {
	names := make([]string, 0, len(f.Proxies))
	for _, proxy := range f.Proxies {
		name, _ := proxy["name"].(string)
		names = append(names, name)
	}
	return names
}

// Load 读取节点文件。文件不存在、为空或只有注释时返回空的 File。
// reserved 是生成配置里已经占用的名字，节点不能重名。
func Load(path string, reserved map[string]bool) (File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("读取节点文件 %s: %w", path, err)
	}
	text := stripComments(string(data))
	if strings.TrimSpace(text) == "" {
		return File{}, nil
	}
	if links := countLinks(text); links > 0 {
		return File{Links: links}, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), "")); err == nil {
		if links := countLinks(string(decoded)); links > 0 {
			return File{Links: links}, nil
		}
	}
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return File{}, fmt.Errorf("解析节点文件 %s: %w", path, err)
	}
	list, err := proxyList(value)
	if err != nil {
		return File{}, fmt.Errorf("节点文件 %s: %w", path, err)
	}
	seen := map[string]bool{}
	file := File{Proxies: make([]map[string]any, 0, len(list))}
	for index, item := range list {
		proxy, ok := item.(map[string]any)
		if !ok {
			return File{}, fmt.Errorf("节点文件 %s 第 %d 个节点不是 Mihomo 节点格式", path, index+1)
		}
		kind, _ := proxy["type"].(string)
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "" {
			return File{}, fmt.Errorf("节点文件 %s 第 %d 个节点缺少 type", path, index+1)
		}
		proxy["type"] = kind
		name := strings.TrimSpace(nameOf(proxy["name"]))
		if name == "" {
			name = "节点 " + strconv.Itoa(index+1)
		}
		if reserved[name] || reserved[strings.ToUpper(name)] {
			return File{}, fmt.Errorf("节点文件 %s 中的节点名 %q 与内置名称冲突，请换一个名字", path, name)
		}
		if seen[name] {
			return File{}, fmt.Errorf("节点文件 %s 中有两个节点都叫 %q", path, name)
		}
		seen[name] = true
		proxy["name"] = name
		file.Proxies = append(file.Proxies, proxy)
	}
	return file, nil
}

func proxyList(value any) ([]any, error) {
	switch typed := value.(type) {
	case map[string]any:
		if list, ok := typed["proxies"]; ok && typed["type"] == nil {
			if list == nil {
				return nil, nil
			}
			items, ok := list.([]any)
			if !ok {
				return nil, errors.New("proxies: 应是节点列表")
			}
			return items, nil
		}
		if typed["type"] != nil {
			return []any{typed}, nil
		}
	case []any:
		return typed, nil
	}
	return nil, errors.New("应是 Mihomo 节点列表（proxies:）或每行一个分享链接")
}

func nameOf(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

// countLinks 统计形如 scheme://... 的行；有任何一行不是链接时返回 0。
func countLinks(text string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		scheme, rest, ok := strings.Cut(line, "://")
		if !ok || rest == "" || scheme == "" || strings.ContainsAny(scheme, " :{}[]\"'") {
			return 0
		}
		count++
	}
	return count
}

func stripComments(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// FileTemplate 是首次从界面打开 proxies.yaml 时写入的说明，全部是注释，
// 不会被当成节点。
const FileTemplate = `# Tizi 节点文件
#
# 有机场订阅时，直接在「配置」页填订阅地址即可，不需要这个文件。
# 这个文件用来添加自己的节点，两种写法任选一种（不要混用）：
#
# 1. 每行一个分享链接（ss://、vmess://、trojan://、vless://、hysteria2:// 等）：
#
# ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ@203.0.113.10:8388#my-ss
# trojan://password@example.com:443?sni=example.com#my-trojan
#
# 2. Mihomo 节点格式（可以逐个出现在「外网代理」和「Claude」的选项里）：
#
# proxies:
#   - name: my-trojan
#     type: trojan
#     server: example.com
#     port: 443
#     password: your-password
#     sni: example.com
#   - name: my-socks
#     type: socks5
#     server: 127.0.0.1
#     port: 1080
#
# 保存后重载 Mihomo 生效。
`
