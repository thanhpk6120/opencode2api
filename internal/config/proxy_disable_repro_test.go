package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A disabled proxy must stay disabled. Previously an empty effective list fell
// back to "direct", so the operator could never turn egress off.
func TestRuntimeProxiesKeepsDisabledEntries(t *testing.T) {
	direct := Config{Proxies: []string{"direct"}}
	direct.ProxyDisabled = []string{Fingerprint("direct")}
	if got := direct.RuntimeProxies(); len(got) != 0 {
		t.Fatalf("disabled direct still routed traffic: %q", got)
	}

	proxy := "http://127.0.0.1:7890"
	only := Config{Proxies: []string{proxy}}
	only.ProxyDisabled = []string{Fingerprint(proxy)}
	if got := only.RuntimeProxies(); len(got) != 0 {
		t.Fatalf("disabling the only proxy fell back to %q", got)
	}

	oneOfTwo := Config{Proxies: []string{"direct", proxy}}
	oneOfTwo.ProxyDisabled = []string{Fingerprint(proxy)}
	if got := oneOfTwo.RuntimeProxies(); len(got) != 1 || got[0] != "direct" {
		t.Fatalf("RuntimeProxies = %q, want [direct]", got)
	}
}

// An untouched configuration keeps the historic "direct" default.
func TestRuntimeProxiesDefaultsToDirect(t *testing.T) {
	cfg := Config{}
	if got := cfg.RuntimeProxies(); len(got) != 1 || got[0] != "direct" {
		t.Fatalf("RuntimeProxies = %q, want [direct]", got)
	}
}

func TestNormalizeAcceptsDisablingEveryProxy(t *testing.T) {
	base := `{"listen":"127.0.0.1:8080","server_keys":["k-1234567890"],"anonymous":true,` +
		`"proxies":["direct"],"proxy_disabled":["` + Fingerprint("direct") + `"],` +
		`"upstream":{"zen":"https://opencode.ai/zen","go":"https://opencode.ai/zen/go"},` +
		`"retry":{"max_attempts":1,"timeout_seconds":1},"models":{"refresh_seconds":60}}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.RuntimeProxies(); len(got) != 0 {
		t.Fatalf("normalized config still routes through %q", got)
	}
}

func TestProxyEntriesForSource(t *testing.T) {
	static := "http://127.0.0.1:7890"
	rotating := "http://127.0.0.1:7891#pool"
	linked := "http://127.0.0.1:7892"
	entries := []string{"direct", static, rotating, linked}

	cfg := Config{Proxies: entries, ProxyRotateLinks: map[string]string{Fingerprint(linked): "https://p.example/rotate"}}
	if got := cfg.ProxyEntriesForSource(entries); len(got) != len(entries) {
		t.Fatalf("both mode filtered entries: %q", got)
	}

	cfg.ProxySource = ProxySourceStatic
	got := cfg.ProxyEntriesForSource(entries)
	if len(got) != 2 || got[0] != "direct" || got[1] != static {
		t.Fatalf("static mode = %q", got)
	}

	cfg.ProxySource = ProxySourceRotating
	got = cfg.ProxyEntriesForSource(entries)
	if len(got) != 2 || got[0] != rotating || got[1] != linked {
		t.Fatalf("rotating mode = %q", got)
	}
}

func TestNormalizeRotatingProxyDefaultsAndValidation(t *testing.T) {
	cfg := Config{}
	if err := normalizeRotatingProxy(&cfg); err != nil {
		t.Fatalf("normalizeRotatingProxy: %v", err)
	}
	if cfg.Rotating.MinIntervalSeconds != DefaultRotateMinIntervalSeconds ||
		cfg.Rotating.MaxIntervalSeconds != DefaultRotateMaxIntervalSeconds ||
		cfg.Rotating.FailedIPCacheSize != DefaultRotateFailedIPCacheSize {
		t.Fatalf("defaults not applied: %+v", cfg.Rotating)
	}

	bad := Config{ProxySource: "sometimes"}
	if err := normalizeRotatingProxy(&bad); err == nil {
		t.Fatal("unknown proxy_source accepted")
	}

	bad = Config{Rotating: RotatingProxyConfig{MinIntervalSeconds: 600, MaxIntervalSeconds: 60}}
	if err := normalizeRotatingProxy(&bad); err == nil {
		t.Fatal("max_interval_seconds below min_interval_seconds accepted")
	}

	bad = Config{Rotating: RotatingProxyConfig{FailedIPCacheSize: 500}}
	if err := normalizeRotatingProxy(&bad); err == nil {
		t.Fatal("oversized failed_ip_cache_size accepted")
	}
}
