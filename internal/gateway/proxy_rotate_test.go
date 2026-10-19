package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"opencode2api/internal/config"
)

func TestCallRotateLinkStatusHandling(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("changed"))
	}))
	defer ok.Close()
	status, detail, err := callRotateLink(context.Background(), ok.URL)
	if err != nil || status != http.StatusOK || detail != "changed" {
		t.Fatalf("callRotateLink(ok) = (%d, %q, %v)", status, detail, err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer bad.Close()
	if _, _, err := callRotateLink(context.Background(), bad.URL); err == nil {
		t.Fatal("a 429 rotate response was reported as success")
	}

	if _, _, err := callRotateLink(context.Background(), "file:///etc/shadow"); err == nil {
		t.Fatal("a non-http rotate link produced a request")
	}
}

// Rotating the exit IP must return the transport to service: an evicted proxy
// would otherwise keep skipping traffic until the next scheduled health check.
func TestResetProxyRouteClearsUnhealthy(t *testing.T) {
	pool, err := newTransportPool([]string{"http://127.0.0.1:7890"}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("build transport pool: %v", err)
	}
	g := &Gateway{transports: pool, logger: testLogger()}
	target := pool.items[0]
	target.healthy.Store(false)

	g.resetProxyRoute(config.Fingerprint(target.name))
	if !target.healthy.Load() {
		t.Fatal("resetProxyRoute left the rotated proxy unhealthy")
	}

	// An unknown id must not touch unrelated transports.
	target.healthy.Store(false)
	g.resetProxyRoute("does-not-exist")
	if target.healthy.Load() {
		t.Fatal("resetProxyRoute affected a proxy with a different fingerprint")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
