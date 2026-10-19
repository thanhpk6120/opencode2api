package gateway

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
)

// exitIPProbeURL returns the caller's exit address. It is the same endpoint the
// health checks use, so an extra dependency is not introduced for rotation.
const exitIPProbeURL = proxyHealthCheckURL

// rotateAttempts bounds the "rotate again because the new exit IP is already
// known-bad" loop. Each pass is spaced by the configured minimum interval.
const rotateAttempts = 5

// proxyRotator changes the exit IP of rotating proxies. It reacts to failed AI
// model calls, forces a rotation at the configured maximum interval, and skips
// exit ranges that already failed.
type proxyRotator struct {
	gateway *Gateway
	cfg     config.RotatingProxyConfig
	logger  *slog.Logger

	mu       sync.Mutex
	last     map[string]time.Time
	ranges   map[string]string // proxy id -> last observed exit range
	bad      []string          // FIFO of known-bad exit ranges, newest last
	inflight map[string]bool
}

func newProxyRotator(g *Gateway, cfg config.RotatingProxyConfig) *proxyRotator {
	return &proxyRotator{
		gateway:  g,
		cfg:      cfg,
		logger:   g.logger,
		last:     make(map[string]time.Time),
		ranges:   make(map[string]string),
		inflight: make(map[string]bool),
	}
}

// RotatingProxies lists the transports that can change their exit IP.
func (g *Gateway) rotatingProxies() []*proxyTransport {
	if g == nil || g.transports == nil {
		return nil
	}
	out := make([]*proxyTransport, 0, len(g.transports.items))
	for _, proxy := range g.transports.items {
		if g.cfg.ProxyRotateLink(config.Fingerprint(proxy.name)) != "" {
			out = append(out, proxy)
		}
	}
	return out
}

// StartProxyRotation forces a rotation once the maximum interval elapsed.
func (g *Gateway) StartProxyRotation(ctx context.Context) {
	if g == nil || g.rotator == nil || g.cfg.Rotating.MaxIntervalSeconds <= 0 {
		return
	}
	interval := time.Duration(g.cfg.Rotating.MaxIntervalSeconds) * time.Second
	// Checking at a fraction of the interval keeps the forced rotation close
	// to its deadline without spinning.
	tick := interval / 6
	if tick < time.Second {
		tick = time.Second
	}
	go func() {
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.rotator.rotateDue(ctx)
			}
		}
	}()
}

// ObserveFailure is called for every failed AI-model attempt. A failure on a
// rotating proxy marks the current exit range bad and, when the minimum
// interval allows it, rotates to a fresh exit.
func (g *Gateway) observeRotatingFailure(proxy *proxyTransport) {
	if g == nil || g.rotator == nil || proxy == nil || !g.cfg.Rotating.AutoRotate {
		return
	}
	if g.cfg.ProxyRotateLink(config.Fingerprint(proxy.name)) == "" {
		return
	}
	go func() {
		// Remember the exit range that failed before changing it, so a rotation
		// that lands back on it is treated as a bad draw.
		g.rotator.noteFailureRange(proxy)
		g.rotator.rotate(proxy, "upstream_failure")
	}()
}

// rotateDue rotates every rotating proxy whose last rotation is older than the
// maximum interval.
func (r *proxyRotator) rotateDue(ctx context.Context) {
	for _, proxy := range r.gateway.rotatingProxies() {
		if ctx.Err() != nil {
			return
		}
		if !r.due(proxy, time.Duration(r.cfg.MaxIntervalSeconds)*time.Second) {
			continue
		}
		go r.rotate(proxy, "max_interval")
	}
}

func (r *proxyRotator) due(proxy *proxyTransport, interval time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.last[config.Fingerprint(proxy.name)]) >= interval
}

// rotate runs the rotate link, then re-rotates while the new exit range is
// already known to be bad. Rotations are serialized per proxy, so a burst of
// failures produces a single rotation.
func (r *proxyRotator) rotate(proxy *proxyTransport, reason string) {
	id := config.Fingerprint(proxy.name)
	link := r.gateway.cfg.ProxyRotateLink(id)
	if link == "" {
		return
	}
	r.mu.Lock()
	if r.inflight[id] {
		r.mu.Unlock()
		return
	}
	if elapsed := time.Since(r.last[id]); elapsed < time.Duration(r.cfg.MinIntervalSeconds)*time.Second {
		r.mu.Unlock()
		return
	}
	r.inflight[id] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, id)
		r.mu.Unlock()
	}()

	for attempt := range rotateAttempts {
		if attempt > 0 {
			// A known-bad exit range forces another rotation, but never faster
			// than the minimum interval.
			time.Sleep(time.Duration(r.cfg.MinIntervalSeconds) * time.Second)
		}
		status, _, err := callRotateLink(context.Background(), link)
		if err != nil {
			// Stamp the attempt so a failing endpoint is retried on the same
			// minimum-interval schedule instead of once per failed request.
			r.mu.Lock()
			r.last[id] = time.Now()
			r.mu.Unlock()
			r.logger.Warn("proxy rotation failed", "component", "proxy", "event", "proxy_rotate_failed", "proxy", config.RedactURL(proxy.name), "reason", reason, "error", err)
			return
		}
		r.mu.Lock()
		r.last[id] = time.Now()
		r.mu.Unlock()
		r.gateway.resetProxyRoute(id)
		rangeKey := r.probeExitRange(proxy)
		if rangeKey == "" {
			r.logger.Info("proxy exit rotated", "component", "proxy", "event", "proxy_rotated", "proxy", config.RedactURL(proxy.name), "reason", reason, "status", status)
			return
		}
		r.mu.Lock()
		r.ranges[id] = rangeKey
		bad := r.isBadLocked(rangeKey)
		r.mu.Unlock()
		r.logger.Info("proxy exit rotated", "component", "proxy", "event", "proxy_rotated", "proxy", config.RedactURL(proxy.name), "reason", reason, "status", status, "exit_range", rangeKey, "known_bad", bad)
		if !bad {
			return
		}
	}
	r.logger.Warn("proxy kept landing on known-bad exit ranges", "component", "proxy", "event", "proxy_rotate_exhausted", "proxy", config.RedactURL(proxy.name), "reason", reason)
}

// noteFailureRange records the exit range of a proxy that just failed an AI
// model call. It is separate from rotate() so the failure is remembered even
// when the minimum interval blocks an immediate rotation.
func (r *proxyRotator) noteFailureRange(proxy *proxyTransport) {
	id := config.Fingerprint(proxy.name)
	r.mu.Lock()
	rangeKey := r.ranges[id]
	r.mu.Unlock()
	if rangeKey == "" {
		rangeKey = r.probeExitRange(proxy)
		if rangeKey == "" {
			return
		}
		r.mu.Lock()
		r.ranges[id] = rangeKey
		r.mu.Unlock()
	}
	r.markBad(rangeKey)
}

// markBad appends a range to the bounded cache, dropping the oldest entry.
func (r *proxyRotator) markBad(rangeKey string) {
	limit := r.cfg.FailedIPCacheSize
	if limit < 1 {
		limit = config.DefaultRotateFailedIPCacheSize
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.bad {
		if existing == rangeKey {
			return
		}
	}
	r.bad = append(r.bad, rangeKey)
	if len(r.bad) > limit {
		r.bad = r.bad[len(r.bad)-limit:]
	}
}

// isBad reports whether a range is in the failure cache.
func (r *proxyRotator) isBad(rangeKey string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isBadLocked(rangeKey)
}

func (r *proxyRotator) isBadLocked(rangeKey string) bool {
	for _, existing := range r.bad {
		if existing == rangeKey {
			return true
		}
	}
	return false
}

// probeExitRange reads the current exit address through the proxy and reduces
// it to a network range: /24 for IPv4 and /48 for IPv6.
func (r *proxyRotator) probeExitRange(proxy *proxyTransport) string {
	if proxy == nil || proxy.client == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exitIPProbeURL, nil)
	if err != nil {
		return ""
	}
	resp, err := proxy.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "ip=") {
			continue
		}
		return exitRange(strings.TrimSpace(strings.TrimPrefix(line, "ip=")))
	}
	return ""
}

// exitRange maps an address to the range the failure cache tracks. Anything
// unparsable is returned verbatim so it still participates in the cache.
func exitRange(address string) string {
	ip := net.ParseIP(address)
	if ip == nil {
		return address
	}
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.0/24", v4[0], v4[1], v4[2])
	}
	v6 := ip.To16()
	if v6 == nil {
		return address
	}
	return fmt.Sprintf("%x:%x:%x::/48", uint16(v6[0])<<8|uint16(v6[1]), uint16(v6[2])<<8|uint16(v6[3]), uint16(v6[4])<<8|uint16(v6[5]))
}
