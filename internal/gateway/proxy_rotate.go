package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

// rotateLinkTimeout bounds the operator-called "change exit IP" endpoint. The
// link usually just answers immediately; a slow one must not hold an admin
// request open indefinitely.
const rotateLinkTimeout = 20 * time.Second

// RotateProxyExit calls the operator-configured rotate link for one proxy and
// then returns that proxy to service. Providers change the exit IP for the
// next connection, so a proxy evicted by an outage-era failure must be cleared
// before traffic can use the new IP.
func (m *RuntimeManager) RotateProxyExit(ctx context.Context, id string) (string, error) {
	link := m.Config().ProxyRotateLink(id)
	if link == "" {
		return "", fmt.Errorf("no rotate link configured for this proxy")
	}
	status, detail, err := callRotateLink(ctx, link)
	if err != nil {
		return "", err
	}
	runtime := m.current.Load()
	if runtime != nil {
		runtime.gateway.resetProxyRoute(id)
	}
	if detail == "" {
		detail = fmt.Sprintf("HTTP %d", status)
	}
	return detail, nil
}

// callRotateLink performs the rotate request. Any 2xx/3xx answer counts as a
// successful rotation; the response body is only read to avoid leaking the
// connection, never surfaced, because providers put tokens in it.
func callRotateLink(ctx context.Context, link string) (int, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, rotateLinkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, link, nil)
	if err != nil {
		return 0, "", fmt.Errorf("rotate link invalid: %w", err)
	}
	req.Header.Set("User-Agent", httpx.UserAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("rotate link request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	// A rejected rotation must not be reported as success: the caller would
	// clear the proxy's health on a lie.
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return resp.StatusCode, "", fmt.Errorf("rotate link returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, strings.TrimSpace(string(body)), nil
}

// resetProxyRoute clears health and cooldown state for the transport whose
// fingerprint matches the configured proxy id, so the freshly rotated exit is
// eligible on the next request. It also clears the renewal flag: this is the
// path taken by the operator's manual "Rotate IP" button, where an explicit
// action is taken as proof enough that the exit changed.
func (g *Gateway) resetProxyRoute(id string) {
	if g == nil || g.transports == nil {
		return
	}
	for _, proxy := range g.transports.items {
		if config.Fingerprint(proxy.name) != id {
			continue
		}
		proxy.refreshing.Store(false)
		g.resetProxyHealth(proxy)
		return
	}
}

// resetProxyHealth clears health and cooldown state for one transport without
// touching the renewal flag. The automatic recovery path uses it: a rotation
// that only got a 2xx from the rotate link is not yet proof that the new exit
// works, so the proxy must stay out of service until the model probe passes.
func (g *Gateway) resetProxyHealth(proxy *proxyTransport) {
	if g == nil || proxy == nil {
		return
	}
	proxy.healthy.Store(true)
	g.restoreProxy(proxy)
	g.logger.Info("proxy exit rotated", "component", "proxy", "event", "proxy_rotated", "proxy", config.RedactURL(proxy.name))
}
