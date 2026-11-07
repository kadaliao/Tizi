package manager

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
)

// DelayTimeout 是测速时每个节点的超时，与 Mihomo 定时测速的默认值一致。
const DelayTimeout = 5 * time.Second

var infoNode = regexp.MustCompile(config.InfoNodePattern)

// IsInfoNode 报告 name 是不是订阅里的说明条目（剩余流量、到期时间等）。
func IsInfoNode(name string) bool {
	return infoNode.MatchString(name)
}

// NodeDelays 去掉说明条目的测速结果：Mihomo 会照样测它们，界面上给「剩余
// 流量」标「超时」只会让人误会。
func NodeDelays(delays map[string]int) map[string]int {
	for name := range delays {
		if IsInfoNode(name) {
			delete(delays, name)
		}
	}
	return delays
}

// TestDelays 测一遍可以上外网的节点，返回节点名到延迟（毫秒）的对应，0 表示
// 没测通。测完后「自动选择」按这次的结果重新挑节点。内网和 Tailscale 不上
// 外网，不测。
func TestDelays(client controller.Client) (map[string]int, error) {
	return client.TestDelays(config.HealthCheckURL, DelayTimeout,
		[]string{config.IntranetGroup, config.TailscaleGroup}, IsInfoNode)
}

// DelaySummary 用一句话总结测速结果，例如「40 个节点里 32 个能用，最快的是
// 香港 02（85ms）」。
func DelaySummary(delays map[string]int) string {
	if len(delays) == 0 {
		return "没有可以测速的节点"
	}
	names := make([]string, 0, len(delays))
	for name, delay := range delays {
		if delay > 0 {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return fmt.Sprintf("%d 个节点都没测通，请检查网络或订阅是否过期", len(delays))
	}
	sort.Slice(names, func(i, j int) bool {
		if delays[names[i]] != delays[names[j]] {
			return delays[names[i]] < delays[names[j]]
		}
		return names[i] < names[j]
	})
	fastest := names[0]
	if len(names) == len(delays) {
		return fmt.Sprintf("%d 个节点都能用，最快的是 %s（%dms）", len(delays), fastest, delays[fastest])
	}
	return fmt.Sprintf("%d 个节点里 %d 个能用，最快的是 %s（%dms）", len(delays), len(names), fastest, delays[fastest])
}
