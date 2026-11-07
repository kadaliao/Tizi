package controller

import (
	"testing"
	"time"
)

func TestConnectionDestination(t *testing.T) {
	tests := []struct {
		name                          string
		host, sniff, ip, port, expect string
	}{
		{name: "域名优先", host: "example.com", sniff: "sniff.example.com", ip: "203.0.113.1", port: "443", expect: "example.com:443"},
		{name: "嗅探域名", sniff: "sniff.example.com", ip: "203.0.113.1", port: "443", expect: "sniff.example.com:443"},
		{name: "IPv6 加方括号", ip: "2001:db8::1", port: "443", expect: "[2001:db8::1]:443"},
		{name: "无端口", ip: "203.0.113.1", expect: "203.0.113.1"},
		{name: "未知目标", expect: "未知目标"},
	}
	for _, test := range tests {
		var connection Connection
		connection.Metadata.Host = test.host
		connection.Metadata.SniffHost = test.sniff
		connection.Metadata.DestinationIP = test.ip
		connection.Metadata.DestinationPort = test.port
		if got := connection.Destination(); got != test.expect {
			t.Errorf("%s: Destination() = %q，期望 %q", test.name, got, test.expect)
		}
	}
}

func TestRouteAndResolveRoute(t *testing.T) {
	if got := Route([]string{"openvpn-1", "内网"}); got != "内网 -> openvpn-1" {
		t.Fatalf("Route = %q", got)
	}
	groups := []Group{
		{Name: "默认出口", Now: "外网代理"},
		{Name: "外网代理", Now: "hk-node"},
		{Name: "环A", Now: "环B"},
		{Name: "环B", Now: "环A"},
	}
	if got := ResolveRoute(groups, "默认出口"); got != "默认出口 -> 外网代理 -> hk-node" {
		t.Fatalf("ResolveRoute = %q", got)
	}
	if got := ResolveRoute(groups, "环A"); got != "环A -> 环B -> 环A" {
		t.Fatalf("策略组成环时应停止展开: %q", got)
	}
}

func TestRatesSkipNewAndResetConnections(t *testing.T) {
	previous := []Connection{{ID: "a", Download: 100, Upload: 10}, {ID: "reset", Download: 500}}
	current := []Connection{{ID: "a", Download: 300, Upload: 30}, {ID: "reset", Download: 5}, {ID: "new", Download: 50}}
	rates := Rates(previous, current, 2*time.Second)
	if len(rates) != 1 || rates["a"] != (Rate{Download: 100, Upload: 10}) {
		t.Fatalf("rates = %+v", rates)
	}
	if Rates(previous, current, 0) != nil {
		t.Fatal("间隔为 0 时不应计算速度")
	}
}
