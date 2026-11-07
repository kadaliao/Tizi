package system

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestVPNStateBelongsToProcessTree(t *testing.T) {
	state := VPNState{parentPIDs: map[int]int{
		20: 10,
		30: 20,
		40: 30,
		50: 10,
		60: 70,
		70: 60,
	}}

	tests := []struct {
		name string
		pid  int
		root int
		want bool
	}{
		{name: "root", pid: 10, root: 10, want: true},
		{name: "direct child", pid: 20, root: 10, want: true},
		{name: "nested descendant", pid: 40, root: 10, want: true},
		{name: "sibling tree", pid: 40, root: 50, want: false},
		{name: "missing process", pid: 99, root: 10, want: false},
		{name: "cycle", pid: 60, root: 10, want: false},
		{name: "invalid root", pid: 40, root: 0, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := state.BelongsToProcessTree(test.pid, test.root); got != test.want {
				t.Fatalf("BelongsToProcessTree(%d, %d) = %v, want %v", test.pid, test.root, got, test.want)
			}
		})
	}
}

func TestParseSSOwner(t *testing.T) {
	output := `LISTEN 0 4096 127.0.0.1:7890 0.0.0.0:* users:(("mihomo",pid=4321,fd=7))`
	got := parseSSOwner(output)
	if got.PID != 4321 || got.Command != "mihomo" {
		t.Fatalf("parseSSOwner() = %#v", got)
	}
}

func TestParseLinuxDefaultRoute(t *testing.T) {
	if got := parseLinuxDefaultRoute("default via 192.168.1.1 dev enp0s3 proto dhcp\n"); got != "enp0s3 via 192.168.1.1" {
		t.Fatalf("parseLinuxDefaultRoute() = %q", got)
	}
}

func TestSupportedUbuntuRelease(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "ubuntu 22.04", data: "ID=ubuntu\nVERSION_ID=\"22.04\"\n", want: true},
		{name: "ubuntu 24.04", data: "ID=\"ubuntu\"\nVERSION_ID=24.04\n", want: true},
		{name: "unsupported ubuntu", data: "ID=ubuntu\nVERSION_ID=\"26.04\"\n"},
		{name: "debian", data: "ID=debian\nVERSION_ID=\"12\"\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "os-release")
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := isSupportedUbuntu(path); got != test.want {
				t.Fatalf("isSupportedUbuntu() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLinuxDNSServerParsing(t *testing.T) {
	got := parseLinuxDNSServers("Link 2 (enp0s3): 192.168.1.1 1.1.1.1\n")
	if !reflect.DeepEqual(got, []string{"192.168.1.1", "1.1.1.1"}) {
		t.Fatalf("parseLinuxDNSServers() = %v", got)
	}
}

func TestAdministratorAuthorizationCommandUsesAskPassOnlyWhenConfigured(t *testing.T) {
	t.Cleanup(func() { UseAskPass("") })

	terminal := AdministratorAuthorizationCommand()
	if !reflect.DeepEqual(terminal.Args, []string{"sudo", "-v"}) || terminal.Env != nil {
		t.Fatalf("终端授权命令 = %v env=%v", terminal.Args, terminal.Env)
	}

	UseAskPass("/Applications/Example.app/Contents/MacOS/askpass")
	dialog := AdministratorAuthorizationCommand()
	if !reflect.DeepEqual(dialog.Args, []string{"sudo", "-A", "-p", AskPassPrompt, "-v"}) {
		t.Fatalf("图形授权命令 = %v", dialog.Args)
	}
	found := false
	for _, entry := range dialog.Env {
		if entry == "SUDO_ASKPASS=/Applications/Example.app/Contents/MacOS/askpass" {
			found = true
		}
	}
	if !found {
		t.Fatal("图形授权命令应通过 SUDO_ASKPASS 指定密码框程序")
	}
	if strings.Contains(AskPassPrompt, "%") {
		t.Fatal("sudo 会展开提示中的 % 转义")
	}
}

func TestParseProcessState(t *testing.T) {
	const token = "Wed Oct 28 00:08:05 2026"
	cases := []struct {
		output string
		want   ProcessState
	}{
		{"Ss   Wed Oct 28 00:08:05 2026\n", ProcessRunning},
		{"Z    Wed Oct 28 00:08:05 2026\n", ProcessGone},
		{"S    Wed Oct 28 09:30:00 2026\n", ProcessGone},
		{"", ProcessUnknown},
		{"S\n", ProcessUnknown},
	}
	for _, c := range cases {
		if got := parseProcessState(c.output, token); got != c.want {
			t.Errorf("parseProcessState(%q) = %v，期望 %v", c.output, got, c.want)
		}
	}
}

func TestProcessStateOfRealProcesses(t *testing.T) {
	self := os.Getpid()
	token := ProcessStartToken(self)
	if token == "" {
		t.Skip("ps 不可用")
	}
	if got := ProcessStateOf(self, token); got != ProcessRunning {
		t.Fatalf("当前进程应判断为运行中，得到 %v", got)
	}
	if got := ProcessStateOf(self, "Thu Jan  1 00:00:00 1970"); got != ProcessGone {
		t.Fatalf("启动时间不同说明 PID 已换成别的进程，得到 %v", got)
	}

	// 子进程退出后、被回收前是僵尸进程：ps 里仍有原来的启动时间，但已经退出。
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	childToken := ProcessStartToken(pid)
	deadline := time.Now().Add(5 * time.Second)
	for ProcessStateOf(pid, childToken) != ProcessGone && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := ProcessStateOf(pid, childToken); got != ProcessGone {
		t.Fatalf("未回收的僵尸进程应判断为已退出，得到 %v", got)
	}
	_ = cmd.Wait()
	if got := ProcessStateOf(pid, childToken); got != ProcessGone {
		t.Fatalf("回收后的进程应判断为已退出，得到 %v", got)
	}
}
