package admin

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/gateway"
	"opencode2api/internal/httpx"
)

// ProxyView is the operator-facing view of one configured proxy. The address
// is redacted (credentials hidden); the ID is the stable fingerprint used for
// edit/delete/enable operations.
type ProxyView struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	Healthy   bool   `json:"healthy"`
	Checking  bool   `json:"checking"`
	Enabled   bool   `json:"enabled"`
	Source    string `json:"source"`
	ZenKeys   int    `json:"zen_keys"`
	GoKeys    int    `json:"go_keys"`
	Anonymous bool   `json:"anonymous"`
}

func (a *Server) proxyViews() []ProxyView {
	cfg := a.manager.Config()
	resources := a.manager.Resources()
	disabled := make(map[string]struct{}, len(cfg.ProxyDisabled))
	for _, id := range cfg.ProxyDisabled {
		disabled[id] = struct{}{}
	}
	byAddr := make(map[string]gateway.ProxyStatus, len(resources.Proxies))
	for _, st := range resources.Proxies {
		byAddr[st.Address] = st
	}
	views := make([]ProxyView, 0, len(cfg.Proxies))
	for _, raw := range cfg.Proxies {
		id := config.Fingerprint(raw)
		redacted := config.RedactURL(raw)
		view := ProxyView{ID: id, Address: redacted, Enabled: true, Source: "config"}
		if _, ok := disabled[id]; ok {
			view.Enabled = false
		} else if _, ok := disabled[raw]; ok {
			view.Enabled = false
		}
		if st, ok := byAddr[redacted]; ok {
			view.Healthy = st.Healthy
			view.Checking = st.Checking
			view.ZenKeys = st.ZenKeys
			view.GoKeys = st.GoKeys
			view.Anonymous = st.Anonymous
		}
		views = append(views, view)
	}
	return views
}

func (a *Server) handleListProxies(w http.ResponseWriter, _ *http.Request) {
	cfg := a.manager.Config()
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"proxies":    a.proxyViews(),
		"proxy_mode": cfg.ProxySelectionMode(),
		"proxyfile":  cfg.ProxyFile,
	})
}

func (a *Server) handleCreateProxies(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Lines []string `json:"lines"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	normalized, err := config.NormalizeProxyLines(input.Lines)
	if err != nil {
		writeAdminError(w, 400, "invalid_proxy", a.manager.Redact(err.Error()))
		return
	}
	if len(normalized) == 0 {
		writeAdminError(w, 400, "invalid_proxy", "no proxy lines provided")
		return
	}
	_, err = a.manager.Update(func(cfg *config.Config) error {
		known := make(map[string]struct{}, len(cfg.Proxies))
		for _, raw := range cfg.Proxies {
			known[config.Fingerprint(raw)] = struct{}{}
		}
		for _, raw := range normalized {
			if _, dup := known[config.Fingerprint(raw)]; dup {
				continue
			}
			cfg.Proxies = append(cfg.Proxies, raw)
			known[config.Fingerprint(raw)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		writeAdminError(w, 400, "proxy_create_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleListProxies(w, r)
}

func (a *Server) handleUpdateProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var input struct {
		Value   string `json:"value,omitempty"`
		Enabled *bool  `json:"enabled,omitempty"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	_, err := a.manager.Update(func(cfg *config.Config) error {
		idx := -1
		for i, raw := range cfg.Proxies {
			if config.Fingerprint(raw) == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errUnknownProxy
		}
		if strings.TrimSpace(input.Value) != "" {
			normalized, err := config.ParseProxyLine(input.Value)
			if err != nil {
				return err
			}
			for i, raw := range cfg.Proxies {
				if i != idx && config.Fingerprint(raw) == config.Fingerprint(normalized) {
					return errDuplicateProxy
				}
			}
			oldID := config.Fingerprint(cfg.Proxies[idx])
			cfg.Proxies[idx] = normalized
			// A credential change keeps the enabled flag attached to the proxy.
			if isProxyDisabled(cfg.ProxyDisabled, oldID) {
				cfg.ProxyDisabled = replaceProxyDisabled(cfg.ProxyDisabled, oldID, config.Fingerprint(normalized))
			}
		}
		if input.Enabled != nil {
			fp := config.Fingerprint(cfg.Proxies[idx])
			if *input.Enabled {
				cfg.ProxyDisabled = removeProxyDisabled(cfg.ProxyDisabled, fp)
			} else if !isProxyDisabled(cfg.ProxyDisabled, fp) {
				cfg.ProxyDisabled = append(cfg.ProxyDisabled, fp)
			}
		}
		return nil
	})
	if err != nil {
		writeAdminError(w, 400, "proxy_update_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleListProxies(w, r)
}

func (a *Server) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := a.manager.Update(func(cfg *config.Config) error {
		kept := cfg.Proxies[:0]
		found := false
		for _, raw := range cfg.Proxies {
			if config.Fingerprint(raw) == id {
				found = true
				continue
			}
			kept = append(kept, raw)
		}
		if !found {
			return errUnknownProxy
		}
		cfg.Proxies = kept
		cfg.ProxyDisabled = removeProxyDisabled(cfg.ProxyDisabled, id)
		return nil
	})
	if err != nil {
		writeAdminError(w, 400, "proxy_delete_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleListProxies(w, r)
}

func (a *Server) handleSetProxyMode(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Mode string `json:"mode"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	switch input.Mode {
	case config.ProxyModeSticky, config.ProxyModeRoundRobin, config.ProxyModeRandom:
	default:
		writeAdminError(w, 400, "invalid_proxy_mode", "mode must be sticky, roundrobin, or random")
		return
	}
	_, err := a.manager.Update(func(cfg *config.Config) error {
		cfg.ProxyMode = input.Mode
		return nil
	})
	if err != nil {
		writeAdminError(w, 400, "proxy_mode_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleListProxies(w, r)
}

var (
	errUnknownProxy   = proxyErr("unknown or stale proxy id")
	errDuplicateProxy = proxyErr("proxy already exists")
)

type proxyErr string

func (e proxyErr) Error() string { return string(e) }

func isProxyDisabled(disabled []string, id string) bool {
	for _, item := range disabled {
		if item == id {
			return true
		}
	}
	return false
}

func removeProxyDisabled(disabled []string, id string) []string {
	out := disabled[:0]
	for _, item := range disabled {
		if item != id {
			out = append(out, item)
		}
	}
	return out
}

func replaceProxyDisabled(disabled []string, oldID, newID string) []string {
	out := make([]string, 0, len(disabled))
	for _, item := range disabled {
		if item == oldID {
			out = append(out, newID)
		} else {
			out = append(out, item)
		}
	}
	return out
}

// handleTestProxy checks one configured or ad-hoc proxy with an isolated
// client. It never mutates pool health, bindings, or cooldowns: the check
// only reports reachability.
func (a *Server) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID    string `json:"id,omitempty"`
		Value string `json:"value,omitempty"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	raw := strings.TrimSpace(input.Value)
	if raw == "" && input.ID != "" {
		for _, proxy := range a.manager.Config().Proxies {
			if config.Fingerprint(proxy) == input.ID {
				raw = proxy
				break
			}
		}
	}
	if raw == "" {
		// Ad-hoc single line: accept shorthand too so the operator can test
		// before adding.
		if strings.TrimSpace(input.Value) != "" {
			raw = strings.TrimSpace(input.Value)
		} else {
			writeAdminError(w, 400, "unknown_proxy", "proxy id or value is required")
			return
		}
	}
	if raw != "direct" && !strings.Contains(raw, "://") {
		normalized, err := config.ParseProxyLine(raw)
		if err != nil {
			writeAdminError(w, 400, "invalid_proxy", a.manager.Redact(err.Error()))
			return
		}
		raw = normalized
	}
	start := time.Now()
	reachable, status, detail := probeProxy(raw, 12*time.Second)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":         reachable,
		"address":    config.RedactURL(raw),
		"http_status": status,
		"latency_ms": time.Since(start).Milliseconds(),
		"detail":     detail,
	})
}

// probeProxy performs a single GET through one proxy URL with an isolated
// transport. Any HTTP response proves the route is reachable; only transport
// errors fail the probe.
func probeProxy(raw string, timeout time.Duration) (bool, int, string) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	if raw == "direct" {
		transport.Proxy = nil
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return false, 0, "invalid proxy URL"
		}
		transport.Proxy = http.ProxyURL(u)
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		return false, 0, err.Error()
	}
	req.Header.Set("User-Agent", httpx.UserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, err.Error()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return true, resp.StatusCode, "reachable"
}
