package domain

import "testing"

func TestNormalize(t *testing.T) {
	got, err := Normalize("HTTPS://例子.测试/path")
	if err != nil || got != "xn--fsqu00a.xn--0zwm56d" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

func TestRejectIP(t *testing.T) {
	for _, v := range []string{"1.2.3.4", "bad domain"} {
		if _, err := Normalize(v); err == nil {
			t.Errorf("expected rejection for %q", v)
		}
	}
}

func TestExtractHostPortAndLog(t *testing.T) {
	for input, want := range map[string]string{
		"example.com:443": "example.com",
		"example.com:":    "example.com",
		"[TCP] 61.52.219.206:48638 --> subs.2519885.dpdns.org:443 match Match using group": "subs.2519885.dpdns.org",
		"[TCP] 192.168.1.2:1234 --> newapi.linuxdo.edu.rs: match Match using group":        "newapi.linuxdo.edu.rs",
	} {
		got, err := Normalize(input)
		if err != nil || got != want {
			t.Fatalf("Normalize(%q)=%q,%v want %q", input, got, err, want)
		}
	}
}

func TestRejectMalformedTrailingPorts(t *testing.T) {
	for _, input := range []string{"example.com::", "example.com:443:8443"} {
		if _, err := Normalize(input); err == nil {
			t.Fatalf("Normalize(%q) should reject malformed port", input)
		}
	}
}

func TestExtractMultipleCandidates(t *testing.T) {
	got, err := Extract("see example.com and example.net")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %#v err=%v", got, err)
	}
	if _, err := Normalize("see example.com and example.net"); err == nil {
		t.Fatal("Normalize must reject ambiguous text")
	}
}

func TestRegistrable(t *testing.T) {
	if got := Registrable("subs.2519885.dpdns.org"); got != "dpdns.org" {
		t.Fatalf("got %q", got)
	}
}

func TestRuleRootIsStrict(t *testing.T) {
	for input, want := range map[string]string{
		"cdn.legendsen.se":        "legendsen.se",
		"subs.2519885.dpdns.org":  "2519885.dpdns.org",
		"www.example.co.uk":       "example.co.uk",
		"xn--fsqu00a.xn--0zwm56d": "xn--fsqu00a.xn--0zwm56d",
	} {
		if got := RuleRoot(input); got != want {
			t.Fatalf("RuleRoot(%q)=%q want %q", input, got, want)
		}
	}
	if got := RuleRoot("co.uk"); got != "" {
		t.Fatalf("public suffix must not become a rule root: %q", got)
	}
}

func TestNormalizePlainDomainWithLetterT(t *testing.T) {
	got, err := Normalize("testingcf.jsdelivr.net")
	if err != nil || got != "testingcf.jsdelivr.net" {
		t.Fatalf("got %q, err %v", got, err)
	}
}
