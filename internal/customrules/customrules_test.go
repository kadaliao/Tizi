package customrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDomainFromInput(t *testing.T) {
	for _, input := range []string{"example.com", "https://www.example.com/path?q=1", "HTTP://Example.COM:8443"} {
		got, err := DomainFromInput(input)
		if err != nil {
			t.Fatalf("DomainFromInput(%q): %v", input, err)
		}
		want := "example.com"
		if strings.Contains(input, "www.") {
			want = "www.example.com"
		}
		if got != want {
			t.Fatalf("DomainFromInput(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := DomainFromInput("192.0.2.1"); err == nil {
		t.Fatal("IP address should be rejected")
	}
}

func TestDestinationFromInputUsesWebsitePort(t *testing.T) {
	cases := []struct {
		input string
		host  string
		port  int
	}{
		{input: "example.com", host: "example.com", port: 443},
		{input: "https://Example.COM/path", host: "example.com", port: 443},
		{input: "http://example.com/path", host: "example.com", port: 80},
		{input: "https://example.com:8443/path", host: "example.com", port: 8443},
	}
	for _, tc := range cases {
		host, port, err := DestinationFromInput(tc.input)
		if err != nil || host != tc.host || port != tc.port {
			t.Errorf("DestinationFromInput(%q) = (%q, %d, %v), want (%q, %d, nil)", tc.input, host, port, err, tc.host, tc.port)
		}
	}
	if _, _, err := DestinationFromInput("ftp://example.com"); err == nil {
		t.Fatal("scheme without a known or explicit port should be rejected")
	}
}

func TestEnsureTemplateCreatesFileWithoutOverwritingUserRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "custom-rules.txt")
	if err := EnsureTemplate(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != Template || !strings.Contains(string(data), "IP-CIDR") {
		t.Fatalf("unexpected template: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("template permissions = %o, want 600", info.Mode().Perm())
	}

	const custom = "DOMAIN,private.example,DIRECT\n"
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureTemplate(path); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != custom {
		t.Fatalf("existing rules were overwritten: %q", data)
	}
}

func TestSaveReplacesSimpleRuleAndPreservesOtherLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-rules.txt")
	content := "# keep this comment\nDOMAIN-SUFFIX,example.com,DIRECT\nDOMAIN,exact.example,外网代理\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, Rule{Domain: "https://example.com/docs", Target: "外网代理"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "# keep this comment") || !strings.Contains(got, "DOMAIN,exact.example,外网代理") {
		t.Fatalf("unrelated lines lost: %q", got)
	}
	if strings.Count(got, "DOMAIN-SUFFIX,example.com,") != 1 || !strings.Contains(got, "DOMAIN-SUFFIX,example.com,外网代理") {
		t.Fatalf("rule was not replaced: %q", got)
	}
	rules, err := Load(path)
	if err != nil || len(rules) != 1 || rules[0].Target != "外网代理" {
		t.Fatalf("Load() = %#v, err=%v", rules, err)
	}
}

func TestParseRulesAcceptsKnownFormsAndStripsComments(t *testing.T) {
	content := "# header\r\n\r\nDOMAIN-SUFFIX,example.com,外网代理 # inline note\n" +
		"IP-CIDR,203.0.113.0/24,DIRECT,no-resolve\n" +
		"AND,((NETWORK,TCP),(DST-PORT,8443)),REJECT\n" +
		"MATCH,默认出口\n"
	rules, err := ParseRules(content)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DOMAIN-SUFFIX,example.com,外网代理",
		"IP-CIDR,203.0.113.0/24,DIRECT,no-resolve",
		"AND,((NETWORK,TCP),(DST-PORT,8443)),REJECT",
		"MATCH,默认出口",
	}
	if strings.Join(rules, "|") != strings.Join(want, "|") {
		t.Fatalf("ParseRules() = %#v, want %#v", rules, want)
	}
}

func TestParseRulesRejectsInvalidLinesWithLineNumber(t *testing.T) {
	cases := map[string]string{
		"DOMAIN-SUFFIX,example.com,DIRECT\nDOMAIN-SUFIX,typo.example,DIRECT\n": "第 2 行",
		"\n\nDOMAIN,example.com,Proxy\n":                                       "第 3 行",
		"DOMAIN-SUFFIX,example.com\n":                                          "第 1 行",
		"DOMAIN-SUFFIX,,DIRECT\n":                                              "第 1 行",
		"MATCH,DIRECT,extra\n":                                                 "第 1 行",
		"AND,NETWORK,DIRECT\n":                                                 "第 1 行",
		"DOMAIN-SUFFIX,example.com,DIRECT,bogus\n":                             "第 1 行",
	}
	for content, wantFragment := range cases {
		_, err := ParseRules(content)
		if err == nil || !strings.Contains(err.Error(), wantFragment) {
			t.Errorf("ParseRules(%q) error = %v, want containing %q", content, err, wantFragment)
		}
	}
}

func TestSaveRejectsFileWithInvalidHandWrittenLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-rules.txt")
	original := "BROKEN LINE\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Save(path, Rule{Domain: "example.com", Target: "REJECT"})
	if err == nil || !strings.Contains(err.Error(), "第 1 行") {
		t.Fatalf("Save should report the invalid line, got %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != original {
		t.Fatalf("file must stay untouched on validation failure: %q, %v", data, readErr)
	}
}

func TestSaveAcceptsRejectTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-rules.txt")
	if err := Save(path, Rule{Domain: "ads.example.com", Target: "REJECT"}); err != nil {
		t.Fatal(err)
	}
	rules, err := Load(path)
	if err != nil || len(rules) != 1 || rules[0].Target != "REJECT" {
		t.Fatalf("Load() = %#v, err=%v", rules, err)
	}
}

func TestSaveRefusesClaudeDomains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-rules.txt")
	for _, input := range []string{"claude.ai", "https://api.anthropic.com/v1", "a.claudeusercontent.com"} {
		if err := Save(path, Rule{Domain: input, Target: "DIRECT"}); err == nil || !strings.Contains(err.Error(), "Claude") {
			t.Fatalf("%s: error = %v", input, err)
		}
	}
	if err := Save(path, Rule{Domain: "notclaude.ai.example.com", Target: "Claude"}); err != nil {
		t.Fatalf("other domains may use the Claude route: %v", err)
	}
	if _, err := ParseRules("DOMAIN-SUFFIX,example.org,Claude\n"); err != nil {
		t.Fatalf("Claude is a valid route in custom-rules.txt: %v", err)
	}
}
