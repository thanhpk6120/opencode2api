// Proxy line parser: accepts operator-friendly shorthand and normalizes it
// to the URL form the transport pool consumes.
package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Supported proxy selection modes.
const (
	ProxyModeSticky     = "sticky"
	ProxyModeRoundRobin = "roundrobin"
	ProxyModeRandom     = "random"
)

// Proxy source modes restrict which configured entries are used at all.
// Empty means "both", so existing configurations keep their behavior.
const (
	ProxySourceBoth     = "both"
	ProxySourceStatic   = "static"
	ProxySourceRotating = "rotating"
)

// Rotating-proxy defaults. They are applied during Normalize so the hot path
// and the operator UI read one canonical value.
const (
	DefaultRotateMinIntervalSeconds = 60
	DefaultRotateMaxIntervalSeconds = 1800
	DefaultRotateFailedIPCacheSize  = 10
)

// ParseProxyLine normalizes one operator-supplied proxy line into the URL
// form stored in Config.Proxies. Accepted forms:
//
//	direct
//	host:port
//	host:port:username:password
//	http://[user:pass@]host:port, https://..., socks5://..., socks5h://...
//
// A bare host:port pair defaults to the http scheme. IPv6 hosts must be
// bracketed (e.g. [::1]:8080:user:pass). Passwords may not contain a colon;
// use the URL form with percent-encoding for those.
func ParseProxyLine(line string) (string, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
	trimmed = strings.TrimSpace(stripProxyLineComment(trimmed))
	if trimmed == "" {
		return "", fmt.Errorf("empty proxy line")
	}
	if trimmed == "direct" {
		return "direct", nil
	}
	if strings.Contains(trimmed, "://") {
		u, err := url.Parse(trimmed)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("invalid proxy URL %q", RedactURL(trimmed))
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return "", fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
		return trimmed, nil
	}
	host, port, username, password, err := splitProxyShorthand(trimmed)
	if err != nil {
		return "", err
	}
	var auth string
	if username != "" {
		auth = url.UserPassword(username, password).String() + "@"
	}
	return "http://" + auth + net.JoinHostPort(host, port), nil
}

// splitProxyShorthand splits host:port[:user:pass], honoring bracketed IPv6.
func splitProxyShorthand(line string) (host, port, username, password string, err error) {
	rest := line
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return "", "", "", "", fmt.Errorf("invalid proxy %q: unbalanced IPv6 bracket", line)
		}
		host = rest[1:end]
		rest = rest[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return "", "", "", "", fmt.Errorf("invalid proxy %q: expected :port after IPv6 host", line)
		}
		rest = rest[1:]
	} else {
		parts := strings.SplitN(rest, ":", 4)
		if len(parts) < 2 {
			return "", "", "", "", fmt.Errorf("invalid proxy %q: expected host:port[:user:pass]", line)
		}
		host = parts[0]
		authGiven := len(parts) > 2
		rest = strings.Join(parts[1:], ":")
		// Re-split remainder so user/pass colons are rejected, not silently kept.
		tail := strings.SplitN(rest, ":", 3)
		port = tail[0]
		if len(tail) > 1 {
			username = tail[1]
		}
		if len(tail) > 2 {
			password = tail[2]
			if strings.Contains(password, ":") {
				return "", "", "", "", fmt.Errorf("invalid proxy %q: password must not contain ':' (use URL form with percent-encoding)", line)
			}
		}
		if host == "" {
			return "", "", "", "", fmt.Errorf("invalid proxy %q: host is empty", line)
		}
		return validateProxyPartsAuth(host, port, username, password, authGiven, line)
	}
	authParts := strings.SplitN(rest, ":", 3)
	port = authParts[0]
	authGiven := len(authParts) > 1
	if len(authParts) > 1 {
		username = authParts[1]
	}
	if len(authParts) > 2 {
		password = authParts[2]
		if strings.Contains(password, ":") {
			return "", "", "", "", fmt.Errorf("invalid proxy %q: password must not contain ':' (use URL form with percent-encoding)", line)
		}
	}
	return validateProxyPartsAuth(host, port, username, password, authGiven, line)
}

func validateProxyParts(host, port, username, password, line string) (string, string, string, string, error) {
	return validateProxyPartsAuth(host, port, username, password, username != "" || password != "", line)
}

func validateProxyPartsAuth(host, port, username, password string, authGiven bool, line string) (string, string, string, string, error) {
	if authGiven && (username == "" || password == "") {
		return "", "", "", "", fmt.Errorf("invalid proxy %q: username and password must both be non-empty when auth is given", line)
	}
	if host == "" {
		return "", "", "", "", fmt.Errorf("invalid proxy %q: host is empty", line)
	}
	if port == "" {
		return "", "", "", "", fmt.Errorf("invalid proxy %q: port is empty", line)
	}
	if num, err := strconv.Atoi(port); err != nil || num < 1 || num > 65535 {
		return "", "", "", "", fmt.Errorf("invalid proxy %q: port must be 1-65535", line)
	}
	if (username == "") != (password == "") {
		return "", "", "", "", fmt.Errorf("invalid proxy %q: username and password must both be set or both be empty", line)
	}
	return host, port, username, password, nil
}

// NormalizeProxyLines parses multiple operator lines, skipping blanks and
// comment-only lines, and reports the first error with its 1-based line number.
func NormalizeProxyLines(lines []string) ([]string, error) {
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if strings.TrimSpace(stripProxyLineComment(trimmed)) == "" {
			continue
		}
		normalized, err := ParseProxyLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out = append(out, normalized)
	}
	return out, nil
}
