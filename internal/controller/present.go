package controller

import (
	"strings"
	"time"
)

// Rate 是一条连接在两次采样之间的平均上下行速度（字节/秒）。
type Rate struct {
	Download float64
	Upload   float64
}

// Destination 返回连接的展示目标：优先域名，其次嗅探域名和目标 IP，
// 并附上端口；IPv6 地址带方括号，避免与端口混淆。
func (c Connection) Destination() string {
	host := c.Metadata.Host
	if host == "" {
		host = c.Metadata.SniffHost
	}
	if host == "" {
		host = c.Metadata.DestinationIP
	}
	if host == "" {
		host = "未知目标"
	}
	if c.Metadata.DestinationPort == "" {
		return host
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + c.Metadata.DestinationPort
}

// Route 把 Mihomo 从出口到入口排列的 chains 反转成「策略组 -> 实际节点」的阅读顺序。
func Route(chains []string) string {
	ordered := make([]string, len(chains))
	for i, chain := range chains {
		ordered[len(chains)-1-i] = chain
	}
	return strings.Join(ordered, " -> ")
}

// ResolveRoute 沿策略组当前选择展开 target，直到落到非策略组节点或出现环。
func ResolveRoute(groups []Group, target string) string {
	seen := map[string]bool{}
	parts := []string{target}
	for {
		if seen[target] {
			break
		}
		seen[target] = true
		found := false
		for _, group := range groups {
			if group.Name == target && group.Now != "" && group.Now != target {
				target = group.Now
				parts = append(parts, target)
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	return strings.Join(parts, " -> ")
}

// Rates 按连接 ID 对比两次采样的累计流量，返回间隔内的平均速度。新出现的
// 连接或计数回退（Mihomo 重启后 ID 复用）的连接没有可信速度，不出现在结果中。
func Rates(previous, current []Connection, elapsed time.Duration) map[string]Rate {
	seconds := elapsed.Seconds()
	if seconds <= 0 {
		return nil
	}
	before := make(map[string]Connection, len(previous))
	for _, connection := range previous {
		before[connection.ID] = connection
	}
	rates := make(map[string]Rate, len(current))
	for _, connection := range current {
		old, ok := before[connection.ID]
		if !ok || connection.Download < old.Download || connection.Upload < old.Upload {
			continue
		}
		rates[connection.ID] = Rate{
			Download: float64(connection.Download-old.Download) / seconds,
			Upload:   float64(connection.Upload-old.Upload) / seconds,
		}
	}
	return rates
}
