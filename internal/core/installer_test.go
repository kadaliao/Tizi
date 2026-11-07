package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kadaliao/tizi/internal/config"
)

func TestSelectCoreAsset(t *testing.T) {
	latest := release{
		TagName: "Prerelease-Alpha",
		Assets: []releaseAsset{
			{Name: "mihomo-darwin-arm64-alpha-bd749c6.gz", URL: "alpha"},
			{Name: "mihomo-darwin-arm64-go124-alpha-bd749c6.gz", URL: "go124"},
			{Name: "mihomo-darwin-amd64-alpha-bd749c6.gz", URL: "amd64"},
		},
	}
	asset, version, err := selectCoreAsset(latest, "darwin", "arm64", config.CoreChannelAlpha)
	if err != nil {
		t.Fatal(err)
	}
	if asset.URL != "alpha" || version != "alpha-bd749c6" {
		t.Fatalf("asset = %#v, version = %q", asset, version)
	}

	latest = release{TagName: "v1.19.28", Assets: []releaseAsset{{Name: "mihomo-darwin-arm64-v1.19.28.gz", URL: "stable"}}}
	asset, version, err = selectCoreAsset(latest, "darwin", "arm64", config.CoreChannelStable)
	if err != nil {
		t.Fatal(err)
	}
	if asset.URL != "stable" || version != "v1.19.28" {
		t.Fatalf("asset = %#v, version = %q", asset, version)
	}
}

func TestSelectCoreAssetRejectsVariantOnlyAlphaRelease(t *testing.T) {
	latest := release{Assets: []releaseAsset{{Name: "mihomo-darwin-arm64-go124-alpha-bd749c6.gz"}}}
	if _, _, err := selectCoreAsset(latest, "darwin", "arm64", config.CoreChannelAlpha); err == nil {
		t.Fatal("selectCoreAsset() accepted a non-standard Alpha variant")
	}
}

func TestSelectLinuxCoreAsset(t *testing.T) {
	latest := release{
		TagName: "v1.19.28",
		Assets: []releaseAsset{
			{Name: "mihomo-linux-amd64-v1.19.28.gz", URL: "standard"},
			{Name: "mihomo-linux-amd64-compatible-v1.19.28.gz", URL: "compatible"},
			{Name: "mihomo-linux-amd64-v3-v1.19.28.gz", URL: "v3"},
		},
	}
	asset, version, err := selectCoreAsset(latest, "linux", "amd64", config.CoreChannelStable)
	if err != nil {
		t.Fatal(err)
	}
	if asset.URL != "compatible" || version != "v1.19.28" {
		t.Fatalf("asset = %#v, version = %q", asset, version)
	}
	if _, _, err := selectCoreAsset(latest, "linux", "arm64", config.CoreChannelStable); err == nil {
		t.Fatal("selectCoreAsset() accepted unsupported Ubuntu arm64")
	}

	latest.Assets = latest.Assets[:1]
	if _, _, err := selectCoreAsset(latest, "linux", "amd64", config.CoreChannelStable); err == nil || !strings.Contains(err.Error(), "amd64-compatible") {
		t.Fatalf("selectCoreAsset() silently fell back to an incompatible asset: %v", err)
	}
}

func TestSelectLinuxAlphaCoreAsset(t *testing.T) {
	latest := release{
		TagName: "Prerelease-Alpha",
		Assets: []releaseAsset{
			{Name: "mihomo-linux-amd64-alpha-bd749c6.gz", URL: "standard"},
			{Name: "mihomo-linux-amd64-compatible-alpha-bd749c6.gz", URL: "compatible"},
			{Name: "mihomo-linux-amd64-v3-alpha-bd749c6.gz", URL: "v3"},
		},
	}
	asset, version, err := selectCoreAsset(latest, "linux", "amd64", config.CoreChannelAlpha)
	if err != nil {
		t.Fatal(err)
	}
	if asset.URL != "compatible" || version != "alpha-bd749c6" {
		t.Fatalf("asset = %#v, version = %q", asset, version)
	}
}

func TestValidateIncludesCommandErrorWithoutOutput(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mihomo")
	if err := os.WriteFile(binary, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Validate(binary, t.TempDir(), filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil {
		t.Fatal("Validate() returned nil")
	}
	if !strings.Contains(err.Error(), "Mihomo 配置校验失败:") || strings.HasSuffix(err.Error(), ":") {
		t.Fatalf("Validate() error = %q, want command failure detail", err)
	}
}

func TestRateLimitDetail(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	if got := rateLimitDetail(response); got != "" {
		t.Fatalf("no headers should yield empty detail, got %q", got)
	}
	response.Header.Set("X-RateLimit-Remaining", "0")
	response.Header.Set("X-RateLimit-Reset", "1700000000")
	got := rateLimitDetail(response)
	if !strings.Contains(got, "剩余 0") || !strings.Contains(got, "重置") || !strings.Contains(got, "GITHUB_TOKEN") {
		t.Fatalf("unexpected detail: %q", got)
	}
	response.StatusCode = http.StatusOK
	if got := rateLimitDetail(response); got != "" {
		t.Fatalf("non-limited status should yield empty detail, got %q", got)
	}
}

func TestDoGetAttachesTokenOnlyToGitHubAPI(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token")
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = request.Header.Get("Authorization")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	response, err := doGet(context.Background(), server.URL, http.ProxyFromEnvironment, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if seen != "" {
		t.Fatalf("non-GitHub host must not receive the token, got %q", seen)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/x/y/releases/latest", nil)
	if request.URL.Host != "api.github.com" {
		t.Fatal("sanity check on host parsing failed")
	}
}

func TestVersionAndValidateTimeOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	binary := filepath.Join(t.TempDir(), "mihomo")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldVersion, oldValidate := versionTimeout, validateTimeout
	versionTimeout, validateTimeout = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { versionTimeout, validateTimeout = oldVersion, oldValidate })
	if got := Version(binary); !strings.Contains(got, "超时") {
		t.Fatalf("Version() = %q, want timeout", got)
	}
	if err := Validate(binary, t.TempDir(), "config.yaml"); err == nil || !strings.Contains(err.Error(), "未完成") {
		t.Fatalf("Validate() = %v, want timeout", err)
	}
}
