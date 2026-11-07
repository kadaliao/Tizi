package claudeblock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRulesPassTunnelsThenBlockEverythingElse(t *testing.T) {
	rules := Rules()
	for _, network := range append(append([]string(nil), Networks...), SelfTestAddress) {
		if !strings.Contains(rules, network) {
			t.Fatalf("rules miss %s:\n%s", network, rules)
		}
	}
	pass := strings.Index(rules, "pass out quick on {")
	block := strings.Index(rules, "block return out quick to <tizi_claude>")
	if pass < 0 || block < 0 || pass > block {
		t.Fatalf("tunnels must be passed before everything else is blocked:\n%s", rules)
	}
	if !strings.Contains(rules, "utun0,") || !strings.Contains(rules, "utun127 }") {
		t.Fatalf("utun0..utun127 must be passed:\n%s", rules)
	}
}

// TestRulesParse 用系统的 pfctl 只做语法检查（-n），不需要管理员权限。
func TestRulesParse(t *testing.T) {
	if _, err := exec.LookPath("pfctl"); err != nil || runtime.GOOS != "darwin" {
		t.Skip("pfctl not available")
	}
	path := filepath.Join(t.TempDir(), "rules.pf")
	if err := os.WriteFile(path, []byte(Rules()), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("pfctl", "-n", "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("pfctl rejected the rules: %v\n%s", err, output)
	}
}

func TestDaemonPlist(t *testing.T) {
	plist := DaemonPlist()
	if strings.Contains(plist, "2>&1 |") || !strings.Contains(plist, "2&gt;&amp;1") {
		t.Fatalf("the shell script must be XML-escaped:\n%s", plist)
	}
	if _, err := exec.LookPath("plutil"); err != nil {
		return
	}
	path := filepath.Join(t.TempDir(), "daemon.plist")
	if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil rejected the plist: %v\n%s", err, output)
	}
	script, err := exec.Command("plutil", "-extract", "ProgramArguments.2", "raw", path).Output()
	if err != nil || strings.TrimSpace(string(script)) != daemonScript() {
		t.Fatalf("plist script = %q, %v", script, err)
	}
}

func fakePrivileged(t *testing.T, fail string) *[]string {
	t.Helper()
	var calls []string
	old := privileged
	privileged = func(name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{filepath.Base(name)}, args...), " ")
		calls = append(calls, call)
		switch {
		case fail != "" && strings.Contains(call, fail):
			return []byte("denied"), errors.New("exit status 1")
		case strings.HasSuffix(call, "-s info"):
			return []byte("Status: Disabled"), nil
		case strings.HasSuffix(call, "-s rules"):
			return []byte("block return out quick from any to <tizi_claude>"), nil
		}
		return nil, nil
	}
	t.Cleanup(func() { privileged = old })
	return &calls
}

func TestInstallAndRemove(t *testing.T) {
	if !Supported() {
		t.Skip("macOS only")
	}
	calls := fakePrivileged(t, "")
	if err := Install(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*calls, "\n")
	for _, want := range []string{
		"install -m 0644 -o root -g wheel",
		"pfctl -a " + Anchor + " -f " + RulesPath,
		"pfctl -E",
		"launchctl bootstrap system " + DaemonPath,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Install must run %q, ran:\n%s", want, got)
		}
	}

	*calls = nil
	if err := Remove(); err != nil {
		t.Fatal(err)
	}
	got = strings.Join(*calls, "\n")
	for _, want := range []string{"launchctl bootout system/" + DaemonLabel, "rm -f " + DaemonPath + " " + RulesPath, "pfctl -a " + Anchor + " -F all"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Remove must run %q, ran:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pfctl -d") {
		t.Fatal("Remove must not turn pf off for other programs")
	}
}

func TestInstallReportsFailures(t *testing.T) {
	if !Supported() {
		t.Skip("macOS only")
	}
	fakePrivileged(t, "pfctl -a")
	if err := Install(); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("Install error = %v", err)
	}
}

func TestPhysicalInterface(t *testing.T) {
	for route, want := range map[string]string{
		"en1 via 192.168.2.1": "en1",
		"eth0 via 10.0.0.1":   "eth0",
		"":                    "",
	} {
		if got := physicalInterface(route); got != want {
			t.Errorf("physicalInterface(%q) = %q, want %q", route, got, want)
		}
	}
}
