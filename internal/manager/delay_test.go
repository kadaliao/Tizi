package manager

import "testing"

func TestIsInfoNodeMatchesSubscriptionNotices(t *testing.T) {
	for _, name := range []string{"剩余流量：12.5 GB", "套餐到期：2026-12-01", "距离下次重置剩余：3 天", "官网 example.com", "最新网址：example.net", "Traffic: 10GB", "EXPIRE 2027"} {
		if !IsInfoNode(name) {
			t.Errorf("%q should be treated as a notice", name)
		}
	}
	for _, name := range []string{"香港 01", "🇯🇵 日本 IPLC 02", "US-LA 0.5x", "新加坡 流媒体"} {
		if IsInfoNode(name) {
			t.Errorf("%q is a real node", name)
		}
	}
}

func TestDelaySummary(t *testing.T) {
	cases := []struct {
		delays map[string]int
		want   string
	}{
		{nil, "没有可以测速的节点"},
		{map[string]int{"a": 0, "b": 0}, "2 个节点都没测通，请检查网络或订阅是否过期"},
		{map[string]int{"香港 01": 120, "日本 02": 85}, "2 个节点都能用，最快的是 日本 02（85ms）"},
		{map[string]int{"香港 01": 0, "日本 02": 85, "美国 03": 300}, "3 个节点里 2 个能用，最快的是 日本 02（85ms）"},
	}
	for _, tc := range cases {
		if got := DelaySummary(tc.delays); got != tc.want {
			t.Errorf("DelaySummary(%v) = %q, want %q", tc.delays, got, tc.want)
		}
	}
}

func TestNodeDelaysDropsNotices(t *testing.T) {
	delays := NodeDelays(map[string]int{"剩余流量：12 GB": 0, "香港 01": 85})
	if len(delays) != 1 || delays["香港 01"] != 85 {
		t.Fatalf("NodeDelays = %v", delays)
	}
}
