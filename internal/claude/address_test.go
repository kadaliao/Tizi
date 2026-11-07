package claude

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAnyRecognizesProviderFormats(t *testing.T) {
	for _, tc := range []struct {
		text string
		want Address
	}{
		// 标准 URL，用户名和密码在前，转义的字符会解码。
		{"socks5://alice:p%40ss@203.0.113.9:10003", Address{Scheme: "socks5", Host: "203.0.113.9", Port: 10003, Username: "alice", Password: "p@ss"}},
		{"HTTP://alice:secret@gw.example.com:443/", Address{Scheme: "http", Host: "gw.example.com", Port: 443, Username: "alice", Password: "secret"}},
		{"socks5h://gw.example.com:1080", Address{Scheme: "socks5", Host: "gw.example.com", Port: 1080}},
		// 不写协议。
		{"alice:secret@gw.example.com:10003", Address{Host: "gw.example.com", Port: 10003, Username: "alice", Password: "secret"}},
		{"alice:p@ss@gw.example.com:10003", Address{Host: "gw.example.com", Port: 10003, Username: "alice", Password: "p@ss"}},
		// 用户名和密码在后。
		{"gw.example.com:10003@alice:secret", Address{Host: "gw.example.com", Port: 10003, Username: "alice", Password: "secret"}},
		{"socks5://gw.example.com:10003@alice:p@ss", Address{Scheme: "socks5", Host: "gw.example.com", Port: 10003, Username: "alice", Password: "p@ss"}},
		// 全用冒号分隔：主机在前或在后，密码里可以有冒号。
		{"203.0.113.9:10003:alice:secret", Address{Host: "203.0.113.9", Port: 10003, Username: "alice", Password: "secret"}},
		{"gw.example.com:10003:alice:se:cret", Address{Host: "gw.example.com", Port: 10003, Username: "alice", Password: "se:cret"}},
		{"alice:secret:gw.example.com:10003", Address{Host: "gw.example.com", Port: 10003, Username: "alice", Password: "secret"}},
		{"alice:se:cret:203.0.113.9:10003", Address{Host: "203.0.113.9", Port: 10003, Username: "alice", Password: "se:cret"}},
		{"https://gw.example.com:443:alice:secret", Address{Scheme: "https", Host: "gw.example.com", Port: 443, Username: "alice", Password: "secret"}},
		// 纯数字的密码不会被当成端口。
		{"203.0.113.9:10003:alice:12345", Address{Host: "203.0.113.9", Port: 10003, Username: "alice", Password: "12345"}},
		{"alice:12345:203.0.113.9:10003", Address{Host: "203.0.113.9", Port: 10003, Username: "alice", Password: "12345"}},
		// 两种拆法都说得通时，选主机是 IP 的那种。
		{"user.example:8080:203.0.113.9:10003", Address{Host: "203.0.113.9", Port: 10003, Username: "user.example", Password: "8080"}},
		// 只有主机和端口，或只有主机。
		{" gw.example.com:443 ", Address{Host: "gw.example.com", Port: 443}},
		{"gw.example.com:443:alice", Address{Host: "gw.example.com", Port: 443, Username: "alice"}},
		{"gw.example.com", Address{Host: "gw.example.com"}},
		{"2001:db8::1", Address{Host: "2001:db8::1"}},
		{"[2001:db8::1]:8080:alice:secret", Address{Host: "2001:db8::1", Port: 8080, Username: "alice", Password: "secret"}},
		{"alice:secret@[2001:db8::1]:8080", Address{Host: "2001:db8::1", Port: 8080, Username: "alice", Password: "secret"}},
	} {
		got, err := ParseAny(tc.text)
		if err != nil || got != tc.want {
			t.Errorf("ParseAny(%q) = %+v, %v; want %+v", tc.text, got, err, tc.want)
		}
	}
}

func TestParseAnyRejectsUnknownFormats(t *testing.T) {
	for _, text := range []string{"", "alice:secret", "alice", "gw.example.com:http", "gw.example.com:70000:alice:secret", "alice:secret@bob:pass"} {
		if got, err := ParseAny(text); !errors.Is(err, ErrUnknownFormat) {
			t.Errorf("ParseAny(%q) = %+v, %v; want ErrUnknownFormat", text, got, err)
		}
	}
	if _, err := ParseAny("ss://abc@1.2.3.4:443"); err == nil || !strings.Contains(err.Error(), "只支持") {
		t.Errorf("unsupported scheme error = %v", err)
	}
}
