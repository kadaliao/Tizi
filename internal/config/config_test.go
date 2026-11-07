package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoadSettings(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{
		ConfigDir: dir,
		DataDir:   filepath.Join(dir, "data"),
		Settings:  filepath.Join(dir, "settings.json"),
		CoreBin:   filepath.Join(dir, "data", "bin", "mihomo"),
	}
	settings := DefaultSettings()
	settings.OVPNPath = "/tmp/example.ovpn"
	settings.AllowLAN = true
	settings.ChinaDirect = false
	settings.CoreChannel = CoreChannelAlpha
	if err := Save(paths, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OVPNPath != settings.OVPNPath || loaded.MixedPort != 7890 || !loaded.AllowLAN || loaded.ChinaDirect || loaded.CoreChannel != CoreChannelAlpha {
		t.Fatalf("unexpected settings: %#v", loaded)
	}
	info, err := os.Stat(paths.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected settings permissions: %o", info.Mode().Perm())
	}
}

func TestResolvePathsIncludesCustomRules(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	paths, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configRoot, appName, "custom-rules.txt")
	if paths.CustomRules != want {
		t.Fatalf("CustomRules = %q, want %q", paths.CustomRules, want)
	}
}

func TestLoadSettingsDefaultsInvalidCoreChannelToStable(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{Settings: filepath.Join(dir, "settings.json")}
	if err := os.WriteFile(paths.Settings, []byte(`{"core_channel":"nightly"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if settings.CoreChannel != CoreChannelStable {
		t.Fatalf("CoreChannel = %q, want %q", settings.CoreChannel, CoreChannelStable)
	}
}

func TestWriteFileAtomicReplacesContentAndCleansTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("content = %q, err=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, err=%v", info.Mode(), err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
	if err := WriteFileAtomic(filepath.Join(t.TempDir(), "missing", "x"), []byte("x"), 0o600); err == nil {
		t.Fatal("expected error for missing directory")
	}
}

func TestValidateProxyCredentials(t *testing.T) {
	for _, test := range []struct {
		username, password string
		ok                 bool
	}{
		{"", "", true},
		{"alice", "s3cret", true},
		{"alice", "pass:with@marks", true},
		{"alice", "", false},
		{"", "s3cret", false},
		{"ali:ce", "s3cret", false},
		{"alice", "has space", false},
		{"ali\tce", "s3cret", false},
	} {
		err := ValidateProxyCredentials(test.username, test.password)
		if (err == nil) != test.ok {
			t.Errorf("ValidateProxyCredentials(%q, %q) = %v, want ok=%v", test.username, test.password, err, test.ok)
		}
	}
}
