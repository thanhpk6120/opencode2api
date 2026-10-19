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
	"opencode2api/internal/identity"
	wire "opencode2api/internal/protocol"
)

// exitIPProbeURL returns the caller's exit address. It is the same endpoint the
// health checks use, so an extra dependency is not introduced for rotation.
const exitIPProbeURL = proxyHealthCheckURL

// rotateAttempts bounds the "rotate again because the new exit IP is already
// known-bad" loop. Each pass is spaced by the configured minimum interval.
const rotateAttempts = 5

// probeTimeout bounds one AI-model call used to decide whether a renewed exit
// actually works.
const probeTimeout = 60 * time.Second

// proxyRotator changes the exit IP of rotating proxies.
//
// The rotating proxies are tried first. When one fails an AI-model call it is
// taken out of service (refreshing) so the static proxies carry traffic, while
// the rotator renews its exit in the background and keeps calling a real model
// until one exit succeeds. Only then does the rotating proxy come back and the
// static entries return to being pure fallback.
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

// rotatingProxies lists the transports that can change their exit IP.
func (g *Gateway) rotatingProxies() []*proxyTransport {
	if g == nil || g.transports == nil {
		return nil
	}
	out := make([]*proxyTransport, 0, len(g.transports.items))
	for _, proxy := range g.transports.items {
		if proxy.isRotating() && g.cfg.ProxyRotateLink(config.Fingerprint(proxy.name)) != "" {
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
				g.rotator.rotateDue()
			}
		}
	}()
}

// observeRotatingFailure is called for every failed AI-model attempt. A failure
// on a rotating proxy suspends it immediately (so traffic falls back to the
// static proxies), remembers the failed exit range, and renews the exit in the
// background.
func (g *Gateway) observeRotatingFailure(proxy *proxyTransport) {
	if g == nil || !proxy.isRotating() || g.cfg.ProxyRotateLink(config.Fingerprint(proxy.name)) == "" {
		return
	}
	// Without auto_rotate the proxy is still suspended: retrying the same dead
	// exit would only burn attempts. Recovery then waits for the next forced
	// rotation instead.
	proxy.refreshing.Store(true)
	if !g.cfg.Rotating.AutoRotate {
		return
	}
	go func() {
		// Remember the exit range that failed before changing it, so a rotation
		// that lands back on it is treated as a bad draw.
		g.rotator.noteFailureRange(proxy)
		g.rotator.renew(proxy, "upstream_failure")
	}()
}

// rotateDue renews every rotating proxy whose last rotation is older than the
// maximum interval.
func (r *proxyRotator) rotateDue() {
	for _, proxy := range r.gateway.rotatingProxies() {
		if !r.due(proxy, time.Duration(r.cfg.MaxIntervalSeconds)*time.Second) {
			continue
		}
		// A forced rotation of a healthy proxy must not take it out of service:
		// only an entry suspended by a failed call is unavailable to begin with.
		go r.renew(proxy, "max_interval")
	}
}

func (r *proxyRotator) due(proxy *proxyTransport, interval time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.last[config.Fingerprint(proxy.name)]) >= interval
}

// renew runs the rotate link and then keeps calling a real model through the
// new exit until one attempt succeeds. The proxy is returned to service only
// when that probe passes; on failure it stays suspended so the static proxies
// keep serving.
func (r *proxyRotator) renew(proxy *proxyTransport, reason string) {
	id := config.Fingerprint(proxy.name)
	link := r.gateway.cfg.ProxyRotateLink(id)
	if link == "" {
		proxy.refreshing.Store(false)
		return
	}
	if !r.waitForSlot(proxy) {
		return
	}
	defer r.release(proxy)

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
			r.stamp(proxy)
			r.logger.Warn("proxy rotation failed", "component", "proxy", "event", "proxy_rotate_failed", "proxy", config.RedactURL(proxy.name), "reason", reason, "error", err)
			return
		}
		r.stamp(proxy)
		// A transport that keeps connections alive would send the probe (and the
		// next request) over the old tunnel to the old exit IP, making the whole
		// rotation invisible. #pool entries already disable keep-alives.
		proxy.client.CloseIdleConnections()
		// Only the health/cooldown state is cleared here. The renewal flag stays
		// set until the probe below proves the new exit can serve a model,
		// otherwise traffic would resume on an unverified exit.
		r.gateway.resetProxyHealth(proxy)

		rangeKey := r.probeExitRange(proxy)
		bad := false
		if rangeKey != "" {
			r.mu.Lock()
			r.ranges[id] = rangeKey
			bad = r.isBadLocked(rangeKey)
			r.mu.Unlock()
			r.logger.Info("proxy exit rotated", "component", "proxy", "event", "proxy_rotated", "proxy", config.RedactURL(proxy.name), "reason", reason, "status", status, "exit_range", rangeKey, "known_bad", bad)
			if bad {
				// Drawing a known-bad exit again is not worth an AI call.
				continue
			}
		} else {
			r.logger.Info("proxy exit address unreadable", "component", "proxy", "event", "proxy_exit_unreadable", "proxy", config.RedactURL(proxy.name), "reason", reason, "status", status)
		}

		ok, feasible := r.probeModelCall(proxy)
		switch {
		case ok:
			r.resume(proxy, reason, rangeKey)
			r.logger.Info("rotating proxy back in service", "component", "proxy", "event", "rotate_probe_succeeded", "proxy", config.RedactURL(proxy.name), "reason", reason, "exit_range", rangeKey, "attempt", attempt+1)
			return
		case feasible:
			r.logger.Warn("rotated exit still failed the model probe", "component", "proxy", "event", "rotate_probe_failed", "proxy", config.RedactURL(proxy.name), "reason", reason, "exit_range", rangeKey, "attempt", attempt+1)
		case rangeKey != "":
			// The exit address was readable, so the route itself works; there is
			// simply no model to probe it with. Accept the rotation rather than
			// looping on evidence that cannot exist.
			r.logger.Info("proxy exit rotated but cannot be probed", "component", "proxy", "event", "rotate_probe_skipped", "proxy", config.RedactURL(proxy.name), "reason", reason, "exit_range", rangeKey)
			r.resume(proxy, reason, rangeKey)
			return
		default:
			// Neither the exit address nor a model probe could be obtained: the
			// route is unproven. Keep the proxy suspended so the static fallback
			// serves, and let the next rotation try again.
			r.logger.Warn("proxy exit could not be verified; staying on fallback", "component", "proxy", "event", "proxy_unverified", "proxy", config.RedactURL(proxy.name), "reason", reason, "attempt", attempt+1)
			return
		}
	}
	r.logger.Warn("proxy kept landing on unusable exits; staying on the static fallback", "component", "proxy", "event", "proxy_rotate_exhausted", "proxy", config.RedactURL(proxy.name), "reason", reason)
}

// noteFailureRange records the exit range of a proxy that just failed an AI
// model call. It is separate from renew() so the failure is remembered even
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

// claim reserves a rotation slot for one proxy. It refuses while another
// rotation is running or before the minimum interval elapsed, which is what
// keeps a burst of failures from hammering the provider's rotate endpoint.
func (r *proxyRotator) claim(proxy *proxyTransport) bool {
	if proxy == nil {
		return false
	}
	id := config.Fingerprint(proxy.name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[id] {
		return false
	}
	if elapsed := time.Since(r.last[id]); elapsed < time.Duration(r.cfg.MinIntervalSeconds)*time.Second {
		return false
	}
	r.inflight[id] = true
	return true
}

// waitForSlot blocks until a rotation is allowed, or reports false when the
// proxy is already being renewed by another goroutine. Blocking here instead of
// dropping the request is what keeps a suspended proxy from waiting for the
// next forced rotation before it is retried.
func (r *proxyRotator) waitForSlot(proxy *proxyTransport) bool {
	if proxy == nil {
		return false
	}
	id := config.Fingerprint(proxy.name)
	r.mu.Lock()
	busy := r.inflight[id]
	wait := time.Duration(r.cfg.MinIntervalSeconds)*time.Second - time.Since(r.last[id])
	r.mu.Unlock()
	if busy {
		return false
	}
	if wait > 0 {
		time.Sleep(wait)
	}
	return r.claim(proxy)
}

func (r *proxyRotator) release(proxy *proxyTransport) {
	if proxy == nil {
		return
	}
	id := config.Fingerprint(proxy.name)
	r.mu.Lock()
	delete(r.inflight, id)
	r.mu.Unlock()
}

// stamp records that a rotation was attempted now, so a failing endpoint is
// throttled on the same minimum-interval schedule.
func (r *proxyRotator) stamp(proxy *proxyTransport) {
	if proxy == nil {
		return
	}
	id := config.Fingerprint(proxy.name)
	r.mu.Lock()
	r.last[id] = time.Now()
	r.mu.Unlock()
}

// resume returns a rotating proxy to service and records the exit it uses.
func (r *proxyRotator) resume(proxy *proxyTransport, reason, rangeKey string) {
	proxy.refreshing.Store(false)
	r.gateway.restoreProxy(proxy)
	r.logger.Info("rotating proxy back in service", "component", "proxy", "event", "proxy_resumed", "proxy", config.RedactURL(proxy.name), "reason", reason, "exit_range", rangeKey)
}

// probeModelCall performs a real, minimal AI-model call through the proxy. The
// second result reports whether probing is possible at all: an empty catalog or
// a disabled anonymous lane is "cannot probe", which must not be confused with
// "the new exit failed".
func (r *proxyRotator) probeModelCall(proxy *proxyTransport) (ok, feasible bool) {
	g := r.gateway
	if g == nil || proxy == nil || proxy.client == nil || g.catalog == nil {
		return false, false
	}
	// The probe rides the keyless lane, so it only exists when that lane does.
	if !g.cfg.Anonymous {
		return false, false
	}
	model := g.rotationProbeModel()
	if model == "" {
		return false, false
	}
	protocol := g.probeProtocol(model)
	if protocol == "" {
		return false, false
	}

	// Mirror the real anonymous lane: the legacy /zen/v1 gate first, then the
	// configured /inference origin. Probing only the legacy gate would report a
	// healthy exit as broken whenever that gate alone rejects the request.
	feasible = false
	for _, baseURL := range g.anonymousUpstreams() {
		body, err := probePayload(model, protocol, "", isLegacyZenUpstream(baseURL))
		if err != nil {
			continue
		}
		feasible = true
		if r.probeThrough(proxy, baseURL, model, protocol, body) {
			return true, true
		}
	}
	return false, feasible
}

// probeProtocol resolves the wire protocol used for the probe. It prefers the
// routed protocol but falls back to the catalog's native one, so a model that is
// merely marked unavailable does not make the probe impossible.
func (g *Gateway) probeProtocol(model string) wire.Protocol {
	if route, err := g.catalog.Route(model, g.zenNodes.Len() > 0, g.goNodes.Len() > 0, g.cfg.Anonymous); err == nil {
		if protocol := route.ProtocolFor(config.TierZen); protocol != "" {
			return protocol
		}
		return route.Protocol
	}
	diagnostic := g.catalog.Diagnostic(model, wire.Chat, g.zenNodes.Len() > 0, false, g.cfg.Anonymous)
	return diagnostic.NativeProtocols[config.TierZen]
}

// probeThrough performs one minimal model call through the proxy.
func (r *proxyRotator) probeThrough(proxy *proxyTransport, baseURL, model string, protocol wire.Protocol, body []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	ids := identity.RequestIDs{Request: identity.RandomID("req", 16), Session: identity.CanonicalSessionID("rotation:" + config.Fingerprint(proxy.name)), Project: "opencode2api:default-project"}
	req, err := newUpstreamRequest(ctx, baseURL, protocol, body, ids, anonymousZenKey)
	if err != nil {
		return false
	}
	req.Close = true
	resp, err := proxy.client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode/100 != 2 {
		return false
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") && protocol != wire.SystemOne {
		if _, err := wire.CollapseStream(resp.Body, protocol, model); err != nil {
			return false
		}
	}
	return true
}

// rotationProbeModel picks a free model already known to work with the public
// credential, so the probe measures the proxy rather than the model catalogue.
func (g *Gateway) rotationProbeModel() string {
	if g == nil || g.catalog == nil {
		return ""
	}
	models := g.catalog.List()
	for _, model := range models {
		if g.catalog.IsFreeModel(model) && g.catalog.EligibleForAnonymous(model) {
			return model
		}
	}
	// Every free model may be marked unavailable after a run of 403s, which says
	// nothing about a brand-new exit. Fall back to any free model, then to any
	// model at all, so an exit can still be verified.
	for _, model := range models {
		if g.catalog.IsFreeModel(model) {
			return model
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
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
