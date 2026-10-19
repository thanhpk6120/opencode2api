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
	ID         string `json:"id"`
	Address    string `json:"address"`
	Healthy    bool   `json:"healthy"`
	Checking   bool   `json:"checking"`
	Enabled    bool   `json:"enabled"`
	Source     string `json:"source"`
	ZenKeys    int    `json:"zen_keys"`
	GoKeys     int    `json:"go_keys"`
	Anonymous  bool   `json:"anonymous"`
	RotateLink string `json:"rotate_link,omitempty"`
}

func (a *Server) proxyViews() []ProxyView {
	cfg := a.manager.Config()
	resources := a.manager.Resources()
	disabled := make(map[string]struct{}, len(cfg.ProxyDisabled))
	for _, id := range cfg.ProxyDisabled {
		disabled[id] = struct{}{}
	}
	byID := make(map[string]gateway.ProxyStatus, len(resources.Proxies))
	for _, st := range resources.Proxies {
		if st.ID != "" {
			byID[st.ID] = st
		} else {
			byID[st.Address] = st
		}
	}
	views := make([]ProxyView, 0, len(cfg.Proxies))
	for _, raw := range cfg.Proxies {
		id := config.Fingerprint(raw)
		redacted := config.RedactURL(raw)
		view := ProxyView{ID: id, Address: redacted, Enabled: true, Source: "config", RotateLink: cfg.ProxyRotateLink(id)}
		if _, ok := disabled[id]; ok {
			view.Enabled = false
		} else if _, ok := disabled[raw]; ok {
			view.Enabled = false
		}
		if st, ok := byID[id]; ok {
			view.Healthy = st.Healthy
			view.Checking = st.Checking
			view.ZenKeys = st.ZenKeys
			view.GoKeys = st.GoKeys
			view.Anonymous = st.Anonymous
		}
		views = append(views, view)
	}
	// Proxyfile-merged entries are active in traffic but not managed here:
	// surface them read-only so the operator sees the full effective set.
	stored := make(map[string]struct{}, len(cfg.Proxies))
	for _, raw := range cfg.Proxies {
		stored[config.Fingerprint(raw)] = struct{}{}
	}
	for _, st := range resources.Proxies {
		if _, ok := stored[st.ID]; ok {
			continue
		}
		views = append(views, ProxyView{ID: st.ID, Address: st.Address, Healthy: st.Healthy, Checking: st.Checking, Enabled: true, Source: "proxyfile", RotateLink: cfg.ProxyRotateLink(st.ID), ZenKeys: st.ZenKeys, GoKeys: st.GoKeys, Anonymous: st.Anonymous})
	}
	return views
}

func (a *Server) handleListProxies(w http.ResponseWriter, _ *http.Request) {
	cfg := a.manager.Config()
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"proxies":        a.proxyViews(),
		"proxy_mode":     cfg.ProxySelectionMode(),
		"proxyfile":      cfg.ProxyFile,
		"proxy_source":   cfg.ProxySource,
		"rotating_proxy": cfg.Rotating,
	})
}

func (a *Server) handleCreateProxies(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Lines []string `json:"lines"`
		// RotateLink is applied to every proxy line in this request: an
		// operator pasting a batch from one provider almost always shares the
		// same "change exit IP" endpoint.
		RotateLink string `json:"rotate_link,omitempty"`
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
	rotateLink, err := config.NormalizeRotateLink(input.RotateLink)
	if err != nil {
		writeAdminError(w, 400, "invalid_rotate_link", a.manager.Redact(err.Error()))
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
			if rotateLink != "" {
				if cfg.ProxyRotateLinks == nil {
					cfg.ProxyRotateLinks = map[string]string{}
				}
				cfg.ProxyRotateLinks[config.Fingerprint(raw)] = rotateLink
			}
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
		Value      string  `json:"value,omitempty"`
		Enabled    *bool   `json:"enabled,omitempty"`
		RotateLink *string `json:"rotate_link,omitempty"`
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
			if err := rejectRedactedProxyValue(input.Value); err != nil {
				return err
			}
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
			// The rotate link is keyed by fingerprint, so an edited address
			// must carry it over instead of orphaning the mapping.
			if link, ok := cfg.ProxyRotateLinks[oldID]; ok && oldID != config.Fingerprint(normalized) {
				delete(cfg.ProxyRotateLinks, oldID)
				if cfg.ProxyRotateLinks == nil {
					cfg.ProxyRotateLinks = map[string]string{}
				}
				cfg.ProxyRotateLinks[config.Fingerprint(normalized)] = link
			}
		}
		if input.RotateLink != nil {
			link, err := config.NormalizeRotateLink(*input.RotateLink)
			if err != nil {
				return err
			}
			fp := config.Fingerprint(cfg.Proxies[idx])
			if link == "" {
				delete(cfg.ProxyRotateLinks, fp)
			} else {
				if cfg.ProxyRotateLinks == nil {
					cfg.ProxyRotateLinks = map[string]string{}
				}
				cfg.ProxyRotateLinks[fp] = link
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

// handleSaveProxySettings stores the proxy source mode and the rotating-proxy
// schedule. Both live outside the legacy config form, which omits them.
func (a *Server) handleSaveProxySettings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Source   *string                     `json:"proxy_source,omitempty"`
		Rotating *config.RotatingProxyConfig `json:"rotating_proxy,omitempty"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	_, err := a.manager.Update(func(cfg *config.Config) error {
		if input.Source != nil {
			cfg.ProxySource = *input.Source
		}
		if input.Rotating != nil {
			cfg.Rotating = *input.Rotating
		}
		return nil
	})
	if err != nil {
		writeAdminError(w, 400, "proxy_settings_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleListProxies(w, r)
}

var (
	errUnknownProxy   = proxyErr("unknown or stale proxy id")
	errDuplicateProxy = proxyErr("proxy already exists")
)

// rejectRedactedProxyValue stops a client from saving the masked address the UI
// displays ("http://***@host:port"). ParseProxyLine accepts that string and would
// replace the real credentials with the literal mask, permanently breaking the
// proxy entry.
func rejectRedactedProxyValue(value string) error {
	if strings.Contains(value, "***@") || strings.Contains(value, "%2A%2A%2A@") {
		return proxyErr("address is masked; enter the full address including credentials")
	}
	return nil
}

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

// handleRotateProxy calls the operator-configured rotate link for one proxy
// and clears that route's health so the new exit IP is used immediately.
func (a *Server) handleRotateProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := a.manager.RotateProxyExit(r.Context(), id)
	if err != nil {
		writeAdminError(w, 400, "proxy_rotate_failed", a.manager.Redact(err.Error()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"id":      id,
		"detail":  detail,
		"proxies": a.proxyViews(),
	})
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
		"ok":          reachable,
		"address":     config.RedactURL(raw),
		"http_status": status,
		"latency_ms":  time.Since(start).Milliseconds(),
		"detail":      detail,
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
