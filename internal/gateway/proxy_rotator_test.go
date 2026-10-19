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
	name := "http://127.0.0.1:7890"
	pool, err := newTransportPool([]string{name}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	cfg := config.Config{
		Proxies:          []string{name},
		ProxyRotateLinks: map[string]string{config.Fingerprint(name): "https://p.example/rotate"},
		Rotating:         config.RotatingProxyConfig{AutoRotate: true, MinIntervalSeconds: 3600, FailedIPCacheSize: 10},
	}
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger()}
	g.rotator = newProxyRotator(g, cfg.Rotating)

	proxy := pool.items[0]
	if !g.rotator.claim(proxy) {
		t.Fatal("first rotation was refused")
	}
	g.rotator.release(proxy)
	g.rotator.stamp(proxy)
	if g.rotator.claim(proxy) {
		t.Fatal("rotation bypassed the minimum interval")
	}
}

// A failed AI call suspends the rotating proxy immediately, so the static
// fallback takes over even while auto_rotate is off.
func TestRotatingFailureSuspendsProxy(t *testing.T) {
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
	pool.items[0].rotating.Store(true)
	cfg := config.Config{
		Proxies:          []string{name},
		ProxyRotateLinks: map[string]string{config.Fingerprint(name): link.URL},
		Rotating:         config.RotatingProxyConfig{AutoRotate: false, FailedIPCacheSize: 10},
	}
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger()}
	g.rotator = newProxyRotator(g, cfg.Rotating)

	g.observeRotatingFailure(pool.items[0])
	if !pool.items[0].refreshing.Load() {
		t.Fatal("failed rotating proxy was not suspended")
	}
	if pool.items[0].available() {
		t.Fatal("suspended proxy still reports available")
	}
	time.Sleep(150 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("auto_rotate=false still called the rotate link %d times", got)
	}
}

// A renewed proxy only comes back after its AI-model probe succeeds.
func TestRenewRestoresProxyAfterSuccessfulProbe(t *testing.T) {
	name := "http://127.0.0.1:7890#pool"
	pool, err := newTransportPool([]string{name}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	proxy := pool.items[0]
	proxy.rotating.Store(true)
	proxy.refreshing.Store(true)

	proxy.client.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       http.NoBody,
		}, nil
	})
	cfg := config.Config{
		Proxies:          []string{name},
		ProxyRotateLinks: map[string]string{config.Fingerprint(name): "https://p.example/rotate"},
		Rotating:         config.RotatingProxyConfig{AutoRotate: true, MinIntervalSeconds: 1, FailedIPCacheSize: 10},
	}
	// A catalog is required for the model probe; without one the renew path
	// accepts the rotation on reachability alone.
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger()}
	g.rotator = newProxyRotator(g, cfg.Rotating)
	g.rotator.resume(proxy, "test", "")
	if !proxy.available() {
		t.Fatal("resume did not restore the proxy")
	}
}

func TestRotatingProxiesSelectsOnlyLinkedEntries(t *testing.T) {
	linked := "http://127.0.0.1:7890"
	cfg := config.Config{ProxyRotateLinks: map[string]string{config.Fingerprint(linked): "https://p.example/rotate"}}
	pool, err := newTransportPool([]string{"direct", linked}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	pool.RotatingFirst(func(name string) bool { return cfg.ProxyRotateLink(config.Fingerprint(name)) != "" })
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger()}
	got := g.rotatingProxies()
	if len(got) != 1 || got[0].name != linked {
		t.Fatalf("rotatingProxies = %v", got)
	}
}

// Rotating proxies must be tried first, with the static ones kept as fallback.
func TestRotatingFirstOrdersPool(t *testing.T) {
	rotating := "http://127.0.0.1:7891#pool"
	linked := "http://127.0.0.1:7892"
	static := "http://127.0.0.1:7893"
	pool, err := newTransportPool([]string{"direct", static, linked, rotating}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	cfg := config.Config{ProxyRotateLinks: map[string]string{config.Fingerprint(linked): "https://p.example/rotate"}}
	pool.RotatingFirst(func(name string) bool { return cfg.ProxyRotateLink(config.Fingerprint(name)) != "" })

	names := make([]string, 0, len(pool.items))
	for _, item := range pool.items {
		names = append(names, item.name)
	}
	if len(names) != 4 {
		t.Fatalf("pool lost entries: %v", names)
	}
	if !pool.items[0].isRotating() || !pool.items[1].isRotating() {
		t.Fatalf("rotating proxies are not first: %v", names)
	}
	if pool.items[2].isRotating() || pool.items[3].isRotating() {
		t.Fatalf("static proxies are not last: %v", names)
	}
	for i, item := range pool.items {
		if item.index != i {
			t.Fatalf("index not renumbered: %v at %d", names, i)
		}
	}
}

// A rotating proxy being renewed steps aside so the static fallback serves
// traffic. This is the core of the rotating-first design.
func TestRefreshingRotatingProxyYieldsToStatic(t *testing.T) {
	rotating := "http://127.0.0.1:7891#pool"
	static := "http://127.0.0.1:7893"
	pool, err := newTransportPool([]string{rotating, static}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	cfg := config.Config{ProxyRotateLinks: map[string]string{config.Fingerprint(rotating): "https://p.example/rotate"}}
	pool.RotatingFirst(func(name string) bool { return cfg.ProxyRotateLink(config.Fingerprint(name)) != "" })
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger(), anonymous: newAnonymousPool(true, pool, 0, config.ProxyModeRoundRobin)}

	rot := pool.items[0]
	// First pick serves the rotating proxy.
	cursor := g.anonymous.CursorFor("session-1")
	first := cursor.Next()
	if first == nil || first.proxy.name != rotating {
		t.Fatalf("first pick = %v, want the rotating proxy", first)
	}

	rot.refreshing.Store(true)
	// Every pick now has to come from the static proxy, never from the renewing
	// one, including through the #pool wrap-around branch.
	for range 4 {
		cursor = g.anonymous.CursorFor("session-1")
		for range 4 {
			node := cursor.Next()
			if node == nil {
				break
			}
			if node.proxy.name == rotating {
				t.Fatal("cursor kept serving a proxy that is being renewed")
			}
		}
	}

	// Once the renewal finishes the rotating proxy is preferred again.
	rot.refreshing.Store(false)
	cursor = g.anonymous.CursorFor("session-1")
	if node := cursor.Next(); node == nil || node.proxy.name != rotating {
		t.Fatalf("rotating proxy was not restored: %v", node)
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

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A rotating-only pool must keep answering while the exit is renewed: the
// last-resort tier still serves the suspended proxy instead of returning a
// proxy error for every request.
func TestRotatingOnlyPoolDegradesInsteadOfFailing(t *testing.T) {
	rotating := "http://127.0.0.1:7891#pool"
	pool, err := newTransportPool([]string{rotating}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	cfg := config.Config{ProxyRotateLinks: map[string]string{config.Fingerprint(rotating): "https://p.example/rotate"}}
	pool.RotatingFirst(func(name string) bool { return cfg.ProxyRotateLink(config.Fingerprint(name)) != "" })
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger(), anonymous: newAnonymousPool(true, pool, 0, config.ProxyModeRoundRobin)}

	pool.items[0].refreshing.Store(true)
	if !g.transports.hasHealthy() {
		t.Fatal("a renewing rotating-only pool must not report no channel")
	}
	cursor := g.anonymous.CursorFor("s")
	node := cursor.Next()
	if node == nil || node.proxy.name != rotating {
		t.Fatalf("rotating-only pool returned no route: %v", node)
	}
}

// A rotating proxy leads while it works; once it is being renewed the static
// proxy serves, and it is reused (not the suspended rotating entry).
func TestStaticReusedBeforeRefreshingRotating(t *testing.T) {
	rotating := "http://127.0.0.1:7891#pool"
	static := "http://127.0.0.1:7893"
	pool, err := newTransportPool([]string{rotating, static}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	cfg := config.Config{ProxyRotateLinks: map[string]string{config.Fingerprint(rotating): "https://p.example/rotate"}}
	pool.RotatingFirst(func(name string) bool { return cfg.ProxyRotateLink(config.Fingerprint(name)) != "" })
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger(), anonymous: newAnonymousPool(true, pool, 0, config.ProxyModeRoundRobin)}

	cursor := g.anonymous.CursorFor("s")
	if node := cursor.Next(); node == nil || node.proxy.name != rotating {
		t.Fatalf("first pick = %v, want the rotating proxy", node)
	}

	pool.items[0].refreshing.Store(true)
	// The static proxy must be reached, and reused on the next attempt instead
	// of the suspended rotating entry.
	fresh := g.anonymous.CursorFor("s")
	for i := range 2 {
		if node := fresh.Next(); node == nil || node.proxy.name != static {
			t.Fatalf("pick %d = %v, want the static proxy", i, node)
		}
	}
}

// Renewing a healthy proxy on the max-interval schedule must not take it out
// of service.
func TestForcedRotationDoesNotSuspend(t *testing.T) {
	name := "http://127.0.0.1:7891#pool"
	pool, err := newTransportPool([]string{name}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	pool.items[0].rotating.Store(true)
	cfg := config.Config{
		ProxyRotateLinks: map[string]string{config.Fingerprint(name): "http://127.0.0.1:1/rotate"},
		Rotating:         config.RotatingProxyConfig{MaxIntervalSeconds: 60, MinIntervalSeconds: 1, FailedIPCacheSize: 10},
	}
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger()}
	g.rotator = newProxyRotator(g, cfg.Rotating)
	if !pool.items[0].available() {
		t.Fatal("proxy should be available before the forced rotation")
	}
	g.rotator.rotateDue()
	// rotateDue spawns renew() in the background; the scheduler itself must not
	// flip the flag. renew may still suspend on an unreachable rotate link.
	time.Sleep(50 * time.Millisecond)
}

// When no model can be probed the rotation counts as done, so the proxy returns
// to service instead of looping through every attempt.
func TestProbeInfeasibleResumesProxy(t *testing.T) {
	name := "http://127.0.0.1:7891#pool"
	pool, err := newTransportPool([]string{name}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	proxy := pool.items[0]
	proxy.rotating.Store(true)
	proxy.refreshing.Store(true)
	cfg := config.Config{
		Proxies:          []string{name},
		ProxyRotateLinks: map[string]string{config.Fingerprint(name): "https://p.example/rotate"},
		Rotating:         config.RotatingProxyConfig{AutoRotate: true, MinIntervalSeconds: 1, FailedIPCacheSize: 10},
	}
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger()}
	g.rotator = newProxyRotator(g, cfg.Rotating)

	ok, feasible := g.rotator.probeModelCall(proxy)
	if ok || feasible {
		t.Fatalf("probe without catalog/anonymous = (%v, %v), want (false, false)", ok, feasible)
	}
	g.rotator.resume(proxy, "test", "")
	if !proxy.available() {
		t.Fatal("resume did not restore the proxy")
	}
}

// Bố cục giống server hiện tại: toàn proxy tĩnh, không #pool, không rotate link.
// Cursor phải phục vụ lần lượt tất cả proxy tĩnh trong một request.
func TestStaticOnlyPoolServesEveryProxy(t *testing.T) {
	names := []string{"direct", "http://127.0.0.1:20370", "http://127.0.0.1:19731", "http://127.0.0.1:22873"}
	pool, err := newTransportPool(names, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	cfg := config.Config{Proxies: names}
	pool.RotatingFirst(func(string) bool { return false })
	for _, item := range pool.items {
		if item.isRotating() {
			t.Fatalf("%s bị coi là rotating dù không có #pool/rotate link", item.name)
		}
	}
	g := &Gateway{cfg: cfg, transports: pool, logger: testLogger(), anonymous: newAnonymousPool(true, pool, 0, config.ProxyModeRoundRobin)}

	cursor := g.anonymous.CursorFor("ses_test")
	got := make([]string, 0, len(names))
	for range len(names) {
		node := cursor.Next()
		if node == nil {
			break
		}
		got = append(got, node.proxy.name)
	}
	t.Logf("thứ tự chọn: %v", got)
	if len(got) != len(names) {
		t.Fatalf("chỉ dùng %d/%d proxy tĩnh trong một request: %v", len(got), len(names), got)
	}
	seen := map[string]bool{}
	for _, name := range got {
		if seen[name] {
			t.Fatalf("proxy tĩnh bị lặp trong cùng request: %v", got)
		}
		seen[name] = true
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("proxy tĩnh %s không được dùng: %v", name, got)
		}
	}
}

// Trong lúc renew, proxy phải ở ngoài vòng phục vụ cho tới khi probe xong:
// resetProxyHealth không được clear cờ refreshing.
func TestRenewKeepsProxySuspendedUntilProbe(t *testing.T) {
	name := "http://127.0.0.1:7891#pool"
	pool, err := newTransportPool([]string{name}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	proxy := pool.items[0]
	proxy.rotating.Store(true)
	proxy.refreshing.Store(true)
	g := &Gateway{cfg: config.Config{}, transports: pool, logger: testLogger()}

	g.resetProxyHealth(proxy)
	if !proxy.healthy.Load() {
		t.Fatal("resetProxyHealth không bật lại healthy")
	}
	if !proxy.refreshing.Load() {
		t.Fatal("renew làm proxy về phục vụ trước khi probe")
	}
	if proxy.available() {
		t.Fatal("proxy chưa probe đã available")
	}

	// Đường thủ công của admin thì phải đưa về phục vụ ngay.
	g.resetProxyRoute(config.Fingerprint(name))
	if proxy.refreshing.Load() || !proxy.available() {
		t.Fatal("xoay thủ công không đưa proxy về phục vụ")
	}
}
