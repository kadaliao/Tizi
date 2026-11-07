package nodes

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxies.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingOrTemplateIsEmpty(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "missing.yaml"), write(t, FileTemplate), write(t, "\n# only comments\n")} {
		file, err := Load(path, nil)
		if err != nil || file.Count() != 0 {
			t.Fatalf("%s: file=%+v err=%v", path, file, err)
		}
	}
}

func TestLoadYAMLShapes(t *testing.T) {
	for name, content := range map[string]string{
		"proxies": "proxies:\n  - {name: a, type: ss, server: 203.0.113.1, port: 8388}\n  - {name: b, type: Trojan, server: b.example.com, port: 443}\n",
		"list":    "- {name: a, type: ss, server: 203.0.113.1, port: 8388}\n- {name: b, type: trojan, server: b.example.com, port: 443}\n",
	} {
		t.Run(name, func(t *testing.T) {
			file, err := Load(write(t, content), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(file.Names(), []string{"a", "b"}) || file.Proxies[1]["type"] != "trojan" {
				t.Fatalf("file = %+v", file)
			}
		})
	}
	file, err := Load(write(t, "type: socks5\nserver: 127.0.0.1\nport: 1080\n"), nil)
	if err != nil || !reflect.DeepEqual(file.Names(), []string{"节点 1"}) {
		t.Fatalf("single unnamed node: %+v, %v", file, err)
	}
}

func TestLoadShareLinks(t *testing.T) {
	links := "trojan://p@a.example.com:443#a\nss://YWVzLTI1Ni1nY206cA@203.0.113.5:8388#b\n"
	for name, content := range map[string]string{
		"plain":  "# mine\n" + links,
		"base64": base64.StdEncoding.EncodeToString([]byte(links)),
	} {
		t.Run(name, func(t *testing.T) {
			file, err := Load(write(t, content), nil)
			if err != nil || file.Links != 2 || len(file.Proxies) != 0 {
				t.Fatalf("file=%+v err=%v", file, err)
			}
		})
	}
}

func TestLoadRejectsBadNodes(t *testing.T) {
	for name, tc := range map[string]struct {
		content  string
		reserved map[string]bool
		want     string
	}{
		"duplicate": {content: "- {name: a, type: ss}\n- {name: a, type: ss}\n", want: "两个节点"},
		"reserved":  {content: "- {name: 外网代理, type: ss}\n", reserved: map[string]bool{"外网代理": true}, want: "冲突"},
		"no type":   {content: "- {name: a, server: x}\n", want: "缺少 type"},
		"garbage":   {content: "just: [a, b]\n", want: "应是"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.content), tc.reserved)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
