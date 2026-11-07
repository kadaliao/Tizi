package configshare

import (
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestSessionServesOnlyTokenizedConfigURL(t *testing.T) {
	content := []byte("proxies:\n  - name: openvpn-1\n")
	session, err := start(content, "127.0.0.1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	response, err := http.Get(session.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK || string(body) != string(content) {
		t.Fatalf("GET config = status %d body %q", response.StatusCode, body)
	}
	if got := response.Header.Get("Profile-Title"); got != "Tizi" {
		t.Fatalf("Profile-Title = %q", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	baseURL := session.URL[:strings.Index(session.URL, "/config.yaml")]
	response, err = http.Get(baseURL + "/wrong/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong token status = %d", response.StatusCode)
	}
}

func TestSessionExpires(t *testing.T) {
	session, err := start([]byte("proxies: []\n"), "127.0.0.1", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		t.Fatal("session did not expire")
	}
	if _, err := http.Get(session.URL); err == nil {
		t.Fatal("expired session still accepted connections")
	}
}

func TestInterfaceScorePrefersPhysicalLAN(t *testing.T) {
	wifi := interfaceScore("en0", netip.MustParseAddr("192.168.1.20"))
	tunnel := interfaceScore("utun5", netip.MustParseAddr("10.0.0.2"))
	if wifi <= tunnel {
		t.Fatalf("physical LAN score %d must exceed tunnel score %d", wifi, tunnel)
	}
}

func TestQRCodeRenderersSupportTUIAndCLI(t *testing.T) {
	const content = "http://192.168.1.20:54321/token/config.yaml"
	dark, darkWidth, err := RenderQRCode(content, true)
	if err != nil {
		t.Fatal(err)
	}
	light, lightWidth, err := RenderQRCode(content, false)
	if err != nil {
		t.Fatal(err)
	}
	terminal, terminalWidth, err := RenderANSIQRCode(content)
	if err != nil {
		t.Fatal(err)
	}
	if darkWidth != lightWidth || lightWidth != terminalWidth || dark == light {
		t.Fatalf("QR render widths = %d/%d/%d, palettes equal=%v", darkWidth, lightWidth, terminalWidth, dark == light)
	}
	if !strings.Contains(terminal, "\x1b[40m") || !strings.Contains(terminal, "\x1b[47m") {
		t.Fatal("ANSI QR code does not force black and white modules")
	}
}
