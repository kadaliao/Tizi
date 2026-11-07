package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMatchHostUsesFirstMatchingDomainRule(t *testing.T) {
	rules := []Rule{
		{Type: "DOMAIN-SUFFIX", Payload: "example.com", Proxy: "外网代理"},
		{Type: "MATCH", Proxy: "默认出口"},
	}
	got, ok := MatchHost(rules, "api.example.com")
	if !ok || got.Proxy != "外网代理" {
		t.Fatalf("MatchHost() = %#v, %v", got, ok)
	}
	got, ok = MatchHost([]Rule{{Type: "DomainSuffix", Payload: "example.com", Proxy: "外网代理"}}, "api.example.com")
	if !ok || got.Proxy != "外网代理" {
		t.Fatalf("camel-case MatchHost() = %#v, %v", got, ok)
	}
	got, ok = MatchHost(rules, "other.test")
	if !ok || got.Type != "MATCH" {
		t.Fatalf("fallback MatchHost() = %#v, %v", got, ok)
	}
}

func TestMatchHostSkipsConnectionContextBeforeDomainRules(t *testing.T) {
	rules := []Rule{
		{Type: "AND", Payload: "((NETWORK,UDP),(DST-PORT,443))", Proxy: "REJECT"},
		{Type: "IP-CIDR", Payload: "10.0.0.0/8", Proxy: "内网"},
		{Type: "DOMAIN-SUFFIX", Payload: "example.com", Proxy: "外网代理"},
		{Type: "GEOIP", Payload: "CN", Proxy: "DIRECT"},
	}
	got, ok := MatchHost(rules, "docs.example.com")
	if !ok || got.Proxy != "外网代理" {
		t.Fatalf("MatchHost() = %#v, %v; context rules hid domain match", got, ok)
	}
}

func TestMatchHostDoesNotGuessPastDynamicRule(t *testing.T) {
	rules := []Rule{{Type: "GEOIP", Payload: "CN", Proxy: "DIRECT"}, {Type: "MATCH", Proxy: "默认出口"}}
	if got, ok := MatchHost(rules, "example.cn"); ok || got.Type != "" {
		t.Fatalf("MatchHost() = %#v, %v; want unknown", got, ok)
	}
}

func TestMatchHostDoesNotSkipCompoundDomainRule(t *testing.T) {
	rules := []Rule{
		{Type: "AND", Payload: "((DOMAIN-SUFFIX,example.com),(NETWORK,TCP))", Proxy: "DIRECT"},
		{Type: "MATCH", Proxy: "默认出口"},
	}
	if got, ok := MatchHost(rules, "docs.example.com"); ok || got.Type != "" {
		t.Fatalf("MatchHost() = %#v, %v; want unknown compound-domain result", got, ok)
	}
}

func TestClientSendsAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"version":"test"}`))
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, Secret: "test-secret", HTTP: server.Client()}
	if !client.Healthy() {
		t.Fatal("expected authenticated health check")
	}
}

func TestCloseConnectionsUsesAuthenticatedDelete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete || request.URL.Path != "/connections" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, Secret: "test-secret", HTTP: server.Client()}
	if err := client.CloseConnections(); err != nil {
		t.Fatal(err)
	}
}

func TestReloadConfigForcesReloadWithPath(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut || request.URL.Path != "/configs" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.URL.Query().Get("force") != "true" {
			t.Errorf("query = %q", request.URL.RawQuery)
		}
		if request.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		gotBody, _ = io.ReadAll(request.Body)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, Secret: "test-secret", HTTP: server.Client()}
	if err := client.ReloadConfig("/home/user/.config/tizi/config.yaml"); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body %q: %v", gotBody, err)
	}
	if payload.Path != "/home/user/.config/tizi/config.yaml" {
		t.Fatalf("path = %q", payload.Path)
	}
}

func TestReloadConfigReportsCoreErrorDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"message":"path is not subpath of home directory"}`))
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, HTTP: server.Client()}
	err := client.ReloadConfig("/tmp/outside.yaml")
	if err == nil {
		t.Fatal("expected reload to fail")
	}
	if !strings.Contains(err.Error(), "path is not subpath of home directory") {
		t.Fatalf("error = %v", err)
	}
}

func TestCloseConnectionUsesAuthenticatedDeleteAndEscapesID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete || request.RequestURI != "/connections/active%2F1%20%3F" {
			t.Errorf("request = %s %s", request.Method, request.RequestURI)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, Secret: "test-secret", HTTP: server.Client()}
	if err := client.CloseConnection("active/1 ?"); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseConnection(" "); err == nil {
		t.Fatal("empty connection ID was accepted")
	}
}

func TestCloseConnectionsMatchingOnlyDeletesSelectedConnections(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/connections" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"connections":[{"id":"keep","rulePayload":"other"},{"id":"close-1","rulePayload":"qq.com"},{"id":"close-2","rulePayload":"qq.com"}]}`))
			return
		}
		if request.Method == http.MethodDelete {
			mu.Lock()
			deleted = append(deleted, request.URL.Path)
			mu.Unlock()
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		writer.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, HTTP: server.Client()}
	closed, err := client.CloseConnectionsMatching(func(connection Connection) bool {
		return connection.Payload == "qq.com"
	})
	if err != nil {
		t.Fatal(err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := fmt.Sprint(deleted); got != "[/connections/close-1 /connections/close-2]" {
		t.Fatalf("deleted = %s", got)
	}
}

func TestCloseConnectionsMatchingContinuesAfterDeleteError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"connections":[{"id":"gone"},{"id":"healthy"}]}`))
			return
		}
		if request.URL.Path == "/connections/gone" {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, HTTP: server.Client()}
	closed, err := client.CloseConnectionsMatching(func(Connection) bool { return true })
	if closed != 1 || err == nil {
		t.Fatalf("closed=%d err=%v, want one success and one error", closed, err)
	}
}

func TestProxyDelayUsesAuthenticatedGetAndReturnsDelay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/proxies/openvpn-1/delay" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := request.URL.Query().Get("timeout"); got != "2000" {
			t.Errorf("timeout = %q", got)
		}
		if got := request.URL.Query().Get("url"); got != "http://10.20.0.2:3128/" {
			t.Errorf("url = %q", got)
		}
		if request.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"delay": 42}`))
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, Secret: "test-secret", HTTP: server.Client()}
	delay, err := client.ProxyDelay("openvpn-1", "http://10.20.0.2:3128/", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if delay != 42 {
		t.Fatalf("delay = %d, want 42", delay)
	}
}

func TestConnectionsReturnsTrafficCountersAndProcess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"connections":[{"id":"active-1","download":4096,"upload":1024,"metadata":{"host":"example.com","process":"curl","processPath":"/usr/bin/curl"}}]}`))
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, HTTP: server.Client()}
	connections, err := client.Connections()
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 1 {
		t.Fatalf("Connections() returned %d items, want 1", len(connections))
	}
	connection := connections[0]
	if connection.Download != 4096 || connection.Upload != 1024 {
		t.Fatalf("traffic counters = %d/%d, want 4096/1024", connection.Download, connection.Upload)
	}
	if connection.Metadata.Process != "curl" || connection.Metadata.ProcessPath != "/usr/bin/curl" {
		t.Fatalf("unexpected process metadata: %#v", connection.Metadata)
	}
}

func TestProbeRouteReturnsActualConnectionMetadata(t *testing.T) {
	var sourcePort atomic.Int64
	controllerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if port := sourcePort.Load(); port != 0 {
			_, _ = fmt.Fprintf(writer, `{"connections":[{"id":"probe-1","metadata":{"sourcePort":"%d","destinationIP":"203.0.113.8","destinationPort":"443"},"chains":["hk-node","外网代理"],"rule":"GeoIP","rulePayload":"US"}]}`, port)
			return
		}
		_, _ = writer.Write([]byte(`{"connections":[]}`))
	}))
	defer controllerServer.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		request, readErr := http.ReadRequest(bufio.NewReader(connection))
		if readErr != nil || request.Method != http.MethodConnect || request.Host != "example.com:443" {
			return
		}
		remote, ok := connection.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return
		}
		sourcePort.Store(int64(remote.Port))
		_, _ = fmt.Fprint(connection, "HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer := make([]byte, 1)
		_, _ = connection.Read(buffer)
	}()

	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	mixedPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	client := Client{BaseURL: controllerServer.URL, HTTP: controllerServer.Client()}
	connection, err := client.ProbeRoute(mixedPort, "example.com", 443)
	if err != nil {
		t.Fatal(err)
	}
	if connection.Rule != "GeoIP" || connection.Payload != "US" || connection.Metadata.DestinationIP != "203.0.113.8" {
		t.Fatalf("unexpected probe connection: %#v", connection)
	}
	if len(connection.Chains) != 2 || connection.Chains[0] != "hk-node" {
		t.Fatalf("unexpected probe chain: %#v", connection.Chains)
	}
}

func TestMatchesProbeConnectionUsesInboundIdentity(t *testing.T) {
	probe := probeIdentity{
		sourceIP:        "127.0.0.1",
		sourcePort:      "60123",
		host:            "example.com",
		destinationPort: 443,
		mixedPort:       7890,
	}
	candidate := Connection{ID: "new"}
	candidate.Metadata.SourceIP = "198.18.0.1"
	candidate.Metadata.SourcePort = "60123"
	candidate.Metadata.InboundPort = "59993"
	if matchesProbeConnection(candidate, nil, probe) {
		t.Fatal("TUN connection with a colliding source port matched the probe")
	}
	candidate.Metadata.SourceIP = "127.0.0.1"
	candidate.Metadata.InboundPort = "7890"
	if !matchesProbeConnection(candidate, nil, probe) {
		t.Fatal("mixed-port connection with the probe source identity was not recognized")
	}
}

func TestMatchesProbeConnectionFallsBackToNewHostAndPort(t *testing.T) {
	known := map[string]bool{"existing": true}
	probe := probeIdentity{sourcePort: "60123", host: "example.com", destinationPort: 443, mixedPort: 7890}
	candidate := Connection{ID: "new"}
	candidate.Metadata.Host = "Example.COM."
	candidate.Metadata.DestinationPort = "443"
	if !matchesProbeConnection(candidate, known, probe) {
		t.Fatal("new matching host and destination port was not recognized")
	}
	candidate.ID = "existing"
	if matchesProbeConnection(candidate, known, probe) {
		t.Fatal("existing connection must not match by hostname")
	}
	candidate.ID = "new"
	candidate.Metadata.DestinationPort = "80"
	if matchesProbeConnection(candidate, known, probe) {
		t.Fatal("connection to a different destination port matched")
	}
}

func TestStreamLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/logs" || r.URL.Query().Get("level") != "info" || r.Header.Get("Authorization") != "Bearer s" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"type":"info","payload":"first"}`+"\n"+`{"type":"warning","payload":"second"}`+"\n")
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, Secret: "s", HTTP: server.Client()}
	var got []string
	err := client.StreamLogs(context.Background(), "info", func(level, payload string) {
		got = append(got, level+":"+payload)
	})
	if err == nil {
		t.Fatal("stream must report the closed connection")
	}
	if strings.Join(got, ",") != "info:first,warning:second" {
		t.Fatalf("got %v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.StreamLogs(ctx, "info", func(string, string) {}); err == nil {
		t.Fatal("canceled stream must fail")
	}
}
