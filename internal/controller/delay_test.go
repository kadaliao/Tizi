package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testProbeURL = "https://www.gstatic.com/generate_204"

// proxiesPayload 模仿 Mihomo 的 /proxies：订阅里两个节点和一条说明条目，
// 外网代理（Selector）、自动选择（URLTest）、内网（Selector）和住宅节点。
const proxiesPayload = `{"proxies": {
  "GLOBAL": {"type": "Selector", "now": "DIRECT", "all": ["DIRECT", "外网代理"]},
  "外网代理": {"type": "Selector", "now": "自动选择", "all": ["自动选择", "住宅节点", "剩余流量：12 GB", "香港 01", "日本 02", "DIRECT"]},
  "自动选择": {"type": "URLTest", "now": "日本 02", "all": ["香港 01", "日本 02"]},
  "内网": {"type": "Selector", "now": "openvpn-1", "all": ["openvpn-1"]},
  "香港 01": {"type": "Trojan", "extra": {"https://www.gstatic.com/generate_204": {"alive": false, "history": [{"delay": 0}]}}},
  "日本 02": {"type": "Shadowsocks", "extra": {
    "https://www.gstatic.com/generate_204": {"alive": true, "history": [{"delay": 300}, {"delay": 85}]},
    "http://10.0.0.1/": {"alive": true, "history": [{"delay": 7}]}}},
  "剩余流量：12 GB": {"type": "Shadowsocks", "extra": {}},
  "住宅节点": {"type": "Socks5"},
  "openvpn-1": {"type": "OpenVPN", "extra": {"http://10.0.0.1/": {"alive": true, "history": [{"delay": 7}]}}},
  "DIRECT": {"type": "Direct"}
}}`

func TestOverviewSplitsGroupsAndReadsDelaysForTestURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(proxiesPayload))
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, HTTP: server.Client()}

	overview, err := client.Overview(testProbeURL)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, group := range overview.Groups {
		names = append(names, group.Name)
	}
	if want := []string{"内网", "外网代理"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("selectable groups = %v, want %v", names, want)
	}
	if len(overview.Auto) != 1 || overview.Auto[0].Name != "自动选择" || overview.Auto[0].Now != "日本 02" {
		t.Fatalf("auto groups = %#v", overview.Auto)
	}
	// 只取测速地址的最后一次结果；没测通记 0；其他地址的结果（内网探测）不算。
	if want := map[string]int{"香港 01": 0, "日本 02": 85}; !reflect.DeepEqual(overview.Delays, want) {
		t.Fatalf("delays = %v, want %v", overview.Delays, want)
	}
	route := ResolveRoute(overview.AllGroups(), "外网代理")
	if route != "外网代理 -> 自动选择 -> 日本 02" {
		t.Fatalf("route = %q", route)
	}

	groups, err := client.Groups()
	if err != nil || len(groups) != 2 {
		t.Fatalf("Groups() = %v, %v; want only the selectable groups", groups, err)
	}
}

func TestGroupDelayTreatsAllTimeoutAsNoResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusGatewayTimeout)
		_, _ = writer.Write([]byte(`{"message": "get delay: all proxies timeout"}`))
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, HTTP: server.Client()}

	delays, err := client.GroupDelay("自动选择", testProbeURL, time.Second)
	if err != nil || len(delays) != 0 {
		t.Fatalf("GroupDelay = %v, %v; want empty result without error", delays, err)
	}
}

func TestDelayRequestOutlivesProbeTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = writer.Write([]byte(`{"delay": 120}`))
	}))
	defer server.Close()
	// Client.HTTP 的超时比 Mihomo 的测速时间短，测速不能用它。
	httpClient := server.Client()
	httpClient.Timeout = 100 * time.Millisecond
	client := Client{BaseURL: server.URL, HTTP: httpClient}

	delay, err := client.ProxyDelay("日本 02", testProbeURL, time.Second)
	if err != nil || delay != 120 {
		t.Fatalf("ProxyDelay = %d, %v", delay, err)
	}
}

func TestTestDelaysCoversAutoGroupAndStandaloneNodes(t *testing.T) {
	var mu sync.Mutex
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/proxies" {
			_, _ = writer.Write([]byte(proxiesPayload))
			return
		}
		path, _ := url.PathUnescape(request.URL.EscapedPath())
		mu.Lock()
		requested = append(requested, path)
		mu.Unlock()
		if request.Method != http.MethodDelete && request.URL.Query().Get("url") != testProbeURL {
			t.Errorf("probe url = %q", request.URL.Query().Get("url"))
		}
		if request.Method == http.MethodDelete {
			if path != "/proxies/自动选择" {
				t.Errorf("unexpected unfix %s", path)
			}
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		switch path {
		case "/group/自动选择/delay":
			_, _ = writer.Write([]byte(`{"日本 02": 90}`))
		case "/proxies/住宅节点/delay":
			_, _ = writer.Write([]byte(`{"delay": 230}`))
		default:
			t.Errorf("unexpected request %s", path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, HTTP: server.Client()}

	isInfo := func(name string) bool { return strings.HasPrefix(name, "剩余流量") }
	delays, err := client.TestDelays(testProbeURL, time.Second, []string{"内网"}, isInfo)
	if err != nil {
		t.Fatal(err)
	}
	// 自动选择里没测通的节点记 0；DIRECT、说明条目和内网节点不测。
	if want := map[string]int{"香港 01": 0, "日本 02": 90, "住宅节点": 230}; !reflect.DeepEqual(delays, want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	// 两次测速，加上测完后让自动选择丢掉测速期间缓存的旧结果。
	if want := []string{"/group/自动选择/delay", "/proxies/住宅节点/delay", "/proxies/自动选择"}; !sameElements(requested, want) {
		t.Fatalf("requests = %v, want %v", requested, want)
	}
}

func sameElements(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, item := range got {
		seen[item]++
	}
	for _, item := range want {
		if seen[item] == 0 {
			return false
		}
		seen[item]--
	}
	return true
}
