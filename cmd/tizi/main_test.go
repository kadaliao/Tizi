package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kadaliao/tizi/internal/config"
)

func TestRunRecoverWeChatPrintsClosedCount(t *testing.T) {
	var output bytes.Buffer
	err := runRecoverWeChat(func() (int, error) { return 3, nil }, &output)
	if err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "已刷新 3 条微信直连，Mihomo 保持运行\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestRunRecoverWeChatReportsPartialCountOnError(t *testing.T) {
	err := runRecoverWeChat(func() (int, error) { return 1, errors.New("controller unavailable") }, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "已刷新 1 条") {
		t.Fatalf("error = %v", err)
	}
}

func TestRecoverWeChatCommandRejectsArgumentsBeforeControllerCall(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	err := run([]string{"recover-wechat", "unexpected"})
	if err == nil || !strings.Contains(err.Error(), "不接受参数") {
		t.Fatalf("error = %v", err)
	}
}

func TestHelpFlagOnSubcommandIsNotAnError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, args := range [][]string{{"start", "-h"}, {"generate", "--help"}, {"install-core", "-h"}, {"tracescope", "-h"}} {
		if err := run(args); err != nil {
			t.Fatalf("run(%v) = %v, want nil", args, err)
		}
	}
}

func TestVersionDoesNotNeedSettingsFile(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := filepath.Join(configHome, "tizi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--version"}, {"version"}, {"-v"}, {"--help"}, {"help"}} {
		if err := run(args); err != nil {
			t.Fatalf("run(%v) = %v, want nil", args, err)
		}
	}
}

func TestTUIRejectsPositionalArguments(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	err := run([]string{"tui", "extra"})
	if err == nil || !strings.Contains(err.Error(), "不接受参数") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseConnectionFlagsNormalizesOVPNPath(t *testing.T) {
	settings := config.DefaultSettings()
	updated, _, err := parseConnectionFlags("start", []string{"--ovpn", "profiles/corp.ovpn"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "profiles", "corp.ovpn"); updated.OVPNPath != want {
		t.Fatalf("OVPNPath = %q, want %q", updated.OVPNPath, want)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err = parseConnectionFlags("start", []string{"--ovpn", "~/corp.ovpn"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "corp.ovpn"); updated.OVPNPath != want {
		t.Fatalf("OVPNPath = %q, want %q", updated.OVPNPath, want)
	}

	updated, _, err = parseConnectionFlags("start", []string{"--ovpn", "  "}, settings)
	if err != nil {
		t.Fatal(err)
	}
	if updated.OVPNPath != "" {
		t.Fatalf("blank OVPNPath should stay empty, got %q", updated.OVPNPath)
	}
}

func TestParseConnectionFlagsProxyAuth(t *testing.T) {
	settings := config.DefaultSettings()
	if _, _, err := parseConnectionFlags("start", []string{"--allow-lan", "--proxy-auth"}, settings); err == nil {
		t.Fatal("--proxy-auth without a username should be rejected")
	}
	updated, _, err := parseConnectionFlags("start", []string{"--allow-lan", "--proxy-user", "alice", "--proxy-auth"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.AllowLAN || updated.ProxyUsername != "alice" || len(updated.ProxyPassword) != 32 {
		t.Fatalf("--proxy-auth must keep the username and generate a password: %+v", updated)
	}
	again, _, err := parseConnectionFlags("reload", []string{"--proxy-auth"}, updated)
	if err != nil {
		t.Fatal(err)
	}
	if again.ProxyUsername != "alice" || again.ProxyPassword != updated.ProxyPassword {
		t.Fatal("--proxy-auth must keep the existing credentials")
	}
	renamed, _, err := parseConnectionFlags("reload", []string{"--proxy-user", "bob"}, updated)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ProxyUsername != "bob" || renamed.ProxyPassword != updated.ProxyPassword {
		t.Fatalf("--proxy-user must only rename: %+v", renamed)
	}
	cleared, _, err := parseConnectionFlags("reload", []string{"--no-proxy-auth"}, updated)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ProxyUsername != "" || cleared.ProxyPassword != "" {
		t.Fatalf("--no-proxy-auth must clear the credentials: %+v", cleared)
	}
	if _, _, err := parseConnectionFlags("reload", []string{"--proxy-user", "a:b"}, updated); err == nil {
		t.Fatal("a username with a colon should be rejected")
	}
	if _, _, err := parseConnectionFlags("start", []string{"--proxy-auth", "--no-proxy-auth"}, settings); err == nil {
		t.Fatal("--proxy-auth with --no-proxy-auth should be rejected")
	}
}

func TestParseConnectionFlagsPrecedence(t *testing.T) {
	settings := config.DefaultSettings()
	settings.AllowLAN = true
	settings.ChinaDirect = true
	updated, force, err := parseConnectionFlags("start", []string{"--allow-lan", "--no-allow-lan", "--proxy-china", "--no-tun", "--force-vpn-conflict"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AllowLAN || updated.ChinaDirect || updated.TunEnabled || !force {
		t.Fatalf("unexpected settings: %+v force=%v", updated, force)
	}
	if _, _, err := parseConnectionFlags("start", []string{"--tun", "--no-tun"}, settings); err == nil {
		t.Fatal("--tun with --no-tun should be rejected")
	}
	if _, _, err := parseConnectionFlags("start", []string{"extra"}, settings); err == nil {
		t.Fatal("positional arguments should be rejected")
	}
}

// 更新后的重载必须由新二进制执行 reload，旧进程只负责调用。
func TestReloadWithBinaryRunsReloadOnGivenExecutable(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	binary := filepath.Join(dir, "fake-tizi")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > '" + record + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reloadWithBinary(binary); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "reload" {
		t.Fatalf("new binary args = %q, want reload", got)
	}

	failing := filepath.Join(dir, "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reloadWithBinary(failing); err == nil {
		t.Fatal("a failed reload in the new binary must be reported")
	}
}
