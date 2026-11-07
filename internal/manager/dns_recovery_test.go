package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kadaliao/tizi/internal/system"
)

func writeTestPIDRecord(t *testing.T, pid int, startToken string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mihomo.pid")
	data, err := json.Marshal(pidRecord{PID: pid, StartToken: startToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSuccessorRunningOnlyForAnotherLiveSession(t *testing.T) {
	self := os.Getpid()
	token := system.ProcessStartToken(self)
	if token == "" {
		t.Skip("cannot read own process start token")
	}
	if !successorRunning(writeTestPIDRecord(t, self, token), self+1) {
		t.Error("a live launcher other than the exiting one owns the DNS state")
	}
	if successorRunning(writeTestPIDRecord(t, self, token), self) {
		t.Error("the exiting launcher's own record is not a successor")
	}
	if successorRunning(writeTestPIDRecord(t, self, "stale-token"), self+1) {
		t.Error("a recycled PID with a different start token is not a live session")
	}
	if successorRunning(filepath.Join(t.TempDir(), "missing.pid"), self) {
		t.Error("no PID file means no successor")
	}
}

func TestWaitStartLockWaitsForStartToFinish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mihomo.pid.lock")
	held, err := acquireStartLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitStartLock(path, 150*time.Millisecond); err == nil {
		t.Fatal("recovery must not run while Start holds the lock")
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		releaseStartLock(held)
	}()
	lock, err := waitStartLock(path, 5*time.Second)
	if err != nil {
		t.Fatalf("recovery should proceed once Start releases the lock: %v", err)
	}
	releaseStartLock(lock)
}

func TestWaitLauncherExitIgnoresFailedChecks(t *testing.T) {
	// ps 失败（Unknown）不能当成启动器已退出，否则 VPN 还在运行时就会恢复 DNS。
	sequence := []system.ProcessState{system.ProcessRunning, system.ProcessUnknown, system.ProcessUnknown, system.ProcessRunning, system.ProcessGone}
	calls := 0
	waitLauncherExit(42, "token", func(pid int, token string) system.ProcessState {
		if pid != 42 || token != "token" {
			t.Fatalf("参数不对: %d %q", pid, token)
		}
		state := sequence[calls]
		calls++
		return state
	}, time.Millisecond)
	if calls != len(sequence) {
		t.Fatalf("应等到确认退出才返回，只检查了 %d 次", calls)
	}
}
