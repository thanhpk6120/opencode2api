package config

import "testing"

func TestParseProxyEntry(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		pool bool
	}{
		{"direct", "direct", false},
		{"", "", false},
		{"  http://127.0.0.1:7890  ", "http://127.0.0.1:7890", false},
		{"http://127.0.0.1:7890", "http://127.0.0.1:7890", false},
		{"http://127.0.0.1:7890#pool", "http://127.0.0.1:7890", true},
		{"http://127.0.0.1:7890#POOL", "http://127.0.0.1:7890", true},
		{"http://user:pass@127.0.0.1:7890#pool", "http://user:pass@127.0.0.1:7890", true},
		{"socks5://127.0.0.1:1080#pool", "socks5://127.0.0.1:1080", true},
		{"socks5h://127.0.0.1:1080#pool", "socks5h://127.0.0.1:1080", true},
		{"pool+http://127.0.0.1:7890", "http://127.0.0.1:7890", true},
		{"pool+socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080", true},
		// A fragment that is not the pool marker belongs to the proxy URL.
		{"http://127.0.0.1:7890#other", "http://127.0.0.1:7890#other", false},
	}
	for _, tc := range cases {
		clean, pool := ParseProxyEntry(tc.raw)
		if clean != tc.want || pool != tc.pool {
			t.Errorf("ParseProxyEntry(%q) = (%q, %v), want (%q, %v)", tc.raw, clean, pool, tc.want, tc.pool)
		}
	}
}

func TestRedactURLKeepsPoolMarker(t *testing.T) {
	// RedactURL preserves literal *** in UI and logs.
	const masked = "http://***@127.0.0.1:7890#pool"
	if got := RedactURL("http://user:pass@127.0.0.1:7890#pool"); got != masked {
		t.Errorf("pool URL redacted as %q, want %q", got, masked)
	}
	if got := RedactURL("pool+http://user:pass@127.0.0.1:7890"); got != masked {
		t.Errorf("pool+ URL redacted as %q, want %q", got, masked)
	}
	if got := RedactURL("http://127.0.0.1:7890"); got != "http://127.0.0.1:7890" {
		t.Errorf("static URL redacted as %q", got)
	}
}

// The same endpoint listed statically and as a pool is two distinct routes and
// must survive deduplication; an exact repeat must not.
func TestResolveProxyFilesKeepsPoolVariantOfSameEndpoint(t *testing.T) {
	cfg := Config{Proxies: []string{
		"http://127.0.0.1:7890",
		"http://127.0.0.1:7890#pool",
		"http://127.0.0.1:7890",
	}}
	if err := resolveProxyFiles("config.json", &cfg); err != nil {
		t.Fatalf("resolveProxyFiles failed: %v", err)
	}
	got := cfg.RuntimeProxies()
	want := []string{"http://127.0.0.1:7890", "http://127.0.0.1:7890#pool"}
	if len(got) != len(want) {
		t.Fatalf("resolved proxies = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolved proxies = %q, want %q", got, want)
		}
	}
}

// A pool entry must validate against the clean URL, and its credentials must
// stay redactable after the marker is stripped.
func TestNormalizeAcceptsPoolProxy(t *testing.T) {
	cfg := Config{
		Listen:      "127.0.0.1:8080",
		ServerKeys:  []string{"local-key"},
		Anonymous:   true,
		Proxies:     []string{"http://user:secret@127.0.0.1:7890#pool", "direct"},
		Upstream:    UpstreamConfig{Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go", Anonymous: "https://opencode.ai/inference"},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 10, MaxIdleConnsPerHost: 10, IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15},
		Logging:     LoggingConfig{Level: "info", RingSize: 100},
		Prefer:      TierGo,
	}
	normalized, err := Normalize("config.json", cfg)
	if err != nil {
		t.Fatalf("Normalize rejected a pool proxy: %v", err)
	}
	redactor := NewSecretRedactor()
	redactor.Replace(normalized)
	if got := redactor.String("dial http://user:secret@127.0.0.1:7890 failed"); got != "dial *** failed" {
		t.Errorf("pool proxy credentials survived redaction: %q", got)
	}
}

// A rotate link is operator input that the gateway calls server-side, so only
// http/https survives normalization; anything else must be rejected rather
// than stored.
func TestNormalizeRotateLinkRejectsNonHTTP(t *testing.T) {
	if _, err := NormalizeRotateLink("file:///etc/shadow"); err == nil {
		t.Error("file:// rotate link accepted")
	}
	if _, err := NormalizeRotateLink("javascript:alert(1)"); err == nil {
		t.Error("javascript: rotate link accepted")
	}
	if got, err := NormalizeRotateLink("  https://p.example/change?token=1  "); err != nil || got != "https://p.example/change?token=1" {
		t.Errorf("NormalizeRotateLink = (%q, %v)", got, err)
	}
	if got, err := NormalizeRotateLink("   "); err != nil || got != "" {
		t.Errorf("blank rotate link = (%q, %v), want empty and no error", got, err)
	}
}

// Rotate links are keyed by proxy fingerprint and must survive a config
// round-trip through Normalize, including an edited-address remap.
func TestNormalizeKeepsRotateLinks(t *testing.T) {
	proxy := "http://user:pass@127.0.0.1:7890"
	cfg := Config{
		Listen:           "127.0.0.1:8080",
		ServerKeys:       []string{"local-key"},
		Anonymous:        true,
		Proxies:          []string{proxy},
		ProxyRotateLinks: map[string]string{Fingerprint(proxy): "https://p.example/change", "stale": "  "},
		Upstream:         UpstreamConfig{Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go", Anonymous: "https://opencode.ai/inference"},
		Retry:            RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models:           ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance:      PerformanceConfig{MaxIdleConns: 10, MaxIdleConnsPerHost: 10, IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15},
		Logging:          LoggingConfig{Level: "info", RingSize: 100},
		Prefer:           TierGo,
	}
	normalized, err := Normalize("config.json", cfg)
	if err != nil {
		t.Fatalf("Normalize rejected rotate links: %v", err)
	}
	if got := normalized.ProxyRotateLink(Fingerprint(proxy)); got != "https://p.example/change" {
		t.Errorf("rotate link lost: %q", got)
	}
	if _, ok := normalized.ProxyRotateLinks["stale"]; ok {
		t.Error("blank rotate link was persisted")
	}
	// Clone must deep-copy the map, or an admin update would mutate the live cfg.
	clone := Clone(normalized)
	clone.ProxyRotateLinks[Fingerprint(proxy)] = "https://other.example"
	if normalized.ProxyRotateLink(Fingerprint(proxy)) != "https://p.example/change" {
		t.Error("Clone shares the rotate-link map with the original")
	}
}
