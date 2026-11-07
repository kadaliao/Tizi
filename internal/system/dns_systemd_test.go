package system

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeLinuxDNSRunner struct {
	calls       [][]string
	revertFails bool
	ruleOutput  string
}

func (f *fakeLinuxDNSRunner) Output(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	switch {
	case name == "ip" && reflect.DeepEqual(args, []string{"link", "show", "dev", LinuxTUNInterface}):
		return []byte("9: tizi: <POINTOPOINT,UP>"), nil
	case name == "ip" && reflect.DeepEqual(args, []string{"rule", "show"}):
		return []byte(f.ruleOutput), nil
	case name == "ip":
		return []byte("default via 192.168.1.1 dev enp0s3\n"), nil
	case name == "resolvectl":
		return []byte("Link 2 (enp0s3): 192.168.1.1\n"), nil
	default:
		return nil, fmt.Errorf("unexpected command: %s %v", name, args)
	}
}

func (f *fakeLinuxDNSRunner) PrivilegedOutput(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{"sudo", "-n", name}, args...))
	if len(args) > 0 && args[0] == "revert" && f.revertFails {
		return []byte("Link not found"), fmt.Errorf("exit 1")
	}
	return nil, nil
}

func (f *fakeLinuxDNSRunner) Run(name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil
}

func TestPrepareAndRestoreLinuxDNS(t *testing.T) {
	useTempNetFS(t)
	runner := &fakeLinuxDNSRunner{}
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := prepareLinuxDNS(statePath, 1053, DNSDomains{Private: []string{"corp.example.com", "ts.net"}, Search: []string{"corp.example.com"}}, runner); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"platform":"linux"`) || !strings.Contains(string(data), `"interface":"tizi"`) {
		t.Fatalf("unexpected state: %s", data)
	}
	wantCalls := []string{
		"sudo -n resolvectl dns tizi 127.0.0.1:1053",
		"sudo -n resolvectl domain tizi ~. corp.example.com",
		"sudo -n resolvectl default-route tizi true",
	}
	var joined []string
	for _, call := range runner.calls {
		joined = append(joined, strings.Join(call, " "))
	}
	for _, want := range wantCalls {
		if !containsCall(joined, want) {
			t.Fatalf("missing call %q in %v", want, joined)
		}
	}
	restored, err := restoreLinuxDNS(statePath, runner)
	if err != nil || !restored {
		t.Fatalf("restored=%v err=%v", restored, err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state still exists: %v", err)
	}
}

func TestRestoreLinuxDNSAcceptsRemovedInterface(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "dns-state.json")
	if err := os.WriteFile(statePath, []byte(`{"platform":"linux","interface":"tizi","service":"systemd-resolved"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := restoreLinuxDNS(statePath, &fakeLinuxDNSRunner{revertFails: true})
	if err != nil || !restored {
		t.Fatalf("restored=%v err=%v", restored, err)
	}
}

func TestCleanupLinuxTUNPolicy(t *testing.T) {
	runner := &fakeLinuxDNSRunner{ruleOutput: `0: from all lookup local
9000: from all to 198.18.0.0/30 lookup 2022
9001: not from all dport 53 lookup main suppress_prefixlength 0
9001: from all iif tizi goto 9010
9002: not from all iif lo lookup 2022
9002: from 0.0.0.0 iif lo lookup 2022
9002: from 198.18.0.0/30 iif lo lookup 2022
9010: from all nop
32766: from all lookup main
`}
	if err := cleanupLinuxTUNPolicy(runner); err != nil {
		t.Fatal(err)
	}
	var joined []string
	for _, call := range runner.calls {
		joined = append(joined, strings.Join(call, " "))
	}
	for _, want := range []string{
		"sudo -n ip rule delete priority 9000",
		"sudo -n ip rule delete priority 9001",
		"sudo -n ip rule delete priority 9002",
		"sudo -n ip rule delete priority 9010",
		"sudo -n ip route flush table 2022",
	} {
		if !containsCall(joined, want) {
			t.Fatalf("missing call %q in %v", want, joined)
		}
	}
}

func TestCleanupLinuxTUNPolicyRejectsUnknownRule(t *testing.T) {
	runner := &fakeLinuxDNSRunner{ruleOutput: "9001: from all lookup 100\n"}
	if err := cleanupLinuxTUNPolicy(runner); err == nil || !strings.Contains(err.Error(), "拒绝清理未知") {
		t.Fatalf("cleanupLinuxTUNPolicy() error = %v", err)
	}
	for _, call := range runner.calls {
		if len(call) >= 3 && call[0] == "sudo" && call[2] == "ip" {
			t.Fatalf("cleanup changed an unknown rule: %v", call)
		}
	}
}

func containsCall(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
