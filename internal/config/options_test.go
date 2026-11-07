package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitListAndNormalizeDomains(t *testing.T) {
	if got := SplitList(" a.com, b.com，c.com\nd.com a.com "); !reflect.DeepEqual(got, []string{"a.com", "b.com", "c.com", "d.com"}) {
		t.Fatalf("SplitList = %#v", got)
	}
	if got := NormalizeDomains([]string{"*.Corp.Example.com.", "+.lab.example.com", ".x.io", "corp.example.com"}); !reflect.DeepEqual(got, []string{"corp.example.com", "lab.example.com", "x.io"}) {
		t.Fatalf("NormalizeDomains = %#v", got)
	}
}

func TestValidateIntranet(t *testing.T) {
	valid := Settings{
		IntranetDomains:   []string{"corp.example.com", "internal"},
		IntranetCIDRs:     []string{"10.0.0.0/8"},
		IntranetDNS:       []string{"10.0.0.53"},
		IntranetHTTPProxy: "10.0.0.2:3128",
		IntranetProbeURL:  "http://wiki.corp.example.com/",
	}
	if err := ValidateIntranet(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Settings){
		"domain": func(s *Settings) { s.IntranetDomains = []string{"bad/domain"} },
		"cidr":   func(s *Settings) { s.IntranetCIDRs = []string{"10.0.0.0"} },
		"dns":    func(s *Settings) { s.IntranetDNS = []string{"dns.corp"} },
		"http":   func(s *Settings) { s.IntranetHTTPProxy = "10.0.0.2" },
		"probe":  func(s *Settings) { s.IntranetProbeURL = "wiki.corp" },
	} {
		settings := valid
		mutate(&settings)
		if err := ValidateIntranet(settings); err == nil {
			t.Errorf("%s: invalid value accepted", name)
		}
	}
}

func TestMaskSubscriptionAndSecret(t *testing.T) {
	if got := MaskSubscription("https://sub.example.com/api/v1/client/subscribe?token=abc"); got != "https://sub.example.com/…" {
		t.Fatalf("MaskSubscription = %q", got)
	}
	if got := MaskSecret("tskey-auth-abcdefghijklmnop"); strings.Contains(got, "ijkl") || !strings.HasPrefix(got, "tskey-au") {
		t.Fatalf("MaskSecret = %q", got)
	}
	if err := ValidateSubscription("ftp://x"); err == nil {
		t.Fatal("non-http subscription accepted")
	}
	if err := ValidateTailscale(Settings{TailscaleHostname: "bad name"}); err == nil {
		t.Fatal("invalid hostname accepted")
	}
}
