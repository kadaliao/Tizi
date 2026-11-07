package system

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type fakeDNSRunner struct {
	servers           []string
	automatic         bool
	searchDomains     []string
	failNextSet       bool
	failNextSearchSet bool
	// v6 是各网络服务的 IPv6 配置，缺省为 automatic。
	v6 map[string]IPv6ServiceState
	// failV6LimitOn 非空时，限制该服务 IPv6 的命令失败；failInfoOn 同理，
	// 让读取该服务配置的命令失败。
	failV6LimitOn string
	failInfoOn    string
	calls         [][]string
}

func (f *fakeDNSRunner) v6State(service string) IPv6ServiceState {
	if state, ok := f.v6[service]; ok {
		return state
	}
	return IPv6ServiceState{Service: service, Mode: ipv6ModeAutomatic}
}

func (f *fakeDNSRunner) setV6(state IPv6ServiceState) {
	if f.v6 == nil {
		f.v6 = make(map[string]IPv6ServiceState)
	}
	f.v6[state.Service] = state
}

func (f *fakeDNSRunner) v6Info(service string) string {
	state := f.v6State(service)
	switch state.Mode {
	case ipv6ModeManual:
		return fmt.Sprintf("IPv6: Manual\nIPv6 IP address: %s\nIPv6 Prefix Length: %d\nIPv6 Router: %s\n", state.Address, state.PrefixLength, state.Router)
	case ipv6ModeLinkLocal, ipv6ModeOff:
		// 真机上「仅本地链路」不输出 IPv6: 这一行（「关闭」未在真机确认，按同样
		// 处理），只剩下面两行。
		return "IPv6 IP address: none\nIPv6 Router: none\n"
	default:
		return "IPv6: Automatic\nIPv6 IP address: none\nIPv6 Router: none\n"
	}
}

func (f *fakeDNSRunner) Output(name string, args ...string) ([]byte, error) {
	return f.output(false, name, args...)
}

func (f *fakeDNSRunner) PrivilegedOutput(name string, args ...string) ([]byte, error) {
	return f.output(true, name, args...)
}

func (f *fakeDNSRunner) output(privileged bool, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	if privileged {
		call = append([]string{"sudo", "-n"}, call...)
	}
	f.calls = append(f.calls, call)
	switch {
	case name == "route":
		return []byte("gateway: 192.168.1.1\ninterface: en1\n"), nil
	case name == "networksetup" && len(args) > 0 && args[0] == "-listnetworkserviceorder":
		return []byte("(1) Ethernet\n(Hardware Port: Ethernet, Device: en0)\n(2) Wi-Fi\n(Hardware Port: Wi-Fi, Device: en1)\n(3) Tailscale\n(Hardware Port: io.tailscale.ipn.macsys, Device: )\n"), nil
	case name == "networksetup" && len(args) == 2 && args[0] == "-getinfo":
		if args[1] == f.failInfoOn {
			return []byte("injected info failure"), fmt.Errorf("exit 1")
		}
		return []byte(f.v6Info(args[1])), nil
	case name == "networksetup" && len(args) >= 2 && args[0] == "-setv6LinkLocal":
		if args[1] == f.failV6LimitOn {
			return []byte("injected v6 failure"), fmt.Errorf("exit 1")
		}
		f.setV6(IPv6ServiceState{Service: args[1], Mode: ipv6ModeLinkLocal})
		return nil, nil
	case name == "networksetup" && len(args) >= 2 && args[0] == "-setv6automatic":
		f.setV6(IPv6ServiceState{Service: args[1], Mode: ipv6ModeAutomatic})
		return nil, nil
	case name == "networksetup" && len(args) == 5 && args[0] == "-setv6manual":
		prefix, _ := strconv.Atoi(args[3])
		f.setV6(IPv6ServiceState{Service: args[1], Mode: ipv6ModeManual, Address: args[2], PrefixLength: prefix, Router: args[4]})
		return nil, nil
	case name == "networksetup" && len(args) > 1 && args[0] == "-getdnsservers":
		if f.automatic {
			return []byte("There aren't any DNS Servers set on Wi-Fi.\n"), nil
		}
		return []byte(strings.Join(f.servers, "\n") + "\n"), nil
	case name == "networksetup" && len(args) > 1 && args[0] == "-getsearchdomains":
		if len(f.searchDomains) == 0 {
			return []byte("There aren't any Search Domains set on Wi-Fi.\n"), nil
		}
		return []byte(strings.Join(f.searchDomains, "\n") + "\n"), nil
	case name == "networksetup" && len(args) > 2 && args[0] == "-setdnsservers":
		if f.failNextSet {
			f.failNextSet = false
			return []byte("injected set failure"), fmt.Errorf("exit 1")
		}
		if len(args) == 3 && args[2] == "Empty" {
			f.automatic = true
			f.servers = nil
		} else {
			f.automatic = false
			f.servers = append([]string(nil), args[2:]...)
		}
		return nil, nil
	case name == "networksetup" && len(args) > 2 && args[0] == "-setsearchdomains":
		if f.failNextSearchSet {
			f.failNextSearchSet = false
			return []byte("injected search set failure"), fmt.Errorf("exit 1")
		}
		if len(args) == 3 && args[2] == "Empty" {
			f.searchDomains = nil
		} else {
			f.searchDomains = append([]string(nil), args[2:]...)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command: %v", call)
	}
}

func (f *fakeDNSRunner) Run(name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil
}

func useTempResolverDir(t *testing.T) string {
	t.Helper()
	old := resolverDir
	dir := filepath.Join(t.TempDir(), "resolver")
	resolverDir = dir
	t.Cleanup(func() { resolverDir = old })
	return dir
}

func TestParseNetworkService(t *testing.T) {
	output := "(1) Ethernet\n(Hardware Port: Ethernet, Device: en0)\n(4) Wi-Fi\n(Hardware Port: Wi-Fi, Device: en1)\n"
	if got := parseNetworkService(output, "en1"); got != "Wi-Fi" {
		t.Fatalf("service = %q, want Wi-Fi", got)
	}
}

func TestParseDNSServers(t *testing.T) {
	servers, automatic, err := parseDNSServers("114.114.114.114\n223.5.5.5\n")
	if err != nil || automatic || !reflect.DeepEqual(servers, []string{"114.114.114.114", "223.5.5.5"}) {
		t.Fatalf("servers=%v automatic=%v err=%v", servers, automatic, err)
	}
	servers, automatic, err = parseDNSServers("There aren't any DNS Servers set on Wi-Fi.\n")
	if err != nil || !automatic || len(servers) != 0 {
		t.Fatalf("automatic parse: servers=%v automatic=%v err=%v", servers, automatic, err)
	}
}

func TestParseSearchDomains(t *testing.T) {
	domains, err := parseSearchDomains("example.com\nnode.example.com\n")
	if err != nil || !reflect.DeepEqual(domains, []string{"example.com", "node.example.com"}) {
		t.Fatalf("domains=%v err=%v", domains, err)
	}
	domains, err = parseSearchDomains("There aren't any Search Domains set on Wi-Fi.\n")
	if err != nil || len(domains) != 0 {
		t.Fatalf("empty search domains: domains=%v err=%v", domains, err)
	}
}

func TestAdministratorAuthorizationCommand(t *testing.T) {
	cmd := AdministratorAuthorizationCommand()
	if got := strings.Join(cmd.Args, " "); got != "sudo -v" {
		t.Fatalf("authorization command = %q", got)
	}
}

func TestDNSWritesUsePrivilegedRunner(t *testing.T) {
	runner := &fakeDNSRunner{}
	if err := setDNS(runner, "Wi-Fi", nil); err != nil {
		t.Fatal(err)
	}
	if err := setSearchDomains(runner, "Wi-Fi", []string{"corp"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if len(call) < 3 || call[0] != "sudo" || call[1] != "-n" || call[2] != "networksetup" {
			t.Fatalf("write call was not privileged: %v", call)
		}
	}
}

// testDNSDomains 是测试用的私有域名：一个内网域名加 MagicDNS。
var testDNSDomains = DNSDomains{Private: []string{"corp.example.com", "ts.net"}, Search: []string{"corp.example.com"}}

func TestPrepareAndRestoreDNS(t *testing.T) {
	resolverDir := useTempResolverDir(t)
	runner := &fakeDNSRunner{
		servers:       []string{"9.9.9.9"},
		searchDomains: []string{"custom.example", "LAB.EXAMPLE.ORG."},
	}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareDNS(statePath, 1053, testDNSDomains, runner); err != nil {
		t.Fatal(err)
	}
	if !runner.automatic {
		t.Fatal("prepare did not reset DNS to automatic")
	}
	wantRuntimeSearch := []string{"custom.example", "LAB.EXAMPLE.ORG.", "corp.example.com"}
	if !reflect.DeepEqual(runner.searchDomains, wantRuntimeSearch) {
		t.Fatalf("runtime search domains=%v, want %v", runner.searchDomains, wantRuntimeSearch)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state DNSState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Service != "Wi-Fi" || state.Automatic || !reflect.DeepEqual(state.Servers, []string{"9.9.9.9"}) {
		t.Fatalf("unexpected state: %#v", state)
	}
	if !state.SearchDomainsCaptured || !reflect.DeepEqual(state.SearchDomains, []string{"custom.example", "LAB.EXAMPLE.ORG."}) {
		t.Fatalf("search domains were not captured: %#v", state)
	}
	resolverContent, err := os.ReadFile(filepath.Join(resolverDir, "corp.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resolverContent), "port 1053") {
		t.Fatalf("resolver did not point to mihomo DNS: %s", resolverContent)
	}
	tailnetResolverContent, err := os.ReadFile(filepath.Join(resolverDir, "ts.net"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tailnetResolverContent), "port 1053") {
		t.Fatalf("ts.net resolver did not point to mihomo DNS: %s", tailnetResolverContent)
	}
	twimgResolverContent, err := os.ReadFile(filepath.Join(resolverDir, "twimg.com"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(twimgResolverContent), "port 1053") {
		t.Fatalf("twimg.com resolver did not point to mihomo DNS: %s", twimgResolverContent)
	}

	restored, err := restoreDNS(statePath, runner)
	if err != nil || !restored {
		t.Fatalf("restored=%v err=%v", restored, err)
	}
	if runner.automatic || !reflect.DeepEqual(runner.servers, []string{"9.9.9.9"}) {
		t.Fatalf("DNS was not restored: automatic=%v servers=%v", runner.automatic, runner.servers)
	}
	if !reflect.DeepEqual(runner.searchDomains, []string{"custom.example", "LAB.EXAMPLE.ORG."}) {
		t.Fatalf("search domains were not restored: %v", runner.searchDomains)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("restored state file still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(resolverDir, "corp.example.com")); !os.IsNotExist(err) {
		t.Fatalf("resolver file was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(resolverDir, "ts.net")); !os.IsNotExist(err) {
		t.Fatalf("ts.net resolver file was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(resolverDir, "twimg.com")); !os.IsNotExist(err) {
		t.Fatalf("twimg.com resolver file was not removed: %v", err)
	}
}

func TestRestoreDNSExplainsUnreadableState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 000 files")
	}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := os.WriteFile(statePath, []byte(`{"service":"Wi-Fi","automatic":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(statePath, 0); err != nil {
		t.Fatal(err)
	}

	restored, err := restoreDNS(statePath, &fakeDNSRunner{})
	if restored || err == nil {
		t.Fatalf("restored=%v err=%v", restored, err)
	}
	for _, want := range []string{"请勿删除", "sudo chown", statePath} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestRestoreAutomaticDNSAfterRuntimeOverride(t *testing.T) {
	useTempResolverDir(t)
	runner := &fakeDNSRunner{automatic: true}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareDNS(statePath, 1053, testDNSDomains, runner); err != nil {
		t.Fatal(err)
	}

	runner.automatic = false
	runner.servers = []string{"114.114.114.114"}
	restored, err := restoreDNS(statePath, runner)
	if err != nil || !restored {
		t.Fatalf("restored=%v err=%v", restored, err)
	}
	if !runner.automatic || len(runner.servers) != 0 {
		t.Fatalf("automatic DNS was not restored: automatic=%v servers=%v", runner.automatic, runner.servers)
	}
}

func TestPrepareDNSRollsBackSetFailure(t *testing.T) {
	resolverDir := useTempResolverDir(t)
	runner := &fakeDNSRunner{
		servers:       []string{"9.9.9.9"},
		searchDomains: []string{"custom.example"},
		failNextSet:   true,
	}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareDNS(statePath, 1053, testDNSDomains, runner); err == nil || !strings.Contains(err.Error(), "injected set failure") {
		t.Fatalf("expected injected prepare error, got %v", err)
	}
	if runner.automatic || !reflect.DeepEqual(runner.servers, []string{"9.9.9.9"}) {
		t.Fatalf("DNS was not rolled back: automatic=%v servers=%v", runner.automatic, runner.servers)
	}
	if !reflect.DeepEqual(runner.searchDomains, []string{"custom.example"}) {
		t.Fatalf("search domains were not rolled back: %v", runner.searchDomains)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rollback left DNS state behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(resolverDir, "corp.example.com")); !os.IsNotExist(err) {
		t.Fatalf("rollback left resolver behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(resolverDir, "ts.net")); !os.IsNotExist(err) {
		t.Fatalf("rollback left ts.net resolver behind: %v", err)
	}
}

func TestPrepareDNSRollsBackSearchDomainFailure(t *testing.T) {
	resolverDir := useTempResolverDir(t)
	runner := &fakeDNSRunner{
		automatic:         true,
		searchDomains:     []string{"custom.example"},
		failNextSearchSet: true,
	}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareDNS(statePath, 1053, testDNSDomains, runner); err == nil || !strings.Contains(err.Error(), "injected search set failure") {
		t.Fatalf("expected injected search-domain error, got %v", err)
	}
	if !runner.automatic || !reflect.DeepEqual(runner.searchDomains, []string{"custom.example"}) {
		t.Fatalf("state was not rolled back: automatic=%v search=%v", runner.automatic, runner.searchDomains)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rollback left DNS state behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(resolverDir, "ts.net")); !os.IsNotExist(err) {
		t.Fatalf("rollback left resolver behind: %v", err)
	}
}

func TestRestoreLegacyStateDoesNotChangeSearchDomains(t *testing.T) {
	useTempResolverDir(t)
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := os.WriteFile(statePath, []byte(`{"service":"Wi-Fi","automatic":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDNSRunner{searchDomains: []string{"custom.example"}}
	restored, err := restoreDNS(statePath, runner)
	if err != nil || !restored {
		t.Fatalf("restored=%v err=%v", restored, err)
	}
	if !reflect.DeepEqual(runner.searchDomains, []string{"custom.example"}) {
		t.Fatalf("legacy state changed search domains: %v", runner.searchDomains)
	}
}

func TestRecoverDNSWithoutSnapshotLeavesServiceDNSAlone(t *testing.T) {
	dir := useTempResolverDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(dir, resolverDomains(testDNSDomains.Private)[0])
	if err := os.WriteFile(managed, managedResolverContent(1053), 0o644); err != nil {
		t.Fatal(err)
	}
	// 用户手动设置的公共 DNS 与本工具默认上游相同，但没有快照就不能动它。
	runner := &fakeDNSRunner{servers: []string{"223.5.5.5"}}
	recovered, err := recoverDNS(filepath.Join(t.TempDir(), "missing.json"), runner)
	if err != nil || recovered || runner.automatic {
		t.Fatalf("recovered=%v automatic=%v err=%v", recovered, runner.automatic, err)
	}
	for _, call := range runner.calls {
		if len(call) > 0 && (call[0] == "networksetup" || call[0] == "route") {
			t.Fatalf("recovery without snapshot ran %v", call)
		}
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("managed resolver file was not cleaned up: %v", err)
	}
}

func TestRestoreDNSRejectsUnmanagedResolverDomain(t *testing.T) {
	dir := useTempResolverDir(t)
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	state := DNSState{Service: "Wi-Fi", Automatic: true, Resolvers: []ResolverFileState{
		{Domain: "../escaped", Existed: true, Content: "owned"},
	}}
	if err := writeDNSState(statePath, state); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDNSRunner{}
	if _, err := restoreDNS(statePath, runner); err == nil || !strings.Contains(err.Error(), "非受管 resolver") {
		t.Fatalf("unmanaged resolver domain accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escaped")); !os.IsNotExist(err) {
		t.Fatalf("escaped resolver path was written: %v", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file removed despite refusal: %v", err)
	}
}

func TestIsManagedResolverDomain(t *testing.T) {
	for _, domain := range resolverDomains(testDNSDomains.Private) {
		if !isManagedResolverDomain(domain) {
			t.Fatalf("managed domain rejected: %q", domain)
		}
	}
	for _, domain := range []string{"", "..", "../corp", "corp/x", "/etc/sudoers", "\\corp", ".hidden", "trailing.", "a..b", "Upper.com", "space here"} {
		if isManagedResolverDomain(domain) {
			t.Fatalf("unmanaged domain accepted: %q", domain)
		}
	}
}

func TestRestoreDNSRejectsUntrustedStateFile(t *testing.T) {
	useTempResolverDir(t)
	root := t.TempDir()
	loose := filepath.Join(root, "loose.json")
	if err := os.WriteFile(loose, []byte(`{"service":"Wi-Fi","automatic":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreDNS(loose, &fakeDNSRunner{}); err == nil || !strings.Contains(err.Error(), "权限") {
		t.Fatalf("world-readable state accepted: %v", err)
	}

	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(`{"service":"Wi-Fi","automatic":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreDNS(link, &fakeDNSRunner{}); err == nil || !strings.Contains(err.Error(), "普通文件") {
		t.Fatalf("symlinked state accepted: %v", err)
	}

	missing := filepath.Join(root, "missing.json")
	if restored, err := restoreDNS(missing, &fakeDNSRunner{}); err != nil || restored {
		t.Fatalf("missing state: restored=%v err=%v", restored, err)
	}
}

func TestRecoverDNSCleansResolverFilesFromOldSettings(t *testing.T) {
	dir := useTempResolverDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 内网域名已经从设置里删掉，但上次连接留下的 resolver 文件仍要清理；
	// 用户自己的 resolver 文件不能动。
	stale := filepath.Join(dir, "old.corp.example")
	mine := filepath.Join(dir, "mine.example")
	if err := os.WriteFile(stale, managedResolverContent(1053), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("nameserver 192.0.2.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverDNS(filepath.Join(t.TempDir(), "missing.json"), &fakeDNSRunner{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale managed resolver was not removed: %v", err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("user resolver was removed: %v", err)
	}
}
