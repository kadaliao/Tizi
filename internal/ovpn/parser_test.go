package ovpn

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const certificateProfile = `client
dev tun
proto udp4
remote a.vpn.example.com 1194
remote b.vpn.example.com 443
cipher AES-256-CBC
auth SHA1
keepalive 3 10
<ca>
-----BEGIN CERTIFICATE-----
CA-DATA
-----END CERTIFICATE-----
</ca>
<cert>
Certificate:
-----BEGIN CERTIFICATE-----
CERT-DATA
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
KEY-DATA
-----END PRIVATE KEY-----
</key>
<tls-crypt>
-----BEGIN OpenVPN Static key V1-----
TLS-DATA
-----END OpenVPN Static key V1-----
</tls-crypt>
`

func TestParseCertificateProfile(t *testing.T) {
	profile, err := Parse(certificateProfile)
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Remotes) != 2 || profile.Remotes[1].Port != 443 {
		t.Fatalf("unexpected remotes: %#v", profile.Remotes)
	}
	if profile.Proto != "udp" || profile.Cipher != "AES-256-GCM" {
		t.Fatalf("unexpected normalization: proto=%s cipher=%s", profile.Proto, profile.Cipher)
	}
	if profile.Ping != 3 || profile.PingRestart != 10 {
		t.Fatalf("unexpected keepalive: ping=%d restart=%d", profile.Ping, profile.PingRestart)
	}
	selected, err := profile.SelectRemotes("b")
	if err != nil || len(selected) != 1 || selected[0].Host != "b.vpn.example.com" {
		t.Fatalf("unexpected selection: %#v, %v", selected, err)
	}
	selected, err = profile.SelectRemotes("a, b")
	if err != nil || len(selected) != 2 || selected[0].Host != "a.vpn.example.com" || selected[1].Host != "b.vpn.example.com" {
		t.Fatalf("unexpected multi-selection: %#v, %v", selected, err)
	}
	if _, err := profile.SelectRemotes("a,missing"); err == nil {
		t.Fatal("missing item in multi-selection should fail")
	}
}

func TestParseCredentialFile(t *testing.T) {
	dir := t.TempDir()
	credentials := filepath.Join(dir, "credentials.txt")
	if err := os.WriteFile(credentials, []byte("user@example.com\nsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `remote vpn.example.com 1194
auth-user-pass credentials.txt
<ca>
-----BEGIN CERTIFICATE-----
CA-DATA
-----END CERTIFICATE-----
</ca>
`
	path := filepath.Join(dir, "profile.ovpn")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Username != "user@example.com" || profile.Password != "secret" {
		t.Fatal("credentials were not loaded")
	}
}

func TestParseRejectsIncompleteCertificate(t *testing.T) {
	content := `remote vpn.example.com 1194
<ca>
-----BEGIN CERTIFICATE-----
CA
-----END CERTIFICATE-----
</ca>
<cert>
-----BEGIN CERTIFICATE-----
CERT
-----END CERTIFICATE-----
</cert>`
	if _, err := Parse(content); err == nil {
		t.Fatal("expected incomplete certificate error")
	}
}

func TestParseNCPDisabledUsesLegacyCipher(t *testing.T) {
	content := `remote vpn.example.com 1194
cipher AES-256-CBC
ncp-disable
<ca>
-----BEGIN CERTIFICATE-----
CA
-----END CERTIFICATE-----
</ca>
<cert>
-----BEGIN CERTIFICATE-----
CERT
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
KEY
-----END PRIVATE KEY-----
</key>`
	profile, err := Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Cipher != "AES-256-CBC" {
		t.Fatalf("unexpected ncp-disabled cipher: %s", profile.Cipher)
	}
}

func TestParseExternalFilesAndDataCiphers(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"cert files/ca.crt": "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
		"client.crt":        "-----BEGIN CERTIFICATE-----\nCERT\n-----END CERTIFICATE-----\n",
		"client.key":        "-----BEGIN PRIVATE KEY-----\nKEY\n-----END PRIVATE KEY-----\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	content := `proto udp
remote vpn.example.com 443 tcp-client
data-ciphers AES-256-GCM:AES-256-CBC
cipher AES-256-CBC
ca "cert files/ca.crt"
cert client.crt
key client.key
`
	path := filepath.Join(dir, "external.ovpn")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Cipher != "AES-256-GCM" || profile.Remotes[0].Proto != "tcp" {
		t.Fatalf("unexpected profile: %#v", profile)
	}
}

func TestParseKeepsInlineCAChain(t *testing.T) {
	content := "remote vpn.example.com 1194\nauth-user-pass\n<auth-user-pass>\nuser\npass\n</auth-user-pass>\n<ca>\n" +
		"# root then intermediate\n-----BEGIN CERTIFICATE-----\nROOT\n-----END CERTIFICATE-----\n" +
		"subject=intermediate\n-----BEGIN CERTIFICATE-----\nINTERMEDIATE\n-----END CERTIFICATE-----\n</ca>\n"
	profile, err := Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	want := "-----BEGIN CERTIFICATE-----\nROOT\n-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\nINTERMEDIATE\n-----END CERTIFICATE-----"
	if profile.CA != want {
		t.Fatalf("CA chain = %q, want %q", profile.CA, want)
	}
	if strings.Count(profile.CA, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("CA chain lost a certificate: %q", profile.CA)
	}
}

func TestParseCRLFProfile(t *testing.T) {
	content := strings.ReplaceAll(certificateProfile, "\n", "\r\n")
	profile, err := Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Remotes) != 2 || profile.Ping != 3 {
		t.Fatalf("unexpected CRLF profile: %#v", profile)
	}
	for name, value := range map[string]string{"ca": profile.CA, "cert": profile.Cert, "key": profile.Key, "tls-crypt": profile.TLSCrypt} {
		if strings.Contains(value, "\r") {
			t.Fatalf("%s still contains CR: %q", name, value)
		}
	}
	if profile.Cert != "-----BEGIN CERTIFICATE-----\nCERT-DATA\n-----END CERTIFICATE-----" {
		t.Fatalf("unexpected cert: %q", profile.Cert)
	}
}

func TestParseAcceptsTLSAuthProfile(t *testing.T) {
	inline := `remote vpn.example.com 1194
key-direction 1
<ca>
-----BEGIN CERTIFICATE-----
CA
-----END CERTIFICATE-----
</ca>
<cert>
-----BEGIN CERTIFICATE-----
CERT
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
KEY
-----END PRIVATE KEY-----
</key>
<tls-auth>
-----BEGIN OpenVPN Static key V1-----
TA
-----END OpenVPN Static key V1-----
</tls-auth>
`
	parsed, err := Parse(inline)
	if err != nil {
		t.Fatalf("inline tls-auth should be accepted: %v", err)
	}
	if !strings.Contains(parsed.TLSAuth, "TA") || parsed.KeyDirection != "1" {
		t.Fatalf("inline tls-auth = %q, direction %q", parsed.TLSAuth, parsed.KeyDirection)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ta.key"), []byte("-----BEGIN OpenVPN Static key V1-----\nTA\n-----END OpenVPN Static key V1-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	external := `remote vpn.example.com 1194
auth-user-pass
<auth-user-pass>
user
pass
</auth-user-pass>
<ca>
-----BEGIN CERTIFICATE-----
CA
-----END CERTIFICATE-----
</ca>
tls-auth ta.key 1
`
	path := filepath.Join(dir, "profile.ovpn")
	if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := ParseFile(path)
	if err != nil {
		t.Fatalf("file tls-auth should be accepted: %v", err)
	}
	if !strings.Contains(fromFile.TLSAuth, "OpenVPN Static key V1") || fromFile.KeyDirection != "1" {
		t.Fatalf("file tls-auth = %q, direction %q", fromFile.TLSAuth, fromFile.KeyDirection)
	}
	profile, err := parseContent(external)
	if err != nil {
		t.Fatal(err)
	}
	if profile.KeyDirection != "1" {
		t.Fatalf("key direction = %q, want 1", profile.KeyDirection)
	}
}

func TestSplitTLSAuthDirective(t *testing.T) {
	cases := map[string][2]string{
		"ta.key":          {"ta.key", ""},
		"ta.key 1":        {"ta.key", "1"},
		`"my ta.key" 0`:   {"my ta.key", "0"},
		"'my ta.key'":     {"my ta.key", ""},
		"  spaced.key  1": {"spaced.key", "1"},
	}
	for input, want := range cases {
		path, direction := splitTLSAuthDirective(input)
		if path != want[0] || direction != want[1] {
			t.Errorf("splitTLSAuthDirective(%q) = (%q, %q), want (%q, %q)", input, path, direction, want[0], want[1])
		}
	}
}

func TestParseReadsRoutesDNSAndDomains(t *testing.T) {
	profile, err := parseContent(`remote vpn.example.com 1194
route 10.20.0.0 255.255.0.0
route 172.20.1.5
route 0.0.0.0 0.0.0.0
route vpn_gateway 255.255.255.255
route 10.20.0.0 255.255.0.0 net_gateway
dhcp-option DNS 10.20.0.53
dhcp-option DNS fd00::53
dhcp-option DOMAIN Corp.Example.com.
dhcp-option DOMAIN-SEARCH lab.example.com
`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.20.0.0/16", "172.20.1.5/32"}; !reflect.DeepEqual(profile.Routes, want) {
		t.Fatalf("routes = %#v, want %#v", profile.Routes, want)
	}
	if want := []string{"10.20.0.53"}; !reflect.DeepEqual(profile.DNS, want) {
		t.Fatalf("dns = %#v, want %#v", profile.DNS, want)
	}
	if want := []string{"corp.example.com", "lab.example.com"}; !reflect.DeepEqual(profile.Domains, want) {
		t.Fatalf("domains = %#v, want %#v", profile.Domains, want)
	}
}
