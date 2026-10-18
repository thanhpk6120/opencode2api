package config

import "testing"

func TestParseProxyLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"direct", "direct"},
		{"  10.0.0.1:8080  ", "http://10.0.0.1:8080"},
		{"10.0.0.1:8080:user:pw", "http://user:pw@10.0.0.1:8080"},
		{"proxy.local:3128", "http://proxy.local:3128"},
		{"[::1]:8080:u:p", "http://u:p@[::1]:8080"},
		{"socks5://u:p@h:1080", "socks5://u:p@h:1080"},
		{"http://u:p@h:8080", "http://u:p@h:8080"},
	}
	for _, c := range cases {
		got, err := ParseProxyLine(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseProxyLine(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	blanks := []string{"# comment only", "", "   ", "// x", "; y"}
	for _, b := range blanks {
		if _, err := ParseProxyLine(b); err == nil {
			t.Errorf("ParseProxyLine(%q) expected error, got nil", b)
		}
	}
	bads := []string{"nocolon", "h:", ":8080", "h:0", "h:99999", "h:abc", "h:8080:onlyuser", "h:8080:u:p:extra", "[::1", "[::1]nocolon", "ftp://h:21", "h:8080::", "h:8080:u:p:ss"}
	for _, b := range bads {
		if _, err := ParseProxyLine(b); err == nil {
			t.Errorf("ParseProxyLine(%q) expected error, got nil", b)
		}
	}
}

func TestNormalizeProxyLines(t *testing.T) {
	lines, err := NormalizeProxyLines([]string{"# head", "", "  a:1:u:p  ", "b:2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "http://u:p@a:1" || lines[1] != "http://b:2" {
		t.Errorf("unexpected bulk result: %v", lines)
	}
	if _, err := NormalizeProxyLines([]string{"good:1", "bad-no-colon"}); err == nil {
		t.Error("expected line-numbered error, got nil")
	}
}

func TestEffectiveProxyListDisablesDirect(t *testing.T) {
	proxies := []string{"direct", "http://10.0.0.1:8080"}
	disabled := []string{Fingerprint("direct")}
	eff := EffectiveProxyList(proxies, disabled)
	if len(eff) != 1 || eff[0] != "http://10.0.0.1:8080" {
		t.Errorf("EffectiveProxyList did not disable direct: %v", eff)
	}
}
