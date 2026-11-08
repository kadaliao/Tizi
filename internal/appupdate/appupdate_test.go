package appupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		latest  string
		current string
		want    bool
	}{
		{latest: "v0.1.13", current: "v0.1.12", want: true},
		{latest: "v0.2.0", current: "v0.1.99", want: true},
		{latest: "v1.0.0", current: "v0.99.99", want: true},
		{latest: "v0.1.12", current: "v0.1.12", want: false},
		{latest: "v0.1.11", current: "v0.1.12", want: false},
		{latest: "v0.1.12", current: "dev", want: true},
		{latest: "invalid", current: "v0.1.12", want: false},
		{latest: "v0.2.5", current: "v0.2.4-1-g171676c-dirty", want: true},
		{latest: "v0.2.5", current: "v0.2.5-dirty", want: false},
		{latest: "v0.2.5", current: "v0.2.5-3-gabc1234", want: false},
		{latest: "v0.2.5", current: "v0.2.6-rc1", want: false},
		{latest: "v0.2.33", current: "v0.2.30+dns-privacy", want: true},
		{latest: "v0.2.30", current: "v0.2.30+dns-privacy", want: false},
	}
	for _, test := range tests {
		if got := isNewer(test.latest, test.current); got != test.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", test.latest, test.current, got, test.want)
		}
	}
}

func TestCheckLatestRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("missing GitHub API Accept header")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"tag_name":"v0.1.13"}`))
	}))
	defer server.Close()

	status, err := check(context.Background(), server.Client(), server.URL, "v0.1.12")
	if err != nil {
		t.Fatal(err)
	}
	if status.Current != "v0.1.12" || status.Latest != "v0.1.13" || !status.Available {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestCheckRejectsInvalidReleaseTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"tag_name":"main"}`))
	}))
	defer server.Close()

	if _, err := check(context.Background(), server.Client(), server.URL, "v0.1.12"); err == nil {
		t.Fatal("expected invalid release tag error")
	}
}

func TestInstallRefusesHomebrewKeg(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		t.Errorf("unexpected request %s", request.URL)
	}))
	defer server.Close()
	for _, executable := range []string{
		"/opt/homebrew/Cellar/tizi/1.2.0/bin/tizi",
		"/usr/local/Cellar/tizi/1.2.0/bin/tizi",
		"/home/linuxbrew/.linuxbrew/Cellar/tizi/1.2.0/bin/tizi",
	} {
		_, err := install(context.Background(), "v1.3.0", server.URL, executable, "", server.Client())
		if !errors.Is(err, ErrHomebrew) {
			t.Fatalf("install(%q) error = %v, want ErrHomebrew", executable, err)
		}
	}
	if homebrewManaged("/Users/me/.local/bin/tizi") {
		t.Fatal("~/.local/bin/tizi is not managed by Homebrew")
	}
}

func TestInstallVerifiesReplacementAndRollsBackFailure(t *testing.T) {
	tests := []struct {
		name            string
		reportedVersion string
		corruptDigest   bool
		wantError       bool
		wantInstalled   string
	}{
		{name: "verified update", reportedVersion: "v0.1.13", wantInstalled: "v0.1.13"},
		{name: "version mismatch rolls back", reportedVersion: "v9.9.9", wantError: true, wantInstalled: "v0.1.12"},
		{name: "checksum mismatch keeps old version", reportedVersion: "v0.1.13", corruptDigest: true, wantError: true, wantInstalled: "v0.1.12"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "tizi")
			writeVersionExecutable(t, executable, "v0.1.12")
			archive := archiveForVersion(t, test.reportedVersion)
			digest := sha256.Sum256(archive)
			digestText := hex.EncodeToString(digest[:])
			if test.corruptDigest {
				digestText = strings.Repeat("0", 64)
			}
			assetName := fmt.Sprintf("tizi-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)

			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/release":
					_, _ = fmt.Fprintf(writer, `{"tag_name":"v0.1.13","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`, assetName, server.URL+"/archive", server.URL+"/checksums")
				case "/archive":
					_, _ = writer.Write(archive)
				case "/checksums":
					_, _ = fmt.Fprintf(writer, "%s  %s\n", digestText, assetName)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			_, err := install(context.Background(), "v0.1.13", server.URL+"/release", executable, "", server.Client())
			if (err != nil) != test.wantError {
				t.Fatalf("install error = %v, wantError %v", err, test.wantError)
			}
			output, commandErr := exec.Command(executable, "version").CombinedOutput()
			if commandErr != nil {
				t.Fatalf("run installed executable: %v: %s", commandErr, output)
			}
			if !strings.Contains(string(output), test.wantInstalled) {
				t.Fatalf("installed version = %q, want %s", strings.TrimSpace(string(output)), test.wantInstalled)
			}
			backups, globErr := filepath.Glob(filepath.Join(dir, ".tizi.backup-*"))
			if globErr != nil || len(backups) != 0 {
				t.Fatalf("backup cleanup = %v, err %v", backups, globErr)
			}
		})
	}
}

func TestExtractBinaryRejectsUnexpectedEntry(t *testing.T) {
	archive := tarGzip(t, map[string]string{"../tizi": "bad"})
	if _, err := extractBinary(archive); err == nil {
		t.Fatal("expected unsafe archive entry error")
	}
}

func writeVersionExecutable(t *testing.T, path, version string) {
	t.Helper()
	content := fmt.Sprintf("#!/bin/sh\necho tizi %s\n", version)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func archiveForVersion(t *testing.T, version string) []byte {
	t.Helper()
	return tarGzip(t, map[string]string{
		"tizi": fmt.Sprintf("#!/bin/sh\necho tizi %s\n", version),
	})
}

func tarGzip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var result bytes.Buffer
	gz := gzip.NewWriter(&result)
	tarWriter := tar.NewWriter(gz)
	for name, content := range files {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func TestResolveExecutableFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real", "tizi")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", "tizi")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got, err := resolveExecutable(link)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if got != want {
		t.Fatalf("resolveExecutable(%q) = %q, want %q", link, got, want)
	}
	if _, err := resolveExecutable(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing executable should fail")
	}
}

func TestFetchReleaseIncludesRateLimitDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-RateLimit-Remaining", "0")
		writer.Header().Set("X-RateLimit-Reset", "1700000000")
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	_, err := fetchRelease(context.Background(), server.Client(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "剩余 0") || !strings.Contains(err.Error(), "重置") {
		t.Fatalf("expected rate-limit detail, got %v", err)
	}
}

// TestInstallUpdatesDesktopApp 覆盖桌面版随命令行一起更新：成功时整包替换；
// 桌面版校验失败只记录错误，命令行照常更新、原桌面版保持不变；Release 没有
// 桌面版安装包时跳过。
func TestInstallUpdatesDesktopApp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("桌面版只在 macOS 上更新")
	}
	tests := []struct {
		name          string
		withAsset     bool
		corruptDigest bool
		wantMarker    string
		wantUpdated   bool
		wantError     bool
	}{
		{name: "同步更新", withAsset: true, wantMarker: "new", wantUpdated: true},
		{name: "校验失败保留旧版", withAsset: true, corruptDigest: true, wantMarker: "old", wantError: true},
		{name: "旧 Release 没有桌面版", wantMarker: "old"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "tizi")
			writeVersionExecutable(t, executable, "v0.1.12")
			applications := filepath.Join(dir, "Applications")
			installed := filepath.Join(applications, DesktopAppName)
			writeDesktopBundle(t, installed, "old")
			desktopZip := desktopArchive(t, "new")

			archive := archiveForVersion(t, "v0.1.13")
			cliName := fmt.Sprintf("tizi-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
			cliDigest := sha256.Sum256(archive)
			desktopDigest := sha256.Sum256(desktopZip)
			desktopDigestText := hex.EncodeToString(desktopDigest[:])
			if test.corruptDigest {
				desktopDigestText = strings.Repeat("0", 64)
			}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/release":
					assets := fmt.Sprintf(`{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}`, cliName, server.URL+"/archive", server.URL+"/checksums")
					if test.withAsset {
						assets += fmt.Sprintf(`,{"name":%q,"browser_download_url":%q}`, desktopAsset, server.URL+"/desktop")
					}
					_, _ = fmt.Fprintf(writer, `{"tag_name":"v0.1.13","assets":[%s]}`, assets)
				case "/archive":
					_, _ = writer.Write(archive)
				case "/desktop":
					_, _ = writer.Write(desktopZip)
				case "/checksums":
					_, _ = fmt.Fprintf(writer, "%s  %s\n%s  %s\n", hex.EncodeToString(cliDigest[:]), cliName, desktopDigestText, desktopAsset)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			result, err := install(context.Background(), "v0.1.13", server.URL+"/release", executable, installed, server.Client())
			if err != nil {
				t.Fatalf("命令行更新不应因桌面版失败: %v", err)
			}
			if output, _ := exec.Command(executable, "version").CombinedOutput(); !strings.Contains(string(output), "v0.1.13") {
				t.Fatalf("命令行没有更新: %s", output)
			}
			if (result.DesktopApp != "") != test.wantUpdated || (result.DesktopError != "") != test.wantError {
				t.Fatalf("result = %+v", result)
			}
			marker, err := os.ReadFile(filepath.Join(installed, "Contents", "Resources", "marker"))
			if err != nil || strings.TrimSpace(string(marker)) != test.wantMarker {
				t.Fatalf("桌面版内容 = %q (%v)，期望 %s", marker, err, test.wantMarker)
			}
			leftovers, _ := filepath.Glob(filepath.Join(applications, ".TiziDesktop.*"))
			if len(leftovers) != 0 {
				t.Fatalf("暂存或备份目录没有清理: %v", leftovers)
			}
		})
	}
}

func writeDesktopBundle(t *testing.T, app, marker string) {
	t.Helper()
	for dir, files := range map[string]map[string]string{
		filepath.Join(app, "Contents"):              {"Info.plist": "<plist><dict><key>CFBundleIdentifier</key><string>" + desktopBundleID + "</string></dict></plist>\n"},
		filepath.Join(app, "Contents", "MacOS"):     {"TiziDesktop": "#!/bin/sh\n"},
		filepath.Join(app, "Contents", "Resources"): {"marker": marker + "\n"},
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func desktopArchive(t *testing.T, marker string) []byte {
	t.Helper()
	dir := t.TempDir()
	app := filepath.Join(dir, DesktopAppName)
	writeDesktopBundle(t, app, marker)
	zipPath := filepath.Join(dir, desktopAsset)
	if output, err := exec.Command("ditto", "-c", "-k", "--keepParent", app, zipPath).CombinedOutput(); err != nil {
		t.Fatalf("ditto: %v: %s", err, output)
	}
	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDesktopNote(t *testing.T) {
	if (Result{}).DesktopNote() != "" {
		t.Fatal("没有桌面版时不应提示")
	}
	if !strings.Contains((Result{DesktopApp: "/Applications/x.app"}).DesktopNote(), "重新打开") {
		t.Fatal("更新成功时应提示重新打开")
	}
	if !strings.Contains((Result{DesktopError: "boom"}).DesktopNote(), "boom") {
		t.Fatal("失败时应带上原因")
	}
}
