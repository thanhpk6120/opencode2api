package config

import "testing"

func TestRedactURLKeepsLiteralMask(t *testing.T) {
	got := RedactURL("http://user:pass@10.0.0.1:8080")
	if got != "http://***@10.0.0.1:8080" {
		t.Errorf("RedactURL = %q, want literal *** mask", got)
	}
	if got = RedactURL("direct"); got != "direct" {
		t.Errorf("RedactURL(direct) = %q", got)
	}
	if got = RedactURL("socks5://u:p@h:1080"); got != "socks5://***@h:1080" {
		t.Errorf("RedactURL(socks) = %q", got)
	}
}
