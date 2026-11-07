package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kadaliao/tizi/internal/config"
	"gopkg.in/yaml.v3"
)

func TestParseURL(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    map[string]any
		wantErr string
	}{
		{raw: "socks5://alice:p%40ss@203.0.113.9:1080", want: map[string]any{"type": "socks5", "server": "203.0.113.9", "port": 1080, "username": "alice", "password": "p@ss"}},
		{raw: "socks5h://gw.example.com:443", want: map[string]any{"type": "socks5", "server": "gw.example.com", "port": 443}},
		{raw: "http://u:p@[2001:db8::1]:8080", want: map[string]any{"type": "http", "server": "2001:db8::1", "port": 8080, "username": "u", "password": "p"}},
		{raw: "https://u:p@gw.example.com:443/", want: map[string]any{"type": "http", "tls": true, "server": "gw.example.com", "port": 443, "username": "u", "password": "p"}},
		{raw: "ss://abc@1.2.3.4:443", wantErr: "只支持"},
		{raw: "socks5://1.2.3.4", wantErr: "主机和端口"},
		{raw: "socks5://1.2.3.4:70000", wantErr: "主机和端口"},
		{raw: "http://1.2.3.4:80/path", wantErr: "路径"},
		{raw: "1.2.3.4:1080", wantErr: "无效"},
	} {
		got, err := ParseURL(tc.raw)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: error = %v, want %q", tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.raw, got, tc.want)
			continue
		}
		for key, value := range tc.want {
			if got[key] != value {
				t.Errorf("%s: %s = %#v, want %#v", tc.raw, key, got[key], value)
			}
		}
	}
}

func TestAddressRoundTripsThroughURL(t *testing.T) {
	for _, tc := range []struct {
		address Address
		want    string
	}{
		{Address{Scheme: "socks5", Host: "203.0.113.9", Port: 10003, Username: "alice", Password: "p@ss:w/rd#%"}, "socks5://alice:p%40ss%3Aw%2Frd%23%25@203.0.113.9:10003"},
		{Address{Scheme: "HTTP", Host: " gw.example.com ", Port: 443, Username: "user-session-1"}, "http://user-session-1@gw.example.com:443"},
		{Address{Scheme: "https", Host: "[2001:db8::1]", Port: 8443}, "https://[2001:db8::1]:8443"},
	} {
		got, err := tc.address.URL()
		if err != nil || got != tc.want {
			t.Errorf("%+v: URL = %q, %v; want %q", tc.address, got, err, tc.want)
			continue
		}
		back, err := SplitURL(got)
		want := tc.address
		want.Scheme = strings.ToLower(want.Scheme)
		want.Host = strings.Trim(strings.TrimSpace(want.Host), "[]")
		if err != nil || back != want {
			t.Errorf("SplitURL(%q) = %+v, %v; want %+v", got, back, err, want)
		}
		proxy, err := ParseURL(got)
		if err != nil || (tc.address.Password != "" && proxy["password"] != tc.address.Password) {
			t.Errorf("ParseURL(%q) = %#v, %v", got, proxy, err)
		}
	}
	if got, _ := SplitURL("socks5h://gw.example.com:443"); got.Scheme != "socks5" {
		t.Errorf("socks5h should split as socks5, got %q", got.Scheme)
	}
}

func TestAddressRejectsBadParts(t *testing.T) {
	for _, tc := range []struct {
		address Address
		wantErr string
	}{
		{Address{Scheme: "ss", Host: "1.2.3.4", Port: 443}, "只支持"},
		{Address{Scheme: "socks5", Host: " ", Port: 443}, "主机"},
		{Address{Scheme: "socks5", Host: "socks5://1.2.3.4", Port: 443}, "不要带协议"},
		{Address{Scheme: "socks5", Host: "1.2.3.4:443", Port: 443}, "不要带协议"},
		{Address{Scheme: "socks5", Host: "u@1.2.3.4", Port: 443}, "不要带协议"},
		{Address{Scheme: "socks5", Host: "1.2.3.4", Port: 0}, "端口"},
		{Address{Scheme: "socks5", Host: "1.2.3.4", Port: 70000}, "端口"},
		{Address{Scheme: "socks5", Host: "1.2.3.4", Port: 443, Password: "secret"}, "用户名"},
	} {
		if _, err := tc.address.URL(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%+v: error = %v, want %q", tc.address, err, tc.wantErr)
		}
	}
}

func TestMaskURLHidesPassword(t *testing.T) {
	got := MaskURL("socks5://alice:secret@203.0.113.9:1080")
	if strings.Contains(got, "secret") || got != "socks5://alice:***@203.0.113.9:1080" {
		t.Fatalf("MaskURL = %q", got)
	}
	if MaskURL("") != "" {
		t.Fatal("empty address must stay empty")
	}
}

func TestLoadChainsThroughFrontProxyByDefault(t *testing.T) {
	settings := config.Settings{ClaudeProxy: "socks5://u:p@203.0.113.9:1080", Subscription: "https://sub.example.com/x"}
	node, err := Load(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if node.Direct || node.Proxy["name"] != config.ResidentialProxyName || node.Proxy["dialer-proxy"] != config.ChainGroup {
		t.Fatalf("unexpected node: %#v", node.Proxy)
	}
	if node.Hint() != "" {
		t.Fatalf("chained node needs no hint, got %q", node.Hint())
	}

	// 没有可以做前置代理的节点时只能直接连接。
	node, err = Load(t.TempDir(), config.Settings{ClaudeProxy: settings.ClaudeProxy})
	if err != nil {
		t.Fatal(err)
	}
	if !node.Direct || node.Proxy["dialer-proxy"] != nil {
		t.Fatalf("node without a front proxy must be dialed directly: %#v", node.Proxy)
	}

	settings.ClaudeProxyDirect = true
	node, err = Load(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := node.Proxy["dialer-proxy"]; ok {
		t.Fatalf("direct node must not have a dialer-proxy: %#v", node.Proxy)
	}
	if !strings.Contains(node.Hint(), "明文") {
		t.Fatalf("direct plaintext node must warn, got %q", node.Hint())
	}
}

func TestLoadNodeFile(t *testing.T) {
	for name, content := range map[string]string{
		"single":  "type: trojan\nserver: example.com\nport: 443\npassword: x\nname: mine\ndialer-proxy: other\n",
		"list":    "- {name: mine, type: trojan, server: example.com, port: \"443\", password: x}\n",
		"proxies": "proxies:\n  - {name: mine, type: trojan, server: example.com, port: 443, password: x}\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, config.ClaudeProxyFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			node, err := Load(dir, config.Settings{Subscription: "https://sub.example.com/x"})
			if err != nil {
				t.Fatal(err)
			}
			if !node.FromFile || node.Type() != "trojan" || node.Port() != 443 || node.Server() != "example.com" {
				t.Fatalf("unexpected node: %#v", node)
			}
			if node.Proxy["name"] != config.ResidentialProxyName || node.Proxy["dialer-proxy"] != config.ChainGroup {
				t.Fatalf("name and dialer-proxy must follow the settings: %#v", node.Proxy)
			}
		})
	}
}

func TestLoadRejectsAmbiguousOrUnusableNodes(t *testing.T) {
	write := func(t *testing.T, content string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, config.ClaudeProxyFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, tc := range []struct {
		name     string
		content  string
		settings config.Settings
		wantErr  string
	}{
		{name: "both set", content: "type: ss\nserver: a\nport: 443\n", settings: config.Settings{ClaudeProxy: "socks5://a:443"}, wantErr: "只保留一个"},
		{name: "two nodes", content: "- {type: ss, server: a, port: 443}\n- {type: ss, server: b, port: 443}\n", wantErr: "只能包含一个"},
		{name: "missing server", content: "type: ss\nport: 443\n", wantErr: "缺少"},
		{name: "direct type", content: "type: direct\nserver: a\nport: 1\n", wantErr: "不能作为"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.content), tc.settings)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}

	// UDP 协议的节点也可以使用。
	if _, err := Load(write(t, "type: hysteria2\nserver: a\nport: 443\n"), config.Settings{}); err != nil {
		t.Fatalf("hysteria2 node should load: %v", err)
	}
}

func TestChainableCountsNodeFile(t *testing.T) {
	dir := t.TempDir()
	if Chainable(dir, config.Settings{}) {
		t.Fatal("nothing configured must not be chainable")
	}
	if err := os.WriteFile(filepath.Join(dir, config.ProxiesFile), []byte("trojan://p@example.com:443#a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !Chainable(dir, config.Settings{}) {
		t.Fatal("a node file must make the residential node chainable")
	}
	if !Chainable(t.TempDir(), config.Settings{TailscaleEnabled: true, TailscaleExitNode: "home"}) {
		t.Fatal("a tailscale exit node must make the residential node chainable")
	}
	if Chainable(t.TempDir(), config.Settings{IntranetHTTPProxy: "10.1.2.3:8080"}) {
		t.Fatal("an intranet HTTP egress without an ovpn must not count")
	}
}

func TestTemplateIsNotANode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.ClaudeProxyFile), []byte(FileTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	node, err := Load(dir, config.Settings{})
	if err != nil || node != nil {
		t.Fatalf("template must not configure a node: %#v, %v", node, err)
	}
	if Configured(dir, config.Settings{}) {
		t.Fatal("template must not count as configured")
	}
}

func TestLoadDialsLocalNodesDirectly(t *testing.T) {
	// SSH 端口转发出来的节点：经前置代理链式连接时，前置代理连的是它自己的
	// 127.0.0.1，只会被拒绝，所以不管设置怎么选都直接连接。
	settings := config.Settings{ClaudeProxy: "socks5://u:p@127.0.0.1:10003", Subscription: "https://sub.example.com/x"}
	node, err := Load(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if !node.Direct {
		t.Fatal("local node must be dialed directly")
	}
	if _, ok := node.Proxy["dialer-proxy"]; ok {
		t.Fatalf("local node must not chain through the front proxy: %#v", node.Proxy)
	}
	if hint := node.Hint(); hint != "" {
		t.Fatalf("local plaintext node stays on this machine and needs no hint, got %q", hint)
	}

	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.1.2.3": true, "::1": true, "[::1]": true,
		"localhost": true, "LOCALHOST.": true, "tunnel.localhost": true,
		"10.0.0.1": false, "192.168.1.1": false, "203.0.113.9": false,
		"isp.decodo.com": false, "127.example.com": false, "": false,
	} {
		if got := IsLocal(host); got != want {
			t.Errorf("IsLocal(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestRunningNodeComparesWithSettings(t *testing.T) {
	settings := config.Settings{ClaudeProxy: "socks5://u:p@203.0.113.9:10003", Subscription: "https://sub.example.com/x"}
	chained, err := Load(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.ClaudeProxyDirect = true
	direct, err := Load(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if Running(nil) != nil {
		t.Fatal("no running node should give nil")
	}
	// 运行中的定义来自 config.yaml，经过一次 YAML 往返，类型要能对上。
	data, err := yaml.Marshal(chained.Proxy)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	running := Running(parsed)
	if running.Direct || !running.Same(*chained) || running.Same(*direct) {
		t.Fatalf("running = %+v", running)
	}
	if got := running.Describe(); got != "经前置代理链式连接 203.0.113.9:10003" {
		t.Fatalf("Describe = %q", got)
	}
	if got := direct.Describe(); got != "本机直接连接 203.0.113.9:10003" {
		t.Fatalf("Describe = %q", got)
	}
}
