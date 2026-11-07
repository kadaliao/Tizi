package system

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseNetworkServices(t *testing.T) {
	output := `An asterisk (*) denotes that a network service is disabled.
(1) USB Controls
(Hardware Port: USB Controls, Device: usbmodemABC)

(2) Wi-Fi
(Hardware Port: Wi-Fi, Device: en1)

(3) *Old Dock
(Hardware Port: Old Dock, Device: en5)

(*) Gone
(Hardware Port: Gone, Device: en6)

(4) Tailscale
(Hardware Port: io.tailscale.ipn.macsys, Device: )
`
	got := parseNetworkServices(output)
	want := []networkService{
		{Name: "USB Controls", Device: "usbmodemABC"},
		{Name: "Wi-Fi", Device: "en1"},
		{Name: "Old Dock", Device: "en5", Disabled: true},
		{Name: "Gone", Device: "en6", Disabled: true},
		{Name: "Tailscale"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("services = %#v, want %#v", got, want)
	}
}

func TestParseIPv6Info(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   IPv6ServiceState
	}{
		{
			name:   "automatic",
			output: "Manually Using DHCP Router Configuration\nIP address: 192.168.2.205\nRouter: 192.168.2.1\nIPv6: Automatic\nIPv6 IP address: none\nIPv6 Router: none\nWi-Fi ID: 5c:1b:f4:96:93:49\n",
			want:   IPv6ServiceState{Mode: ipv6ModeAutomatic},
		},
		{
			name:   "manual",
			output: "IPv6: Manual\nIPv6 IP address: 2001:db8::2\nIPv6 Prefix Length: 64\nIPv6 Router: 2001:db8::1\n",
			want:   IPv6ServiceState{Mode: ipv6ModeManual, Address: "2001:db8::2", PrefixLength: 64, Router: "2001:db8::1"},
		},
		// 真机上（macOS 26）「仅本地链路」的完整输出，没有 IPv6: 这一行。
		{
			name:   "link-local as printed by macOS",
			output: "Manually Using DHCP Router Configuration\nIP address: 192.168.2.205\nSubnet mask: 255.255.255.0\nRouter: 192.168.2.1\nIPv6 IP address: none\nIPv6 Router: none\nWi-Fi ID: 5c:1b:f4:96:93:49\n",
			want:   IPv6ServiceState{Mode: ipv6ModeRestricted},
		},
		{name: "link-local spelled out", output: "IPv6: Link-local\n", want: IPv6ServiceState{Mode: ipv6ModeLinkLocal}},
		{name: "off spelled out", output: "IPv6: Off\n", want: IPv6ServiceState{Mode: ipv6ModeOff}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseIPv6Info(test.output)
			if err != nil || got != test.want {
				t.Fatalf("parseIPv6Info() = %#v, %v; want %#v", got, err, test.want)
			}
		})
	}
	for _, output := range []string{"", "IP address: 192.168.2.2\nRouter: 192.168.2.1\n", "IPv6: Whatever\n"} {
		if _, err := parseIPv6Info(output); err == nil {
			t.Fatalf("parseIPv6Info(%q) accepted unusable output", output)
		}
	}
}

func v6Calls(runner *fakeDNSRunner) []string {
	var calls []string
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "-setv6") {
			calls = append(calls, joined)
		}
	}
	return calls
}

func TestPrepareLimitsPhysicalServicesAndRestoreReturnsThem(t *testing.T) {
	useTempResolverDir(t)
	runner := &fakeDNSRunner{automatic: true}
	runner.setV6(IPv6ServiceState{Service: "Ethernet", Mode: ipv6ModeManual, Address: "2001:db8::2", PrefixLength: 64, Router: "2001:db8::1"})
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareDNS(statePath, 1053, testDNSDomains, runner); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"Ethernet", "Wi-Fi"} {
		if got := runner.v6State(service).Mode; got != ipv6ModeLinkLocal {
			t.Fatalf("%s IPv6 mode = %q after prepare, want link-local", service, got)
		}
	}
	if got := runner.v6State("Tailscale").Mode; got != ipv6ModeAutomatic {
		t.Fatalf("non-hardware service was touched: %q", got)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state DNSState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	wantState := []IPv6ServiceState{
		{Service: "Ethernet", Mode: ipv6ModeManual, Address: "2001:db8::2", PrefixLength: 64, Router: "2001:db8::1"},
		{Service: "Wi-Fi", Mode: ipv6ModeAutomatic},
	}
	if !reflect.DeepEqual(state.IPv6, wantState) {
		t.Fatalf("snapshot = %#v, want %#v", state.IPv6, wantState)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "-setv6") && (call[0] != "sudo" || call[1] != "-n") {
			t.Fatalf("IPv6 change was not privileged: %v", call)
		}
	}

	restored, err := restoreDNS(statePath, runner)
	if err != nil || !restored {
		t.Fatalf("restored=%v err=%v", restored, err)
	}
	if got := runner.v6State("Ethernet"); got != wantState[0] {
		t.Fatalf("Ethernet = %#v, want %#v", got, wantState[0])
	}
	if got := runner.v6State("Wi-Fi"); got.Mode != ipv6ModeAutomatic {
		t.Fatalf("Wi-Fi = %#v, want automatic", got)
	}
}

func TestPrepareLeavesAlreadyRestrictedServicesAlone(t *testing.T) {
	useTempResolverDir(t)
	runner := &fakeDNSRunner{automatic: true}
	runner.setV6(IPv6ServiceState{Service: "Ethernet", Mode: ipv6ModeOff})
	runner.setV6(IPv6ServiceState{Service: "Wi-Fi", Mode: ipv6ModeLinkLocal})
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareDNS(statePath, 1053, testDNSDomains, runner); err != nil {
		t.Fatal(err)
	}
	if calls := v6Calls(runner); len(calls) != 0 {
		t.Fatalf("IPv6 was changed on services that needed nothing: %v", calls)
	}
	if _, err := restoreDNS(statePath, runner); err != nil {
		t.Fatal(err)
	}
	if calls := v6Calls(runner); len(calls) != 0 {
		t.Fatalf("restore changed IPv6 it never touched: %v", calls)
	}
	if runner.v6State("Ethernet").Mode != ipv6ModeOff || runner.v6State("Wi-Fi").Mode != ipv6ModeLinkLocal {
		t.Fatal("a user's IPv6 choice was overwritten")
	}
}

func TestPrepareRollsBackIPv6WhenLimitFails(t *testing.T) {
	resolverDir := useTempResolverDir(t)
	runner := &fakeDNSRunner{automatic: true, failV6LimitOn: "Wi-Fi"}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	err := prepareDNS(statePath, 1053, testDNSDomains, runner)
	if err == nil || !strings.Contains(err.Error(), "injected v6 failure") {
		t.Fatalf("expected injected IPv6 error, got %v", err)
	}
	for _, service := range []string{"Ethernet", "Wi-Fi"} {
		if got := runner.v6State(service).Mode; got != ipv6ModeAutomatic {
			t.Fatalf("%s IPv6 mode = %q after rollback, want automatic", service, got)
		}
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rollback left DNS state behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(resolverDir, "ts.net")); !os.IsNotExist(err) {
		t.Fatalf("rollback left resolver behind: %v", err)
	}
}

func TestPrepareFailsWhenDefaultServiceIPv6IsUnreadable(t *testing.T) {
	useTempResolverDir(t)
	runner := &fakeDNSRunner{automatic: true, failInfoOn: "Wi-Fi"}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareDNS(statePath, 1053, testDNSDomains, runner); err == nil || !strings.Contains(err.Error(), "Wi-Fi IPv6") {
		t.Fatalf("expected default-service IPv6 error, got %v", err)
	}
	if calls := v6Calls(runner); len(calls) != 0 {
		t.Fatalf("IPv6 changed despite failed snapshot: %v", calls)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("failed prepare left DNS state behind: %v", err)
	}
}

func TestPrepareSkipsUnreadableNonDefaultService(t *testing.T) {
	useTempResolverDir(t)
	runner := &fakeDNSRunner{automatic: true, failInfoOn: "Ethernet"}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareDNS(statePath, 1053, testDNSDomains, runner); err != nil {
		t.Fatal(err)
	}
	if got := runner.v6State("Wi-Fi").Mode; got != ipv6ModeLinkLocal {
		t.Fatalf("Wi-Fi IPv6 mode = %q, want link-local", got)
	}
}

func TestPrepareRejectsIncompleteManualIPv6OnDefaultService(t *testing.T) {
	useTempResolverDir(t)
	runner := &fakeDNSRunner{automatic: true}
	runner.setV6(IPv6ServiceState{Service: "Wi-Fi", Mode: ipv6ModeManual, Address: "2001:db8::2"})
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareDNS(statePath, 1053, testDNSDomains, runner); err == nil || !strings.Contains(err.Error(), "手动 IPv6") {
		t.Fatalf("expected manual IPv6 error, got %v", err)
	}
	if calls := v6Calls(runner); len(calls) != 0 {
		t.Fatalf("IPv6 changed despite unrestorable config: %v", calls)
	}
}

func TestRestoreIPv6ValidatesUntrustedState(t *testing.T) {
	useTempResolverDir(t)
	tests := []struct {
		name  string
		state IPv6ServiceState
		want  string
	}{
		{name: "unknown mode", state: IPv6ServiceState{Service: "Wi-Fi", Mode: "off"}, want: "未知模式"},
		{name: "bad address", state: IPv6ServiceState{Service: "Wi-Fi", Mode: ipv6ModeManual, Address: "; rm -rf /", PrefixLength: 64, Router: "2001:db8::1"}, want: "手动配置无效"},
		{name: "bad prefix", state: IPv6ServiceState{Service: "Wi-Fi", Mode: ipv6ModeManual, Address: "2001:db8::2", PrefixLength: 500, Router: "2001:db8::1"}, want: "手动配置无效"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeDNSRunner{}
			statePath := filepath.Join(t.TempDir(), "dns-state.json")
			if err := writeDNSState(statePath, DNSState{Service: "Wi-Fi", Automatic: true, IPv6: []IPv6ServiceState{test.state}}); err != nil {
				t.Fatal(err)
			}
			if _, err := restoreDNS(statePath, runner); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("restoreDNS() error = %v, want %q", err, test.want)
			}
			if calls := v6Calls(runner); len(calls) != 0 {
				t.Fatalf("invalid state reached networksetup: %v", calls)
			}
			if _, err := os.Stat(statePath); err != nil {
				t.Fatalf("state removed despite refusal: %v", err)
			}
		})
	}
}

func TestRestoreIPv6IgnoresServicesThatNoLongerExist(t *testing.T) {
	useTempResolverDir(t)
	runner := &fakeDNSRunner{}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	state := DNSState{Service: "Wi-Fi", Automatic: true, IPv6: []IPv6ServiceState{
		{Service: "Unplugged Dock", Mode: ipv6ModeAutomatic},
		{Service: "Tailscale", Mode: ipv6ModeAutomatic},
		{Service: "Wi-Fi", Mode: ipv6ModeAutomatic},
	}}
	if err := writeDNSState(statePath, state); err != nil {
		t.Fatal(err)
	}
	if restored, err := restoreDNS(statePath, runner); err != nil || !restored {
		t.Fatalf("restored=%v err=%v", restored, err)
	}
	want := []string{"sudo -n networksetup -setv6automatic Wi-Fi"}
	if got := v6Calls(runner); !reflect.DeepEqual(got, want) {
		t.Fatalf("IPv6 calls = %v, want %v", got, want)
	}
}

func TestRestoreLegacyStateDoesNotTouchIPv6(t *testing.T) {
	useTempResolverDir(t)
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := os.WriteFile(statePath, []byte(`{"service":"Wi-Fi","automatic":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDNSRunner{}
	if _, err := restoreDNS(statePath, runner); err != nil {
		t.Fatal(err)
	}
	if calls := v6Calls(runner); len(calls) != 0 {
		t.Fatalf("legacy state changed IPv6: %v", calls)
	}
}

// useTempNetFS 把 Linux 的 /sys 与 /proc 探测指向临时目录，避免测试读到
// 开发机或 CI 上真实的网卡。
func useTempNetFS(t *testing.T) (sys, proc string) {
	t.Helper()
	oldSys, oldProc := sysClassNet, procIPv6Dir
	sys, proc = filepath.Join(t.TempDir(), "net"), filepath.Join(t.TempDir(), "ipv6")
	sysClassNet, procIPv6Dir = sys, proc
	t.Cleanup(func() { sysClassNet, procIPv6Dir = oldSys, oldProc })
	return sys, proc
}

func addFakeInterface(t *testing.T, sys, proc, name string, physical bool, disableIPv6 string) {
	t.Helper()
	dir := filepath.Join(sys, name)
	if physical {
		dir = filepath.Join(dir, "device")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proc, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, name, "disable_ipv6"), []byte(disableIPv6+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotLinuxIPv6OnlyTakesEnabledPhysicalInterfaces(t *testing.T) {
	sys, proc := useTempNetFS(t)
	addFakeInterface(t, sys, proc, "enp0s3", true, "0")
	addFakeInterface(t, sys, proc, "wlp2s0", true, "1")
	addFakeInterface(t, sys, proc, "docker0", false, "0")
	addFakeInterface(t, sys, proc, "lo", false, "0")
	addFakeInterface(t, sys, proc, LinuxTUNInterface, false, "0")

	want := []IPv6ServiceState{{Service: "enp0s3", Mode: ipv6ModeEnabled}}
	if got := snapshotLinuxIPv6(); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want %#v", got, want)
	}
}

func TestLinuxPrepareDisablesIPv6AndRestoreEnablesIt(t *testing.T) {
	sys, proc := useTempNetFS(t)
	addFakeInterface(t, sys, proc, "enp0s3", true, "0")
	runner := &fakeLinuxDNSRunner{}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareLinuxDNS(statePath, 1053, DNSDomains{}, runner); err != nil {
		t.Fatal(err)
	}
	var joined []string
	for _, call := range runner.calls {
		joined = append(joined, strings.Join(call, " "))
	}
	if !containsCall(joined, "sudo -n sysctl -w net.ipv6.conf.enp0s3.disable_ipv6=1") {
		t.Fatalf("IPv6 was not disabled: %v", joined)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ipv6":[{"service":"enp0s3","mode":"enabled"}]`) {
		t.Fatalf("snapshot missing IPv6 interfaces: %s", data)
	}

	runner.calls = nil
	if restored, err := restoreLinuxDNS(statePath, runner); err != nil || !restored {
		t.Fatalf("restored=%v err=%v", restored, err)
	}
	joined = nil
	for _, call := range runner.calls {
		joined = append(joined, strings.Join(call, " "))
	}
	if !containsCall(joined, "sudo -n sysctl -w net.ipv6.conf.enp0s3.disable_ipv6=0") {
		t.Fatalf("IPv6 was not re-enabled: %v", joined)
	}
}

func TestRestoreLinuxIPv6ValidatesUntrustedState(t *testing.T) {
	sys, _ := useTempNetFS(t)
	if err := os.MkdirAll(filepath.Join(sys, "gone0"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, state := range []IPv6ServiceState{
		{Service: "../../etc", Mode: ipv6ModeEnabled},
		{Service: "eth0;reboot", Mode: ipv6ModeEnabled},
		{Service: "gone0", Mode: ipv6ModeAutomatic},
	} {
		runner := &fakeLinuxDNSRunner{}
		if err := restoreLinuxIPv6(runner, []IPv6ServiceState{state}); err == nil {
			t.Fatalf("restoreLinuxIPv6(%#v) accepted untrusted state", state)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("untrusted state reached sysctl: %v", runner.calls)
		}
	}

	runner := &fakeLinuxDNSRunner{}
	if err := restoreLinuxIPv6(runner, []IPv6ServiceState{{Service: "unplugged0", Mode: ipv6ModeEnabled}}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("restore touched an interface that no longer exists: %v", runner.calls)
	}
}
