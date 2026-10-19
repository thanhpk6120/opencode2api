package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ParseProxyEntry splits an optional dynamic-pool marker ("pool+" scheme prefix
// or "#pool" URL fragment) from a configured proxy string and returns the clean
// dial URL alongside whether the entry is a rotating proxy pool (such as Resin).
func ParseProxyEntry(raw string) (clean string, pool bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "direct" {
		return trimmed, false
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "pool+") {
		trimmed = strings.TrimSpace(trimmed[len("pool+"):])
		pool = true
	}
	if u, err := url.Parse(trimmed); err == nil && u.Host != "" {
		if strings.EqualFold(strings.TrimSpace(u.Fragment), "pool") {
			pool = true
			u.Fragment = ""
			trimmed = u.String()
		}
	} else if idx := strings.LastIndex(trimmed, "#"); idx >= 0 && strings.EqualFold(strings.TrimSpace(trimmed[idx+1:]), "pool") {
		pool = true
		trimmed = strings.TrimSpace(trimmed[:idx])
	}
	return trimmed, pool
}

func resolveProxyFiles(configPath string, cfg *Config) error {
	trimList(&cfg.Proxies)
	effective := append([]string(nil), cfg.Proxies...)
	if cfg.ProxyFile != "" {
		resolved := cfg.ProxyFile
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(configPath), resolved)
		}
		proxies, err := readProxyFile(resolved)
		if err != nil {
			return fmt.Errorf("load proxy file %s: %w", resolved, err)
		}
		effective = append(effective, proxies...)
	}

	effective = uniqueProxyEntries(effective)
	if len(effective) == 0 {
		effective = []string{"direct"}
	}
	cfg.effectiveProxies = effective
	return nil
}

func readProxyFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var proxies []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		value := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		value = strings.TrimSpace(stripProxyLineComment(value))
		if value != "" {
			proxies = append(proxies, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return proxies, nil
}

func stripProxyLineComment(line string) string {
	for i := 0; i < len(line); i++ {
		if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' {
			continue
		}
		if line[i] == '#' || line[i] == ';' || (line[i] == '/' && i+1 < len(line) && line[i+1] == '/') {
			return line[:i]
		}
	}
	return line
}

// uniqueProxyEntries drops exact repeats while keeping a #pool entry next to a
// static proxy that dials the same endpoint: they are different routes (fresh
// connection, no cooldown, no health eviction), not two spellings of one.
func uniqueProxyEntries(items []string) []string {
	out := items[:0]
	seen := make(map[string]struct{}, len(items))
	pools := make(map[string]struct{}, len(items))
	for _, item := range items {
		key, pool := ParseProxyEntry(item)
		if key == "" {
			continue
		}
		if pool {
			if _, exists := pools[key]; exists {
				continue
			}
			pools[key] = struct{}{}
			out = append(out, item)
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func trimList(items *[]string) {
	out := (*items)[:0]
	for _, item := range *items {
		if value := strings.TrimSpace(item); value != "" {
			out = append(out, value)
		}
	}
	*items = out
}

// NormalizeRotateLink validates and trims an operator-supplied rotate link
// (the endpoint called to change a proxy's exit IP). It returns "" when the
// input is blank, which clears the mapping.
func NormalizeRotateLink(raw string) (string, error) {
	return parseProxyRotateLink(raw)
}

// parseProxyRotateLink validates the rotate link schema and returns the
// clean URL or an error. Only http/https endpoints are accepted: the link is
// called server-side with the operator's credentials, so a file:// or
// javascript: value would be an SSRF-shaped footgun rather than a feature.
func parseProxyRotateLink(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid rotate link %q", trimmed)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("rotate link %q must use http or https", trimmed)
	}
	return trimmed, nil
}

// normalizeProxyRotateLinks drops empty entries and rejects malformed or
// unsupported links before they are persisted. Values keyed by a fingerprint
// that no longer matches any configured proxy are kept: a proxy can be
// temporarily disabled or restored from proxyfile, and silently losing the
// operator's rotate link would be worse than one stale key.
func normalizeProxyRotateLinks(cfg *Config) error {
	if len(cfg.ProxyRotateLinks) == 0 {
		cfg.ProxyRotateLinks = nil
		return nil
	}
	cleaned := make(map[string]string, len(cfg.ProxyRotateLinks))
	for id, raw := range cfg.ProxyRotateLinks {
		key := strings.TrimSpace(id)
		if key == "" {
			continue
		}
		link, err := parseProxyRotateLink(raw)
		if err != nil {
			return err
		}
		if link == "" {
			continue
		}
		cleaned[key] = link
	}
	if len(cleaned) == 0 {
		cfg.ProxyRotateLinks = nil
		return nil
	}
	cfg.ProxyRotateLinks = cleaned
	return nil
}
