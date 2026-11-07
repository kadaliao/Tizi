package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/system"
)

func TestStatusRejectsReusedPIDIdentity(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{
		ConfigDir: dir,
		DataDir:   filepath.Join(dir, "data"),
		Generated: filepath.Join(dir, "config.yaml"),
		PIDFile:   filepath.Join(dir, "mihomo.pid"),
		CoreBin:   filepath.Join(dir, "missing-mihomo"),
	}
	cmd := exec.Command("sh", "-c", "while :; do sleep 1; done", paths.Generated)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(100 * time.Millisecond)
	record := pidRecord{
		PID:        cmd.Process.Pid,
		StartToken: system.ProcessStartToken(cmd.Process.Pid),
		Executable: system.ProcessExecutable(cmd.Process.Pid),
		ConfigFile: paths.Generated,
	}
	if err := writePID(paths.PIDFile, record); err != nil {
		t.Fatal(err)
	}
	manager := Manager{Paths: paths}
	if !manager.Status().Running {
		t.Fatal("expected matching process to be recognized")
	}
	record.StartToken = "stale-start-token"
	if err := writePID(paths.PIDFile, record); err != nil {
		t.Fatal(err)
	}
	if manager.Status().Running {
		t.Fatal("stale process identity must not be recognized")
	}
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	if system.ProcessCommand(cmd.Process.Pid) == "" {
		t.Fatal("stop killed a process with mismatched identity")
	}
	if _, err := os.Stat(paths.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale PID file removal, got %v", err)
	}
}

func TestIsOpenVPNExecutable(t *testing.T) {
	for _, executable := range []string{"/usr/local/sbin/openvpn", "/Applications/Tunnelblick.app/Contents/Resources/openvpn"} {
		if !isOpenVPNExecutable(executable) {
			t.Fatalf("%q should be recognized as OpenVPN", executable)
		}
	}
	if isOpenVPNExecutable("/usr/local/bin/mihomo") {
		t.Fatal("mihomo must not be recognized as OpenVPN")
	}
}

func TestLoggedCoreUsesSudoSupportsLegacyPIDRecords(t *testing.T) {
	if !loggedCoreUsesSudo("/tmp/tizi _run-logged-core /tmp/mihomo /tmp /tmp/config.yaml /tmp/mihomo.log true") {
		t.Fatal("legacy privileged launcher command was not recognized")
	}
	for _, command := range []string{
		"/tmp/tizi _run-logged-core /tmp/mihomo /tmp /tmp/config.yaml /tmp/mihomo.log false",
		"/tmp/other-command true",
	} {
		if loggedCoreUsesSudo(command) {
			t.Fatalf("%q must not be recognized as privileged launcher", command)
		}
	}
}

func TestOpenLogFilePreservesUnreadableRegularFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open mode 000 files")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "mihomo.log")
	if err := os.WriteFile(path, []byte("old log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	file, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("new log mode = %o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	foundBackup := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "mihomo.log.unreadable-") {
			foundBackup = true
		}
	}
	if !foundBackup {
		t.Fatal("unreadable log was not preserved")
	}
}
