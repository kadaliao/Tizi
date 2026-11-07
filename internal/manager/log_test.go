package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
)

func TestRotatingLogWriterKeepsBoundedBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mihomo.log")
	writer, err := newRotatingLogWriter(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first\n", "second\n", "third\n", "fourth\n"} {
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	assertLogFile(t, path, "fourth\n")
	assertLogFile(t, path+".1", "third\n")
	assertLogFile(t, path+".2", "second\n")
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected third backup: %v", err)
	}
}

func TestRotatingLogWriterRemovesLegacyOversizedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mihomo.log")
	if err := os.WriteFile(path, []byte("legacy log that is too large"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := newRotatingLogWriter(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	assertLogFile(t, path, "new\n")
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("oversized legacy log should not become a backup: %v", err)
	}
}

func TestValidateDNSRecoveryGuardArgs(t *testing.T) {
	validPath := filepath.Join(t.TempDir(), "dns-state.json")
	if got, err := validateDNSRecoveryGuardArgs(validPath, 42, "token"); err != nil || got != validPath {
		t.Fatalf("valid args: path=%q err=%v", got, err)
	}
	for _, test := range []struct {
		path  string
		pid   int
		token string
	}{
		{path: "dns-state.json", pid: 42, token: "token"},
		{path: filepath.Join(t.TempDir(), "other.json"), pid: 42, token: "token"},
		{path: validPath, pid: 1, token: "token"},
		{path: validPath, pid: 42},
	} {
		if _, err := validateDNSRecoveryGuardArgs(test.path, test.pid, test.token); err == nil {
			t.Fatalf("invalid args accepted: %#v", test)
		}
	}
}

func TestRouteChangeTrackerWaitsForStableRoute(t *testing.T) {
	tracker := routeChangeTracker{}
	if tracker.observe("无法读取", 3) || tracker.baseline != "" {
		t.Fatal("unavailable route must not become the baseline")
	}
	if tracker.observe("en0 via 192.0.2.1", 3) {
		t.Fatal("initial route must not trigger recovery")
	}
	for attempt := 1; attempt < 3; attempt++ {
		if tracker.observe("en16 via 192.0.2.2", 3) {
			t.Fatalf("route triggered after %d samples", attempt)
		}
	}
	if !tracker.observe("en16 via 192.0.2.2", 3) {
		t.Fatal("stable changed route did not trigger recovery")
	}
	tracker.accept("en16 via 192.0.2.2")
	if tracker.observe("en16 via 192.0.2.2", 3) {
		t.Fatal("accepted route triggered recovery again")
	}
}

func TestRouteChangeTrackerResetsUnstableCandidate(t *testing.T) {
	tracker := routeChangeTracker{}
	tracker.observe("en0 via 192.0.2.1", 2)
	tracker.observe("en16 via 192.0.2.2", 2)
	if tracker.observe("en0 via 192.0.2.1", 2) {
		t.Fatal("route that returned to baseline triggered recovery")
	}
	if tracker.candidate != "" || tracker.samples != 0 {
		t.Fatalf("candidate was not reset: %#v", tracker)
	}
}

func TestTencentDirectConnectionMatcher(t *testing.T) {
	connection := controller.Connection{Chains: []string{"DIRECT"}}
	connection.Metadata.Host = "c2c.cdn.weixin.qq.com"
	if !isTencentDirectConnection(connection) {
		t.Fatal("WeChat CDN direct connection did not match")
	}

	connection.Metadata.Host = "notqq.com"
	if isTencentDirectConnection(connection) {
		t.Fatal("unrelated host with a qq substring matched")
	}
	connection.Metadata.Host = "qq.com.example"
	if isTencentDirectConnection(connection) {
		t.Fatal("host that only starts with a Tencent suffix matched")
	}
	connection.Metadata.Host = "badjs.weixinbridge.com."
	if !isTencentDirectConnection(connection) {
		t.Fatal("observed WeChat telemetry suffix did not match")
	}
	connection.Metadata.Host = ""
	connection.Metadata.DestinationIP = "157.148.62.201"
	if !isTencentDirectConnection(connection) {
		t.Fatal("known Tencent IP range did not match")
	}

	connection.Metadata.DestinationIP = ""
	connection.Metadata.ProcessPath = "/Applications/WeChat.app/Contents/MacOS/WeChat"
	if !isTencentDirectConnection(connection) {
		t.Fatal("WeChat process did not match")
	}
	connection.Chains = []string{"hk-node", "外网代理"}
	if isTencentDirectConnection(connection) {
		t.Fatal("non-DIRECT connection matched")
	}
}

func TestMonitorDefaultRouteRunsDelayedTencentRefresh(t *testing.T) {
	var routeCalls atomic.Int32
	defaultRoute := func() string {
		if routeCalls.Add(1) == 1 {
			return "en0 via 192.0.2.1"
		}
		return "en16 via 192.0.2.2"
	}
	var closeAllCalls atomic.Int32
	targeted := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	var log bytes.Buffer
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute: defaultRoute,
			closeConnections: func() error {
				closeAllCalls.Add(1)
				return nil
			},
			closeTencentConnections: func() (int, error) {
				if closeAllCalls.Load() != 1 {
					t.Errorf("targeted refresh ran before the full refresh")
				}
				targeted <- struct{}{}
				return 2, nil
			},
			logWriter:           &log,
			interval:            time.Millisecond,
			stableSamples:       2,
			tencentRefreshDelay: 0,
			wakeGap:             0,
		})
	}()

	select {
	case <-targeted:
	case <-time.After(time.Second):
		t.Fatal("targeted Tencent refresh did not run")
	}
	close(stop)
	<-done
	if closeAllCalls.Load() != 1 {
		t.Fatalf("full refresh calls = %d, want 1", closeAllCalls.Load())
	}
	if got := log.String(); !bytes.Contains([]byte(got), []byte("已再次刷新 2 条微信直连")) {
		t.Fatalf("missing targeted refresh log: %q", got)
	}
}

func TestMonitorDefaultRouteDoesNotTargetWhenFullRefreshFails(t *testing.T) {
	var routeCalls atomic.Int32
	defaultRoute := func() string {
		if routeCalls.Add(1) == 1 {
			return "en0 via 192.0.2.1"
		}
		return "en16 via 192.0.2.2"
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	fullAttempted := make(chan struct{})
	var once sync.Once
	var targetedCalls atomic.Int32
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute: defaultRoute,
			closeConnections: func() error {
				once.Do(func() { close(fullAttempted) })
				return errors.New("controller unavailable")
			},
			closeTencentConnections: func() (int, error) {
				targetedCalls.Add(1)
				return 0, nil
			},
			logWriter:           &bytes.Buffer{},
			interval:            time.Millisecond,
			stableSamples:       2,
			tencentRefreshDelay: 0,
			wakeGap:             0,
		})
	}()
	select {
	case <-fullAttempted:
	case <-time.After(time.Second):
		t.Fatal("full refresh was not attempted")
	}
	close(stop)
	<-done
	if targetedCalls.Load() != 0 {
		t.Fatalf("targeted refresh calls = %d, want 0", targetedCalls.Load())
	}
}

func TestMonitorDefaultRouteCancelsPendingTencentRefreshOnStop(t *testing.T) {
	var routeCalls atomic.Int32
	defaultRoute := func() string {
		if routeCalls.Add(1) == 1 {
			return "en0 via 192.0.2.1"
		}
		return "en16 via 192.0.2.2"
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	fullRefreshed := make(chan struct{})
	var once sync.Once
	var targetedCalls atomic.Int32
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute: defaultRoute,
			closeConnections: func() error {
				once.Do(func() { close(fullRefreshed) })
				return nil
			},
			closeTencentConnections: func() (int, error) {
				targetedCalls.Add(1)
				return 0, nil
			},
			logWriter:           &bytes.Buffer{},
			interval:            time.Millisecond,
			stableSamples:       2,
			tencentRefreshDelay: time.Hour,
			wakeGap:             0,
		})
	}()
	select {
	case <-fullRefreshed:
	case <-time.After(time.Second):
		t.Fatal("full refresh did not run")
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop while targeted refresh was pending")
	}
	if targetedCalls.Load() != 0 {
		t.Fatalf("targeted refresh calls = %d, want 0", targetedCalls.Load())
	}
}

func TestMonitorDefaultRouteKeepsSamplingWhileTencentRefreshIsPending(t *testing.T) {
	var routeCalls atomic.Int32
	defaultRoute := func() string {
		switch call := routeCalls.Add(1); {
		case call == 1:
			return "en0 via 192.0.2.1"
		case call <= 3:
			return "en16 via 192.0.2.2"
		default:
			return "en7 via 192.0.2.3"
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	secondRefresh := make(chan struct{})
	var closeAllCalls atomic.Int32
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute: defaultRoute,
			closeConnections: func() error {
				if closeAllCalls.Add(1) == 2 {
					close(secondRefresh)
				}
				return nil
			},
			closeTencentConnections: func() (int, error) { return 0, nil },
			logWriter:               &bytes.Buffer{},
			interval:                time.Millisecond,
			stableSamples:           2,
			tencentRefreshDelay:     time.Hour,
			wakeGap:                 0,
		})
	}()
	select {
	case <-secondRefresh:
	case <-time.After(time.Second):
		t.Fatal("second route change was not observed while targeted refresh was pending")
	}
	close(stop)
	<-done
}

func TestControllerFromGeneratedConfigRejectsRemoteAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("external-controller: 203.0.113.5:9090\nsecret: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := controllerFromGeneratedConfig(path); err == nil {
		t.Fatal("remote Controller address was accepted")
	}
}

func TestRecoverWeChatConnectionsClosesOnlyMatchingDirectConnections(t *testing.T) {
	deleted := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/connections" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"connections":[
				{"id":"wechat-direct","chains":["DIRECT"],"metadata":{"host":"c2c.cdn.weixin.qq.com"}},
				{"id":"other-direct","chains":["DIRECT"],"metadata":{"host":"example.com"}},
				{"id":"wechat-proxy","chains":["hk-node","外网代理"],"metadata":{"host":"c2c.cdn.weixin.qq.com"}}
			]}`))
			return
		}
		if request.Method == http.MethodDelete {
			deleted <- request.URL.Path
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		writer.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	generated := filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf("external-controller: %s\nsecret: test-secret\n", strings.TrimPrefix(server.URL, "http://"))
	if err := os.WriteFile(generated, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	man := Manager{Paths: config.Paths{Generated: generated}}
	closed, err := man.RecoverWeChatConnections()
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
	select {
	case path := <-deleted:
		if path != "/connections/wechat-direct" {
			t.Fatalf("deleted %q", path)
		}
	default:
		t.Fatal("matching WeChat connection was not deleted")
	}
	if len(deleted) != 0 {
		t.Fatalf("unexpected additional deletes: %d", len(deleted))
	}
}

func TestCurrentVPNProxyFindsSelectedOpenVPNNode(t *testing.T) {
	groups := []controller.Group{
		{Name: "外网代理", Now: "hk-node", Choices: []string{"hk-node", "DIRECT"}},
		{Name: vpnGroupName, Now: "openvpn-2", Choices: []string{"openvpn-1", "openvpn-2", "openvpn-3"}},
	}
	if got := currentVPNProxy(groups); got != "openvpn-2" {
		t.Fatalf("currentVPNProxy = %q, want openvpn-2", got)
	}
	if got := currentVPNProxy([]controller.Group{{Name: vpnGroupName, Now: "DIRECT"}}); got != "" {
		t.Fatalf("non-openvpn now = %q, want empty", got)
	}
	if got := currentVPNProxy(nil); got != "" {
		t.Fatalf("missing group = %q, want empty", got)
	}
}

func TestNextVPNProxyRotatesAcrossRemotes(t *testing.T) {
	groups := []controller.Group{
		{Name: vpnGroupName, Now: "openvpn-1", Choices: []string{"openvpn-1", "openvpn-2", "openvpn-3"}},
	}
	if got := nextVPNProxy(groups, "openvpn-2"); got != "openvpn-3" {
		t.Fatalf("next = %q, want openvpn-3", got)
	}
	if got := nextVPNProxy(groups, "openvpn-3"); got != "openvpn-1" {
		t.Fatalf("wrap = %q, want openvpn-1", got)
	}
	if got := nextVPNProxy([]controller.Group{{Name: vpnGroupName, Choices: []string{"openvpn-1"}}}, "openvpn-1"); got != "" {
		t.Fatalf("single remote = %q, want empty", got)
	}
	if got := nextVPNProxy(groups, "missing"); got != "openvpn-1" {
		t.Fatalf("missing current = %q, want openvpn-1", got)
	}
}

func TestVPNHealthyReportsDelayResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/proxies":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"proxies":{"内网":{"type":"Selector","now":"openvpn-1","all":["openvpn-1"]}}}`))
		case request.Method == http.MethodGet && request.URL.Path == "/proxies/openvpn-1/delay":
			if request.URL.Query().Get("url") != "http://wiki.corp.example.com/" {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"delay": 12}`))
		default:
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := controller.Client{BaseURL: server.URL, HTTP: server.Client()}
	if !vpnHealthy(client, "http://wiki.corp.example.com/") {
		t.Fatal("healthy VPN reported unhealthy")
	}
}

func TestVPNHealthyReportsFailureWhenDelayFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/proxies" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"proxies":{"内网":{"type":"Selector","now":"openvpn-1","all":["openvpn-1"]}}}`))
			return
		}
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := controller.Client{BaseURL: server.URL, HTTP: server.Client()}
	if vpnHealthy(client, "http://wiki.corp.example.com/") {
		t.Fatal("unhealthy VPN reported healthy")
	}
}

func TestRecoverVPNClosesConnectionsAndSwitchesRemote(t *testing.T) {
	var mu sync.Mutex
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodDelete && request.URL.Path == "/connections":
			actions = append(actions, "close")
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/proxies":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"proxies":{"内网":{"type":"Selector","now":"openvpn-1","all":["openvpn-1","openvpn-2","openvpn-3"]}}}`))
		case request.Method == http.MethodPut && request.URL.Path == "/proxies/内网":
			actions = append(actions, "select")
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPut && request.URL.Path == "/configs":
			actions = append(actions, "reload")
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := controller.Client{BaseURL: server.URL, Secret: "test-secret", HTTP: server.Client()}
	recovery, err := recoverVPN(client, filepath.Join(t.TempDir(), "config.yaml"), true)
	if err != nil {
		t.Fatal(err)
	}
	if recovery != vpnRecoverySwitched {
		t.Fatalf("recovery with multiple remotes = %v, want switch", recovery)
	}
	mu.Lock()
	defer mu.Unlock()
	// 有备用 remote 时不应重载配置：重载会断开全部连接。
	if got := fmt.Sprint(actions); got != "[close select]" {
		t.Fatalf("actions = %s", got)
	}
}

func TestRecoverVPNRebuildsTunnelWhenSingleRemote(t *testing.T) {
	var mu sync.Mutex
	var selected atomic.Int32
	var reloadQuery string
	var reloadBody string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodDelete && request.URL.Path == "/connections":
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/proxies":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"proxies":{"内网":{"type":"Selector","now":"openvpn-1","all":["openvpn-1"]}}}`))
		case request.Method == http.MethodPut && request.URL.Path == "/proxies/内网":
			selected.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPut && request.URL.Path == "/configs":
			data, _ := io.ReadAll(request.Body)
			reloadQuery = request.URL.RawQuery
			reloadBody = string(data)
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	generated := filepath.Join(t.TempDir(), "config.yaml")
	client := controller.Client{BaseURL: server.URL, HTTP: server.Client()}
	recovery, err := recoverVPN(client, generated, true)
	if err != nil {
		t.Fatal(err)
	}
	if recovery != vpnRecoveryRebuilt {
		t.Fatalf("single remote recovery = %v, want rebuild", recovery)
	}
	if selected.Load() != 0 {
		t.Fatalf("single remote triggered a select: %d", selected.Load())
	}
	if reloadQuery != "force=true" {
		t.Fatalf("reload query = %q, want force=true", reloadQuery)
	}
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(reloadBody), &payload); err != nil {
		t.Fatalf("reload body %q: %v", reloadBody, err)
	}
	if payload.Path != generated {
		t.Fatalf("reload path = %q, want %q", payload.Path, generated)
	}
}

func TestRecoverVPNSkipsRebuildWithoutConfigPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodDelete && request.URL.Path == "/connections":
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/proxies":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"proxies":{"内网":{"type":"Selector","now":"openvpn-1","all":["openvpn-1"]}}}`))
		case request.Method == http.MethodPut:
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := controller.Client{BaseURL: server.URL, HTTP: server.Client()}
	recovery, err := recoverVPN(client, "  ", true)
	if err != nil {
		t.Fatal(err)
	}
	if recovery != vpnRecoveryNone {
		t.Fatalf("recovery without config path = %v, want none", recovery)
	}
}

func TestMonitorDefaultRouteRecoversVPNAfterWake(t *testing.T) {
	var routeCalls atomic.Int32
	defaultRoute := func() string {
		if routeCalls.Add(1) == 2 {
			// 仅在第一次 tick 阻塞超过 wakeGap，模拟一次系统睡眠造成的
			// 时间跳变；之后恢复正常节奏，避免反复触发 wake 分支。
			time.Sleep(30 * time.Millisecond)
		}
		return "en0 via 192.0.2.1"
	}
	recovered := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	var log bytes.Buffer
	var healthChecks atomic.Int32
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute:     defaultRoute,
			closeConnections: func() error { return nil },
			checkVPN: func() bool {
				// 恢复前第一次检查失败，恢复后复检成功。
				return healthChecks.Add(1) > 1
			},
			recoverVPN: func(bool) (vpnRecovery, error) {
				recovered <- struct{}{}
				return vpnRecoverySwitched, nil
			},
			logWriter:       &log,
			interval:        time.Millisecond,
			stableSamples:   2,
			wakeGap:         5 * time.Millisecond,
			wakeSettleDelay: time.Millisecond,
		})
	}()

	select {
	case <-recovered:
	case <-time.After(time.Second):
		t.Fatal("VPN recovery did not run after wake")
	}
	close(stop)
	<-done
	if got := log.String(); !bytes.Contains([]byte(got), []byte("已切换 OpenVPN remote 恢复连接")) {
		t.Fatalf("missing recovery log: %q", got)
	}
}

func TestMonitorDefaultRouteSkipsRecoveryWhenVPNHealthy(t *testing.T) {
	var routeCalls atomic.Int32
	defaultRoute := func() string {
		if routeCalls.Add(1) == 2 {
			time.Sleep(30 * time.Millisecond)
		}
		return "en0 via 192.0.2.1"
	}
	checked := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	var log bytes.Buffer
	var recoveredCalls atomic.Int32
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute:     defaultRoute,
			closeConnections: func() error { return nil },
			checkVPN: func() bool {
				checked <- struct{}{}
				return true
			},
			recoverVPN: func(bool) (vpnRecovery, error) {
				recoveredCalls.Add(1)
				return vpnRecoverySwitched, nil
			},
			logWriter:       &log,
			interval:        time.Millisecond,
			stableSamples:   2,
			wakeGap:         5 * time.Millisecond,
			wakeSettleDelay: time.Millisecond,
		})
	}()

	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("health check did not run after wake")
	}
	close(stop)
	<-done
	if recoveredCalls.Load() != 0 {
		t.Fatalf("recovery ran despite healthy VPN: %d", recoveredCalls.Load())
	}
	if got := log.String(); !bytes.Contains([]byte(got), []byte("VPN 连接正常")) {
		t.Fatalf("missing healthy log: %q", got)
	}
}

func TestRecoverAfterWakeVerifiesRecovery(t *testing.T) {
	var log bytes.Buffer

	recovered := false
	monitor := routeMonitor{
		defaultRoute: func() string { return "en0 via 192.0.2.1" },
		checkVPN:     func() bool { return true },
		recoverVPN:   func(bool) (vpnRecovery, error) { recovered = true; return vpnRecoverySwitched, nil },
		logWriter:    &log,
	}
	if !monitor.recoverAfterWake() || recovered {
		t.Fatal("healthy wake should complete without recovery")
	}

	monitor = routeMonitor{
		defaultRoute: func() string { return "en0 via 192.0.2.1" },
		checkVPN:     func() bool { return false },
		recoverVPN:   func(bool) (vpnRecovery, error) { return vpnRecoverySwitched, nil },
		logWriter:    &log,
	}
	if monitor.recoverAfterWake() {
		t.Fatal("failed post-recovery recheck should request retry")
	}

	monitor = routeMonitor{
		defaultRoute: func() string { return "en0 via 192.0.2.1" },
		checkVPN:     func() bool { return false },
		recoverVPN:   func(bool) (vpnRecovery, error) { return vpnRecoveryNone, errors.New("controller unavailable") },
		logWriter:    &log,
	}
	if monitor.recoverAfterWake() {
		t.Fatal("failed recovery should request retry")
	}

	monitor = routeMonitor{
		defaultRoute: func() string { return "无法读取" },
		logWriter:    &log,
	}
	if monitor.recoverAfterWake() {
		t.Fatal("unavailable route should request retry")
	}
}

func TestMonitorDefaultRouteRecoversVPNWhenHealthChecksFail(t *testing.T) {
	defaultRoute := func() string { return "en0 via 192.0.2.1" }
	recovered := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	var log bytes.Buffer
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute:     defaultRoute,
			closeConnections: func() error { return nil },
			checkVPN:         func() bool { return false },
			recoverVPN: func(bool) (vpnRecovery, error) {
				select {
				case recovered <- struct{}{}:
				default:
				}
				return vpnRecoverySwitched, nil
			},
			logWriter:      &log,
			interval:       time.Millisecond,
			stableSamples:  2,
			wakeGap:        0,
			healthInterval: time.Millisecond,
			healthFailMax:  2,
		})
	}()

	select {
	case <-recovered:
	case <-time.After(time.Second):
		t.Fatal("VPN recovery did not run after health checks failed")
	}
	close(stop)
	<-done
	if got := log.String(); !bytes.Contains([]byte(got), []byte("已切换 OpenVPN remote 主动恢复连接")) {
		t.Fatalf("missing recovery log: %q", got)
	}
}

func TestMonitorDefaultRouteRequiresConsecutiveHealthFailures(t *testing.T) {
	var checks atomic.Int32
	var recoveredCalls atomic.Int32
	thirdCheck := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute:     func() string { return "en0 via 192.0.2.1" },
			closeConnections: func() error { return nil },
			checkVPN: func() bool {
				// 第一次失败，第二次及之后恢复健康：单次抖动不应触发恢复。
				if checks.Add(1) == 3 {
					thirdCheck <- struct{}{}
				}
				return checks.Load() >= 2
			},
			recoverVPN: func(bool) (vpnRecovery, error) {
				recoveredCalls.Add(1)
				return vpnRecoverySwitched, nil
			},
			logWriter:      &bytes.Buffer{},
			interval:       time.Millisecond,
			stableSamples:  2,
			wakeGap:        0,
			healthInterval: time.Millisecond,
			healthFailMax:  2,
		})
	}()

	select {
	case <-thirdCheck:
	case <-time.After(time.Second):
		t.Fatal("health checks did not run")
	}
	close(stop)
	<-done
	if recoveredCalls.Load() != 0 {
		t.Fatalf("recovery ran despite transient failure: %d", recoveredCalls.Load())
	}
}

func TestRecoverVPNWithoutCloseAllOnlyClosesVPNConnections(t *testing.T) {
	var mu sync.Mutex
	var closed []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/connections":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"connections":[
				{"id":"vpn-group","chains":["openvpn-1","内网"],"metadata":{"host":"git.corp.example.com"}},
				{"id":"vpn-node","chains":["openvpn-2"],"metadata":{"host":"10.0.0.8"}},
				{"id":"direct","chains":["DIRECT"],"metadata":{"host":"example.com"}},
				{"id":"proxy","chains":["hk-node","外网代理"],"metadata":{"host":"github.com"}}
			]}`))
		case request.Method == http.MethodDelete && request.URL.Path == "/connections":
			t.Error("VPN-only recovery closed all connections")
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/connections/"):
			closed = append(closed, strings.TrimPrefix(request.URL.Path, "/connections/"))
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/proxies":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"proxies":{"内网":{"type":"Selector","now":"openvpn-1","all":["openvpn-1"]}}}`))
		default:
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := controller.Client{BaseURL: server.URL, HTTP: server.Client()}
	if _, err := recoverVPN(client, "", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(closed)
	if got := fmt.Sprint(closed); got != "[vpn-group vpn-node]" {
		t.Fatalf("closed = %s", got)
	}
}

func TestMonitorDefaultRouteBacksOffRepeatedHealthRecoveries(t *testing.T) {
	var checks atomic.Int32
	var recoveries []int32
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	enough := make(chan struct{}, 1)
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute:     func() string { return "en0 via 192.0.2.1" },
			closeConnections: func() error { return nil },
			checkVPN: func() bool {
				if checks.Add(1) >= 40 {
					select {
					case enough <- struct{}{}:
					default:
					}
				}
				return false
			},
			recoverVPN: func(closeAll bool) (vpnRecovery, error) {
				if closeAll {
					t.Error("periodic health recovery closed all connections")
				}
				mu.Lock()
				recoveries = append(recoveries, checks.Load())
				mu.Unlock()
				return vpnRecoverySwitched, nil
			},
			logWriter:      &bytes.Buffer{},
			interval:       time.Millisecond,
			stableSamples:  2,
			wakeGap:        0,
			healthInterval: time.Millisecond,
			healthFailMax:  2,
		})
	}()

	select {
	case <-enough:
	case <-time.After(5 * time.Second):
		t.Fatal("health checks did not run")
	}
	close(stop)
	<-done
	mu.Lock()
	defer mu.Unlock()
	// 阈值 2：第 2 次失败恢复，之后阈值翻倍为 4、8、16……在 40 次探测内
	// 应恢复于第 2、6、14、30 次，而不是每 2 次一次。
	want := []int32{2, 6, 14, 30}
	if len(recoveries) < len(want) {
		t.Fatalf("recoveries = %v, want at least %v", recoveries, want)
	}
	for index, check := range want {
		if recoveries[index] != check {
			t.Fatalf("recoveries = %v, want prefix %v", recoveries, want)
		}
	}
	if len(recoveries) > len(want) {
		t.Fatalf("recovery ran without backoff: %v", recoveries)
	}
}

func TestMonitorDefaultRouteResetsBackoffAfterHealthyCheck(t *testing.T) {
	var checks atomic.Int32
	var recoveries atomic.Int32
	stop := make(chan struct{})
	done := make(chan struct{})
	enough := make(chan struct{}, 1)
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute:     func() string { return "en0 via 192.0.2.1" },
			closeConnections: func() error { return nil },
			checkVPN: func() bool {
				n := checks.Add(1)
				if n >= 7 {
					select {
					case enough <- struct{}{}:
					default:
					}
					return true
				}
				// 失败、失败(恢复)、成功(重置)、失败、失败(恢复)、失败、……
				return n == 3
			},
			recoverVPN: func(bool) (vpnRecovery, error) {
				recoveries.Add(1)
				return vpnRecoverySwitched, nil
			},
			logWriter:      &bytes.Buffer{},
			interval:       time.Millisecond,
			stableSamples:  2,
			wakeGap:        0,
			healthInterval: time.Millisecond,
			healthFailMax:  2,
		})
	}()

	select {
	case <-enough:
	case <-time.After(5 * time.Second):
		t.Fatal("health checks did not run")
	}
	close(stop)
	<-done
	if got := recoveries.Load(); got != 2 {
		t.Fatalf("recoveries = %d, want 2 (backoff must reset after a healthy check)", got)
	}
}

func assertLogFile(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("%s = %q, want %q", filepath.Base(path), data, expected)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %o, want 600", filepath.Base(path), info.Mode().Perm())
	}
}

func TestClaudeLogWatcherFindsEventsAcrossWrites(t *testing.T) {
	events := make(chan claudeguard.LogEvent, 4)
	watcher := &claudeLogWatcher{events: events}
	lines := `time="2026-10-27T20:00:00+08:00" level=info msg="[TCP] 28.0.0.1:1 --> claude.ai:443 match DomainSuffix(claude.ai) using Claude[住宅节点]"` + "\n" +
		`time="2026-10-27T20:00:01+08:00" level=warning msg="[TCP] dial Claude (match DomainSuffix/claude.ai) 28.0.0.1:2 --> claude.ai:443 error: \"connect\" failed"` + "\n" +
		`time="2026-10-27T20:00:02+08:00" level=info msg="[TCP] 28.0.0.1:3 --> api.anthropic.com:443 match Match using 外网代理[DIRECT]"` + "\n"
	// 按任意位置切开写入，模拟管道分块。
	for _, chunk := range []string{lines[:37], lines[37:190], lines[190:]} {
		if n, err := watcher.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	close(events)
	var got []claudeguard.LogEvent
	for event := range events {
		got = append(got, event)
	}
	if len(got) != 2 || got[0].Error != `"connect" failed` || !got[1].Direct || got[1].Host != "api.anthropic.com" {
		t.Fatalf("events = %+v", got)
	}
}

func TestClaudeLogWatcherNeverBlocks(t *testing.T) {
	watcher := &claudeLogWatcher{events: make(chan claudeguard.LogEvent)}
	line := []byte(`level=info msg="[TCP] 28.0.0.1:3 --> claude.ai:443 match Match using DIRECT"` + "\n")
	done := make(chan struct{})
	go func() {
		_, _ = watcher.Write(line)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a full event channel must not block Mihomo's log output")
	}
}

func TestVPNProbeReadsGeneratedConfigAndSettings(t *testing.T) {
	dir := t.TempDir()
	generated := filepath.Join(dir, "config.yaml")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(generated, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("proxies:\n  - {name: hk, type: trojan, server: hk.example.com, port: 443}\n")
	if hasVPN, probe := vpnProbe(generated); hasVPN || probe != "" {
		t.Fatalf("no ovpn: hasVPN=%v probe=%q", hasVPN, probe)
	}
	write("proxies:\n  - {name: openvpn-1, type: openvpn, server: vpn.example.com, port: 1194}\n")
	if hasVPN, probe := vpnProbe(generated); !hasVPN || probe != "" {
		t.Fatalf("ovpn without probe: hasVPN=%v probe=%q", hasVPN, probe)
	}
	write("proxies:\n  - {name: openvpn-1, type: openvpn, server: vpn.example.com, port: 1194}\n  - {name: intranet-http, type: http, server: 10.20.0.2, port: 3128}\n")
	if _, probe := vpnProbe(generated); probe != "http://10.20.0.2:3128" {
		t.Fatalf("probe should default to the intranet HTTP egress, got %q", probe)
	}
	settings := config.DefaultSettings()
	settings.IntranetProbeURL = "http://wiki.corp.example.com/"
	if err := config.Save(config.Paths{ConfigDir: dir, DataDir: dir, CoreBin: filepath.Join(dir, "bin", "mihomo"), Settings: filepath.Join(dir, "settings.json")}, settings); err != nil {
		t.Fatal(err)
	}
	if _, probe := vpnProbe(generated); probe != "http://wiki.corp.example.com/" {
		t.Fatalf("explicit probe URL must win, got %q", probe)
	}
}
