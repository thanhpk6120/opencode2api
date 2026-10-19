package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	rand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

type proxyTransport struct {
	index    int
	name     string
	pool     bool
	client   *http.Client
	healthy  atomic.Bool
	checking atomic.Bool
	// rotating marks a proxy whose exit IP can be changed on demand (a #pool
	// entry or one with a rotate link). Rotating proxies are tried first so the
	// static ones stay available as the fallback while an exit is renewed.
	rotating atomic.Bool
	// refreshing is set while a rotating proxy is being renewed. Traffic skips
	// it and falls back to the static proxies until a working exit is found.
	refreshing atomic.Bool
}

// isRotating reports whether this proxy can change its exit IP.
func (p *proxyTransport) isRotating() bool { return p != nil && p.rotating.Load() }

// available reports whether traffic may use this proxy right now.
func (p *proxyTransport) available() bool {
	return p != nil && p.healthy.Load() && !p.refreshing.Load()
}

type transportPool struct {
	items []*proxyTransport
	// mode selects how request cursors pick their starting proxy:
	// sticky pins every request to one proxy until it fails, roundrobin
	// walks proxies in order, random picks a fresh start per request.
	mode string
	// sticky holds the pinned proxy index for sticky mode. It advances to
	// the next healthy proxy when the pinned one fails.
	sticky atomic.Uint64
	roundRobin atomic.Uint64
}

// anonymousPool gives the shared OpenCode "public" credential an independent
// cooldown per proxy. Unlike key nodes, anonymous nodes are never rebound:
// changing proxy is the failover mechanism because Zen rate-limits them by IP.
type anonymousPool struct {
	nodes      []*anonymousNode
	next       atomic.Uint64
	cooldown   time.Duration
	mode       string
	transports *transportPool
	hasPool    bool
}

type anonymousNode struct {
	proxy         *proxyTransport
	failures      atomic.Uint32
	cooldownUntil atomic.Int64
}

type anonymousCursor struct {
	pool   *anonymousPool
	start  int
	offset int
	used   []bool
}

func newAnonymousPool(enabled bool, transports *transportPool, cooldown time.Duration, mode string) *anonymousPool {
	if mode == "" {
		mode = config.ProxyModeRoundRobin
	}
	pool := &anonymousPool{cooldown: cooldown, mode: mode, transports: transports}
	if !enabled || transports == nil {
		return pool
	}
	pool.nodes = make([]*anonymousNode, 0, len(transports.items))
	for _, proxy := range transports.items {
		if proxy.pool {
			pool.hasPool = true
		}
		pool.nodes = append(pool.nodes, &anonymousNode{proxy: proxy})
	}
	return pool
}

func (p *anonymousPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.nodes)
}

func (p *anonymousPool) HasPool() bool {
	return p != nil && p.hasPool
}

func (p *anonymousPool) CursorFor(affinity string) anonymousCursor {
	if p == nil || len(p.nodes) == 0 {
		return anonymousCursor{pool: p}
	}
	if p.mode == config.ProxyModeRandom {
		return anonymousCursor{pool: p, start: rand.IntN(len(p.nodes))}
	}
	if p.mode == config.ProxyModeSticky && len(p.nodes) > 0 {
		pinned := p.pinnedIndex()
		if pinned >= 0 && pinned < len(p.nodes) {
			return anonymousCursor{pool: p, start: pinned}
		}
	}
	start := 0
	if affinity == "" {
		start = int((p.next.Add(1) - 1) % uint64(len(p.nodes)))
	} else {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(affinity))
		start = int(hash.Sum64() % uint64(len(p.nodes)))
	}
	return anonymousCursor{pool: p, start: start}
}

// pinnedIndex returns the sticky proxy position mapped onto the anonymous
// node list (nodes mirror transports 1:1 in construction order), advancing
// past unhealthy proxies, or -1 when none is healthy.
func (p *anonymousPool) pinnedIndex() int {
	if p == nil || len(p.nodes) == 0 || p.transports == nil {
		return -1
	}
	pinned := int(p.transports.sticky.Load() % uint64(len(p.nodes)))
	for offset := 0; offset < len(p.nodes); offset++ {
		idx := (pinned + offset) % len(p.nodes)
		if p.nodes[idx].proxy.available() {
			return idx
		}
	}
	return -1
}

// Next visits each healthy, non-cooling static proxy at most once per cursor.
// When the pool contains rotating proxy-pool transports (#pool), it continues
// cycling through them after the first pass so retry.max_attempts can draw a
// fresh upstream connection (and new exit IP) on every attempt. If all static
// proxies are currently cooling or marked unhealthy, Next falls back to the
// earliest-expiring node not yet tried by this cursor instead of returning nil
// immediately.
func (c *anonymousCursor) Next() *anonymousNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	n := len(c.pool.nodes)
	if c.used == nil {
		c.used = make([]bool, n)
	}
	now := time.Now().UnixNano()

	// Pass 0 takes rotating proxies, pass 1 takes the static ones. Both scan the
	// whole ring from the cursor start, so a session hash cannot pin the cursor
	// onto the fallback while a healthy rotating exit exists.
	for pass := 0; pass < 2; pass++ {
		for step := 0; step < n; step++ {
			idx := (c.start + step) % n
			node := c.pool.nodes[idx]
			if c.used[idx] || !node.proxy.available() {
				continue
			}
			if node.proxy.isRotating() != (pass == 0) {
				continue
			}
			if !node.proxy.pool && node.cooldownUntil.Load() > now {
				continue
			}
			c.used[idx] = true
			return node
		}
	}

	// Rotating pool entries may be reused: each attempt opens a fresh
	// connection and therefore a new exit IP.
	if c.pool.hasPool {
		for step := 0; step < n; step++ {
			node := c.pool.nodes[(c.start+step)%n]
			if node.proxy.pool && node.proxy.available() {
				return node
			}
		}
	}

	// Tier 1 revisits entries already used in this cursor, but never one that is
	// being renewed: reusing a healthy static exit beats retrying a dead one.
	// Tier 2 only runs when nothing else exists, so a rotating-only pool degrades
	// to slow requests instead of answering every request with a proxy error.
	for _, allowRefreshing := range []bool{false, true} {
		choice, earliest := -1, int64(0)
		for step := 0; step < n; step++ {
			idx := (c.start + step) % n
			node := c.pool.nodes[idx]
			if node.proxy.refreshing.Load() != allowRefreshing {
				continue
			}
			if !allowRefreshing && !node.proxy.healthy.Load() {
				continue
			}
			until := node.cooldownUntil.Load()
			if choice < 0 || until < earliest {
				choice, earliest = idx, until
			}
		}
		if choice >= 0 {
			c.used[choice] = true
			return c.pool.nodes[choice]
		}
	}
	return nil
}

func (p *anonymousPool) MarkSuccess(node *anonymousNode) {
	if node == nil {
		return
	}
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

// RestoreProxy clears cooldowns for anonymous nodes bound to a recovered
// proxy, mirroring nodePool.RestoreProxy for key nodes. Without this, an
// outage-era cooldown survives the proxy's recovery and the anonymous lane
// stays skipped even though the route is healthy again.
func (p *anonymousPool) RestoreProxy(proxy *proxyTransport) int {
	if p == nil || proxy == nil {
		return 0
	}
	cleared := 0
	for _, node := range p.nodes {
		if node.proxy != proxy {
			continue
		}
		if node.cooldownUntil.Load() == 0 && node.failures.Load() == 0 {
			continue
		}
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		cleared++
	}
	return cleared
}

// maxAnonymousRetryAfterMultiplier bounds an upstream Retry-After on the
// anonymous lane to the same 8x ceiling as its exponential backoff. The lane
// is rate limited per exit IP and, unlike key lanes, has no earliest-expiry
// fallback: a quota response asking for hours would keep the exit skipped long
// after a network change gave it a fresh allowance. Key lanes keep honoring
// Retry-After in full, since their cursor already falls back to the node that
// recovers first.
const maxAnonymousRetryAfterMultiplier = 8

func (p *anonymousPool) MarkFailure(node *anonymousNode, resp *http.Response, err error) {
	if node == nil || (node.proxy != nil && node.proxy.pool) {
		return
	}
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	delay := p.cooldown * time.Duration(1<<min(failures-1, 3))
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = min(retryAfter, p.cooldown*maxAnonymousRetryAfterMultiplier)
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

// RotatingFirst orders the pool so rotating proxies come first and static
// proxies last. Selection (roundrobin, random, sticky, cursors) then naturally
// prefers a rotating exit and keeps the static entries for fallback.
func (p *transportPool) RotatingFirst(isRotating func(name string) bool) {
	if p == nil {
		return
	}
	rotating := make([]*proxyTransport, 0, len(p.items))
	static := make([]*proxyTransport, 0, len(p.items))
	for _, item := range p.items {
		if item.pool || isRotating(item.name) {
			item.rotating.Store(true)
			rotating = append(rotating, item)
			continue
		}
		static = append(static, item)
	}
	ordered := append(rotating, static...)
	for i, item := range ordered {
		item.index = i
	}
	p.items = ordered
}

// pickPreferred scans the pool from start, taking rotating proxies first and
// the static ones only as fallback. It is shared by every selection mode so the
// key lane prefers rotating exits exactly like the anonymous cursor does.
func (p *transportPool) pickPreferred(start int) *proxyTransport {
	if p == nil || len(p.items) == 0 {
		return nil
	}
	n := len(p.items)
	for pass := 0; pass < 2; pass++ {
		for offset := 0; offset < n; offset++ {
			candidate := p.items[((start+offset)%n+n)%n]
			if !candidate.available() || candidate.isRotating() != (pass == 0) {
				continue
			}
			return candidate
		}
	}
	return nil
}

// stickyProxy returns the pinned proxy for sticky mode, skipping to the
// next available one when the pinned proxy is down. It never marks anything:
// health is owned by syncProxyResult/CheckHealth.
func (p *transportPool) stickyProxy() *proxyTransport {
	if p == nil || len(p.items) == 0 {
		return nil
	}
	pinned := int(p.sticky.Load() % uint64(len(p.items)))
	if candidate := p.pickPreferred(pinned); candidate != nil {
		return candidate
	}
	// Nothing is available: keep the pinned proxy so the request still reaches
	// one route instead of failing with no proxy at all.
	return p.items[pinned]
}

// advanceSticky moves the pinned proxy forward after a sticky failure so the
// next request starts on the following proxy.
func (p *transportPool) advanceSticky(failed *proxyTransport) {
	if p == nil || len(p.items) < 2 {
		return
	}
	if failed != nil {
		for i, item := range p.items {
			if item == failed {
				p.sticky.Store(uint64((i + 1) % len(p.items)))
				return
			}
		}
	}
	p.sticky.Add(1)
}

func (p *transportPool) pickRoundRobinHealthy() *proxyTransport {
	if p == nil || len(p.items) == 0 {
		return nil
	}
	start := int(p.roundRobin.Add(1) - 1)
	if candidate := p.pickPreferred(start); candidate != nil {
		return candidate
	}
	return p.items[((start%len(p.items))+len(p.items))%len(p.items)]
}

func (p *transportPool) pickRandomHealthy() *proxyTransport {
	if p == nil || len(p.items) == 0 {
		return nil
	}
	start := rand.IntN(len(p.items))
	if candidate := p.pickPreferred(start); candidate != nil {
		return candidate
	}
	return p.items[start]
}

// hasHealthy reports whether the pool still has a usable route. It is used to
// choose between "a proxy error" and "no channel at all", so it stays true only
// when some proxy can serve now, or when one is being renewed: a renewal is a
// temporary suspension and the entry remains the only route for a
// rotating-only pool. A pool whose every proxy is unhealthy reports false, so
// the client still receives the proxy error instead of a generic bad gateway.
func (p *transportPool) hasHealthy() bool {
	if p == nil || len(p.items) == 0 {
		return false
	}
	for _, proxy := range p.items {
		if proxy.available() || proxy.refreshing.Load() {
			return true
		}
	}
	return false
}

func (p *transportPool) healthCounts() (total, healthy int) {
	if p == nil {
		return 0, 0
	}
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			healthy++
		}
	}
	return len(p.items), healthy
}

func newTransportPool(proxies []string, cfg config.PerformanceConfig, responseHeaderTimeout time.Duration, mode string) (*transportPool, error) {
	if mode == "" {
		mode = config.ProxyModeRoundRobin
	}
	p := &transportPool{items: make([]*proxyTransport, 0, len(proxies)), mode: mode}
	for _, raw := range proxies {
		clean, isPool := config.ParseProxyEntry(raw)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = cfg.MaxIdleConns
		transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
		transport.MaxConnsPerHost = cfg.MaxConnsPerHost
		transport.IdleConnTimeout = time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		transport.ForceAttemptHTTP2 = !isPool
		transport.DisableKeepAlives = isPool
		if isPool {
			// Cloning DefaultTransport after its first dial inherits the
			// already-configured HTTP/2 ALPN and TLSNextProto map, which ignores
			// DisableKeepAlives and req.Close and multiplexes every request over
			// one CONNECT tunnel. An explicit empty TLSNextProto map and HTTP/1.1
			// ALPN force a brand-new TCP + CONNECT tunnel per attempt.
			transport.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
			} else {
				tlsCfg := transport.TLSClientConfig.Clone()
				tlsCfg.NextProtos = []string{"http/1.1"}
				transport.TLSClientConfig = tlsCfg
			}
		}
		transport.DialContext = (&net.Dialer{
			Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		if clean == "direct" {
			transport.Proxy = nil
		} else {
			u, err := url.Parse(clean)
			if err != nil {
				return nil, fmt.Errorf("parse proxy %s: %w", config.RedactURL(raw), err)
			}
			transport.Proxy = http.ProxyURL(u)
		}
		proxy := &proxyTransport{index: len(p.items), name: raw, pool: isPool, client: &http.Client{Transport: transport}}
		proxy.healthy.Store(true)
		p.items = append(p.items, proxy)
	}
	return p, nil
}

type proxyHealthResult struct {
	proxy      *proxyTransport
	err        error
	failed     bool
	wasHealthy bool
}

// CheckHealth concurrently rechecks only proxies already marked unhealthy.
// Healthy proxies are skipped before a check is claimed. Any HTTP response
// from the test URL proves that the route is reachable; only a timeout or
// connection refusal keeps the proxy unhealthy.
func (p *transportPool) CheckHealth(ctx context.Context, target string, timeout time.Duration) []proxyHealthResult {
	results := make(chan proxyHealthResult, len(p.items))
	checks := 0
	for _, proxy := range p.items {
		if proxy.pool || proxy.healthy.Load() || !proxy.checking.CompareAndSwap(false, true) {
			continue
		}
		// A real request may have restored the proxy between the first health
		// read and claiming this check.
		if proxy.healthy.Load() {
			proxy.checking.Store(false)
			continue
		}
		checks++
		go func() {
			results <- p.checkClaimedProxy(ctx, proxy, target, timeout)
		}()
	}
	out := make([]proxyHealthResult, 0, checks)
	for range checks {
		out = append(out, <-results)
	}
	return out
}

// checkClaimedProxy performs a check after the caller has acquired checking.
func (p *transportPool) checkClaimedProxy(ctx context.Context, proxy *proxyTransport, target string, timeout time.Duration) proxyHealthResult {
	defer proxy.checking.Store(false)
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("User-Agent", httpx.UserAgent())
		resp, requestErr := proxy.client.Do(req)
		err = requestErr
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		}
	}
	result := proxyHealthResult{proxy: proxy, err: err, wasHealthy: proxy.healthy.Load()}
	if err == nil {
		result.wasHealthy = proxy.healthy.Swap(true)
	} else if isProxyFailure(err) {
		result.failed = true
		result.wasHealthy = proxy.healthy.Swap(false)
	}
	return result
}

// isProxyFailure deliberately recognizes only failures that say the proxy
// route is unavailable. HTTP responses and unrelated transport/protocol errors
// must not evict a proxy.
func isProxyFailure(err error) bool {
	if errors.Is(err, errFirstEventTimeout) || upstreamAccepted(err) {
		return false // The upstream accepted HTTP; slow generation is not a broken proxy.
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// upstreamNode keeps a key stable while allowing its proxy binding to change
// atomically when a proxy becomes unavailable.
type upstreamNode struct {
	key            string
	index          int
	preferredProxy int
	proxyIndex     atomic.Int64
	failures       atomic.Uint32
	cooldownUntil  atomic.Int64
}

type nodePool struct {
	nodes        []*upstreamNode
	transports   *transportPool
	next         atomic.Uint64
	cooldown     time.Duration
	bindingsMu   sync.Mutex
	bindingCount []int
	mode         string
}

// newNodePool distributes keys over proxies in round-robin order. When there
// are fewer keys than proxies, the remaining proxies are intentionally idle and
// can take over a key immediately if an active proxy fails.
func newNodePool(keys []string, transports *transportPool, cooldown time.Duration, mode string) (*nodePool, error) {
	// An empty pool is legal and means "the operator disabled every proxy".
	// Keys keep their nodes but resolve to no transport, so requests fail
	// fast with a proxy error instead of silently going out through direct.
	if transports == nil {
		transports = &transportPool{}
	}
	if mode == "" {
		mode = config.ProxyModeRoundRobin
	}
	pool := &nodePool{
		nodes:        make([]*upstreamNode, 0, len(keys)),
		transports:   transports,
		cooldown:     cooldown,
		bindingCount: make([]int, len(transports.items)),
		mode:         mode,
	}
	if len(keys) == 0 {
		return pool, nil
	}
	if len(transports.items) == 0 {
		for i, key := range keys {
			node := &upstreamNode{key: key, index: i, preferredProxy: -1}
			node.proxyIndex.Store(-1)
			pool.nodes = append(pool.nodes, node)
		}
		return pool, nil
	}
	for i, key := range keys {
		proxyIndex := i % len(transports.items)
		node := &upstreamNode{key: key, index: i, preferredProxy: proxyIndex}
		node.proxyIndex.Store(int64(proxyIndex))
		pool.nodes = append(pool.nodes, node)
		pool.bindingCount[proxyIndex]++
	}
	return pool, nil
}

// RestoreProxy moves keys that were originally assigned to a recovered proxy
// back to it. Proxies without an original key simply become healthy failover
// candidates again.
func (p *nodePool) RestoreProxy(recoveredProxy int) int {
	if p == nil || p.transports == nil || recoveredProxy < 0 || recoveredProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		current := int(node.proxyIndex.Load())
		if node.preferredProxy != recoveredProxy || current == recoveredProxy {
			continue
		}
		if current >= 0 && current < len(p.bindingCount) {
			p.bindingCount[current]--
		}
		p.bindingCount[recoveredProxy]++
		node.proxyIndex.Store(int64(recoveredProxy))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

func (p *nodePool) Len() int { return len(p.nodes) }

// NodeByID resolves a node by its stable fingerprint. Callers only ever hold
// the fingerprint, never the key value, so the lookup cannot leak a secret.
func (p *nodePool) NodeByID(id string) *upstreamNode {
	if p == nil || id == "" {
		return nil
	}
	for _, node := range p.nodes {
		if config.Fingerprint(node.key) == id {
			return node
		}
	}
	return nil
}

func (p *nodePool) Proxy(node *upstreamNode) *proxyTransport {
	if p == nil || p.transports == nil || len(p.transports.items) == 0 {
		return nil
	}
	if len(p.transports.items) == 1 {
		return p.transports.items[0]
	}
	switch p.mode {
	case config.ProxyModeSticky:
		if candidate := p.transports.stickyProxy(); candidate != nil {
			return candidate
		}
	case config.ProxyModeRandom:
		if candidate := p.transports.pickRandomHealthy(); candidate != nil {
			return candidate
		}
	case config.ProxyModeRoundRobin:
		if candidate := p.transports.pickRoundRobinHealthy(); candidate != nil {
			return candidate
		}
	}
	if node != nil {
		index := int(node.proxyIndex.Load())
		if index >= 0 && index < len(p.transports.items) {
			return p.transports.items[index]
		}
	}
	return p.transports.items[0]
}

// RebindProxy moves every key currently using failedProxy to the least-loaded
// healthy proxy. Empty proxies are selected in configuration order; otherwise
// one of the least-loaded proxies is chosen at random. If every alternative is
// currently unhealthy, it still attempts the least-loaded alternative.
func (p *nodePool) RebindProxy(failedProxy int) int {
	if p == nil || p.transports == nil || len(p.transports.items) < 2 || failedProxy < 0 || failedProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		if int(node.proxyIndex.Load()) != failedProxy {
			continue
		}
		target := p.replacementLocked(failedProxy, true)
		if target < 0 {
			target = p.replacementLocked(failedProxy, false)
		}
		if target < 0 {
			continue
		}
		p.bindingCount[failedProxy]--
		p.bindingCount[target]++
		node.proxyIndex.Store(int64(target))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

func (p *nodePool) replacementLocked(failedProxy int, healthyOnly bool) int {
	minimum := int(^uint(0) >> 1)
	candidates := make([]int, 0, len(p.transports.items)-1)
	for i := range p.transports.items {
		if i == failedProxy || healthyOnly && !p.transports.items[i].healthy.Load() {
			continue
		}
		count := p.bindingCount[i]
		if count == 0 {
			return i
		}
		if count < minimum {
			minimum = count
			candidates = candidates[:0]
			candidates = append(candidates, i)
		} else if count == minimum {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[rand.IntN(len(candidates))]
}

type nodeCursor struct {
	pool *nodePool
	next int
}

// Cursor reserves a different starting node for each concurrent request.
// Selection is delayed until Next, so a node marked failed by the preceding
// attempt is immediately skipped. Both Cursor and Next allocate no memory.
func (p *nodePool) Cursor() nodeCursor {
	if len(p.nodes) == 0 {
		return nodeCursor{pool: p}
	}
	if p != nil && p.mode == config.ProxyModeRandom {
		return nodeCursor{pool: p, next: rand.IntN(len(p.nodes))}
	}
	return nodeCursor{pool: p, next: int((p.next.Add(1) - 1) % uint64(len(p.nodes)))}
}

// CursorFor returns a cursor whose first choice is stable for the supplied
// affinity key. This keeps every turn in a conversation on the same upstream
// key while retaining Next's cooldown-aware failover behavior. Empty affinity
// keys keep the round-robin behavior used by background tasks.
func (p *nodePool) CursorFor(affinity string) nodeCursor {
	if len(p.nodes) == 0 {
		return p.Cursor()
	}
	if p.mode == config.ProxyModeRandom {
		return nodeCursor{pool: p, next: rand.IntN(len(p.nodes))}
	}
	if p.mode == config.ProxyModeSticky && p.transports != nil {
		if pinned := p.transports.stickyProxy(); pinned != nil {
			for i, node := range p.nodes {
				if p.Proxy(node) == pinned {
					return nodeCursor{pool: p, next: i}
				}
			}
		}
	}
	if affinity == "" {
		return p.Cursor()
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(affinity))
	return nodeCursor{pool: p, next: int(hash.Sum64() % uint64(len(p.nodes)))}
}

func (c *nodeCursor) Next() *upstreamNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	choice := -1
	var earliest int64
	for offset := 0; offset < len(c.pool.nodes); offset++ {
		i := (c.next + offset) % len(c.pool.nodes)
		until := c.pool.nodes[i].cooldownUntil.Load()
		if until <= now {
			choice = i
			break
		}
		if choice == -1 || until < earliest {
			choice, earliest = i, until
		}
	}
	if choice < 0 {
		return nil
	}
	c.next = (choice + 1) % len(c.pool.nodes)
	return c.pool.nodes[choice]
}

func (p *nodePool) MarkSuccess(node *upstreamNode) {
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

func (p *nodePool) MarkFailure(node *upstreamNode, resp *http.Response, err error) {
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	multiplier := time.Duration(1 << min(failures-1, 3))
	delay := p.cooldown * multiplier
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = retryAfter
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}
