package manager

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
)

// fakeMihomo 模拟启动过程：先连不上 Controller，再是配置没加载完，然后
// 「Claude」组出现并选中第一项，随后 Mihomo 从 cache.db 恢复上次的选择。
type fakeMihomo struct {
	mu       sync.Mutex
	calls    int
	choices  []string
	now      string
	restored string
	selects  []string
}

func (f *fakeMihomo) Groups() ([]controller.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	switch {
	case f.calls <= 2:
		return nil, errors.New("connection refused")
	case f.calls == 3:
		return []controller.Group{{Name: "GLOBAL", Now: "DIRECT", Choices: []string{"DIRECT"}}}, nil
	case f.calls == 5 && f.restored != "":
		f.now = f.restored
	}
	return []controller.Group{{Name: config.ClaudeGroup, Now: f.now, Choices: f.choices}}, nil
}

func (f *fakeMihomo) Select(group, choice string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selects = append(f.selects, group+"="+choice)
	f.now = choice
	return nil
}

func TestPinClaudeResidentialOverridesRestoredSelection(t *testing.T) {
	fake := &fakeMihomo{
		choices:  []string{config.ResidentialProxyName, "hk-node"},
		now:      config.ResidentialProxyName,
		restored: "hk-node",
	}
	var log bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		pinClaudeResidential(make(chan struct{}), fake, &log, 5*time.Second, 100*time.Millisecond, 5*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pin did not finish after its window")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.now != config.ResidentialProxyName || len(fake.selects) != 1 {
		t.Fatalf("now = %q, selects = %v", fake.now, fake.selects)
	}
	if !strings.Contains(log.String(), "hk-node") {
		t.Fatalf("log should mention the restored choice: %q", log.String())
	}
}

func TestPinClaudeResidentialSkipsWithoutResidentialNode(t *testing.T) {
	fake := &fakeMihomo{choices: []string{"hk-node"}, now: "hk-node"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pinClaudeResidential(make(chan struct{}), fake, &bytes.Buffer{}, 5*time.Second, time.Minute, 5*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pin should stop as soon as it sees no residential node")
	}
	if len(fake.selects) != 0 {
		t.Fatalf("selects = %v", fake.selects)
	}
}

func TestPinClaudeResidentialStops(t *testing.T) {
	fake := &fakeMihomo{choices: []string{config.ResidentialProxyName}, now: config.ResidentialProxyName}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		pinClaudeResidential(stop, fake, &bytes.Buffer{}, time.Minute, time.Minute, 5*time.Millisecond)
	}()
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pin did not stop")
	}
}
