package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
)

func TestExitRangeGroupsBySubnet(t *testing.T) {
	cases := map[string]string{
		"203.0.113.77":      "203.0.113.0/24",
		"203.0.113.4":       "203.0.113.0/24",
		"198.51.100.9":      "198.51.100.0/24",
		"2001:db8:abcd::42": "2001:db8:abcd::/48",
		"not-an-ip":         "not-an-ip",
	}
	for input, want := range cases {
		if got := exitRange(input); got != want {
			t.Errorf("exitRange(%q) = %q, want %q", input, got, want)
		}
	}
}

// The cache keeps only the newest failed_ip_cache_size ranges; the oldest is
// dropped when a new one arrives.
func TestFailedIPCacheIsBounded(t *testing.T) {
	r := &proxyRotator{cfg: config.RotatingProxyConfig{FailedIPCacheSize: 3}}
	for _, rangeKey := range []string{"a", "b", "c", "d"} {
		r.markBad(rangeKey)
	}
	if len(r.bad) != 3 {
		t.Fatalf("cache holds %d ranges, want 3: %q", len(r.bad), r.bad)
	}
	if r.isBad("a") {
		t.Fatal("oldest range was not evicted")
	}
	if !r.isBad("d") {
		t.Fatal("newest range was not cached")
	}
	// Duplicates must not consume slots.
	r.markBad("d")
	if len(r.bad) != 3 {
		t.Fatalf("duplicate range grew the cache to %d", len(r.bad))
	}
}

// The minimum interval blocks a burst of failures from calling the rotate link
// repeatedly.
func TestRotationRespectsMinimumInterval(t *testing.T) {
	var calls atomic.Int32
	link := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}))
	defer link.Close()

	name := "http://127.0.0.1:7890"
	pool, err := newTransportPool([]string{name}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	cfg := config.Config{
		Proxies:          []string{name},
		ProxyRotateLinks: map[string]string{config.Fingerprint(name): link.URL},
		Rotating:         config.RotatingProxyConfig{AutoRotate: true, MinIntervalSeconds: 3600, FailedIPCacheSize: 10},
	}
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger()}
	g.rotator = newProxyRotator(g, cfg.Rotating)

	proxy := pool.items[0]
	g.rotator.rotate(proxy, "test")
	first := calls.Load()
	if first != 1 {
		t.Fatalf("rotate link called %d times, want 1", first)
	}
	g.rotator.rotate(proxy, "test")
	if got := calls.Load(); got != first {
		t.Fatalf("rotation bypassed the minimum interval: %d calls", got)
	}
}

// Auto-rotation is opt-in: without it, a failed model call must not touch the
// rotate link.
func TestObserveRotatingFailureRequiresOptIn(t *testing.T) {
	var calls atomic.Int32
	link := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}))
	defer link.Close()

	name := "http://127.0.0.1:7890"
	pool, err := newTransportPool([]string{name}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	cfg := config.Config{
		Proxies:          []string{name},
		ProxyRotateLinks: map[string]string{config.Fingerprint(name): link.URL},
		Rotating:         config.RotatingProxyConfig{AutoRotate: false, FailedIPCacheSize: 10},
	}
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger()}
	g.rotator = newProxyRotator(g, cfg.Rotating)

	g.observeRotatingFailure(pool.items[0])
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("auto_rotate=false still called the rotate link %d times", got)
	}
}

func TestRotatingProxiesSelectsOnlyLinkedEntries(t *testing.T) {
	pool, err := newTransportPool([]string{"direct", "http://127.0.0.1:7890"}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	linked := "http://127.0.0.1:7890"
	g := &Gateway{
		cfg:        config.Config{ProxyRotateLinks: map[string]string{config.Fingerprint(linked): "https://p.example/rotate"}},
		transports: pool,
		logger:     testLogger(),
	}
	got := g.rotatingProxies()
	if len(got) != 1 || got[0].name != linked {
		t.Fatalf("rotatingProxies = %v", got)
	}
}

// With every proxy disabled the pool is empty. Requests must fail with the
// proxy error instead of silently going out through direct.
func TestEmptyPoolFailsWithProxyError(t *testing.T) {
	g := &Gateway{cfg: config.Config{}, transports: &transportPool{}, logger: testLogger()}
	_, _, err := g.doUpstream(t.Context(), models.Route{}, nil, identity.RequestIDs{Request: "r1", Session: "s1"})
	if err == nil {
		t.Fatal("empty proxy pool allowed a request through")
	}
	if !errors.Is(err, errProxyUnavailable) {
		t.Fatalf("error = %v, want errProxyUnavailable", err)
	}
}

// A key node whose pool has no transport must also report a proxy error rather
// than a bad gateway.
func TestKeyWithoutTransportReportsProxyError(t *testing.T) {
	pool, err := newNodePool([]string{"k-1234567890"}, &transportPool{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("newNodePool: %v", err)
	}
	if got := pool.Proxy(pool.nodes[0]); got != nil {
		t.Fatalf("empty transport pool returned a proxy: %v", got)
	}
}
