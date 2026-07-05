package git

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// transportKind classifies how a source URL is reached.
type transportKind int

const (
	transportHTTPS transportKind = iota
	transportSSH
	transportLocal // filesystem path / file:// — test fixtures only
)

// allowedSchemes is the scheme allow-list from R-AUTH.7. Only https and ssh may
// reach the network; every other scheme fails closed.
var allowedSchemes = map[string]transportKind{
	"https": transportHTTPS,
	"ssh":   transportSSH,
}

// parsedURL is the outcome of parsing and classifying a source URL.
type parsedURL struct {
	raw  string
	kind transportKind
	host string // hostname without port; empty for local paths
}

// Resolver resolves hostnames to IP addresses for the SSRF guard. It is
// satisfied by *net.Resolver; tests inject a fake to stay hermetic.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// parseURL parses and classifies a source URL, honoring allowLocal for the
// filesystem transport used by fixtures. It performs syntactic classification
// only; network guards are applied by validate.
func parseURL(raw string, allowLocal bool) (parsedURL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return parsedURL{}, newError(ReasonSourceNotAllowed, "source url is empty")
	}

	// Explicit scheme (https://, ssh://, file://).
	if strings.Contains(trimmed, "://") {
		u, err := url.Parse(trimmed)
		if err != nil {
			return parsedURL{}, wrapError(ReasonSourceNotAllowed, err, "invalid source url %q", redact(raw))
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme == "file" {
			if !allowLocal {
				return parsedURL{}, newError(ReasonSourceNotAllowed, fmt.Sprintf("scheme %q is not allowed", scheme))
			}
			return parsedURL{raw: trimmed, kind: transportLocal}, nil
		}
		kind, ok := allowedSchemes[scheme]
		if !ok {
			return parsedURL{}, newError(ReasonSourceNotAllowed,
				fmt.Sprintf("scheme %q is not allowed (only https and ssh are permitted)", scheme))
		}
		if u.Hostname() == "" {
			return parsedURL{}, newError(ReasonSourceNotAllowed, fmt.Sprintf("source url %q has no host", redact(raw)))
		}
		return parsedURL{raw: trimmed, kind: kind, host: u.Hostname()}, nil
	}

	// scp-like SSH syntax: [user@]host:path with no leading "/" in host.
	if host, ok := scpHost(trimmed); ok {
		return parsedURL{raw: trimmed, kind: transportSSH, host: host}, nil
	}

	// Otherwise it is a bare filesystem path (fixtures only).
	if allowLocal {
		return parsedURL{raw: trimmed, kind: transportLocal}, nil
	}
	return parsedURL{}, newError(ReasonSourceNotAllowed,
		fmt.Sprintf("source url %q has no supported scheme (use https:// or ssh://)", redact(raw)))
}

// redact removes any inline userinfo (user:password@) from a URL so credentials
// never leak into error messages, status, or events (SPEC R8.3 / TM9).
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("redacted")
	return u.String()
}

// scpHost extracts the host from scp-like syntax ("git@github.com:org/repo.git").
// It returns ok=false when the string is not scp-like.
func scpHost(s string) (string, bool) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 {
		return "", false
	}
	authority := s[:colon]
	// A "/" before the colon means it is a path, not scp syntax.
	if strings.ContainsAny(authority, "/") {
		return "", false
	}
	host := authority
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		host = authority[at+1:]
	}
	if host == "" {
		return "", false
	}
	return host, true
}

// validate applies the security guards to a parsed URL: SSRF IP guards
// (R-AUTH.7) and the operator source allow-list (R-AUTH.3). Local transport is
// exempt (fixtures only).
func (c *Client) validate(ctx context.Context, p parsedURL) error {
	if p.kind == transportLocal {
		return nil
	}
	if err := c.guardHost(ctx, p.host); err != nil {
		return err
	}
	if err := c.checkAllowList(p); err != nil {
		return err
	}
	return nil
}

// guardHost rejects hosts that resolve to link-local, loopback, unspecified, or
// multicast addresses (blocking cloud metadata endpoints such as
// 169.254.169.254). Private RFC1918 ranges are intentionally allowed so that
// in-cluster git servers work. Hostnames are resolved only when a Resolver is
// configured; IP-literal hosts are always checked.
func (c *Client) guardHost(ctx context.Context, host string) error {
	return guardResolvedHost(ctx, c.resolver, host)
}

// guardResolvedHost applies the SSRF IP guard to host using resolver. It is used
// both for the initial source URL and, via the redirect policy, for every
// redirect hop (R-AUTH.7 / TM5). IP-literal hosts are always checked; hostnames
// are resolved only when a resolver is provided.
func guardResolvedHost(ctx context.Context, resolver Resolver, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if blocked, why := blockedIP(ip); blocked {
			return newError(ReasonSourceNotAllowed, fmt.Sprintf("source host %q is a %s address", host, why))
		}
		return nil
	}
	if resolver == nil {
		return nil
	}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return wrapError(ReasonFetchFailed, err, "resolving source host %q", host)
	}
	for _, a := range addrs {
		if blocked, why := blockedIP(a.IP); blocked {
			return newError(ReasonSourceNotAllowed,
				fmt.Sprintf("source host %q resolves to a %s address", host, why))
		}
	}
	return nil
}

// blockedIP reports whether ip is in a range Kohen refuses to reach.
func blockedIP(ip net.IP) (bool, string) {
	switch {
	case ip.IsLoopback():
		return true, "loopback"
	case ip.IsUnspecified():
		return true, "unspecified"
	case ip.IsLinkLocalUnicast():
		return true, "link-local"
	case ip.IsLinkLocalMulticast():
		return true, "link-local multicast"
	case ip.IsMulticast():
		return true, "multicast"
	case ip.IsInterfaceLocalMulticast():
		return true, "interface-local multicast"
	}
	return false, ""
}

// checkAllowList enforces the operator source allow-list (R-AUTH.3). An empty
// allow-list permits any host (operators are advised to set one). An entry is a
// host ("github.com", matching that host and its subdomains) or a URL prefix
// (containing "/" or "://", matched literally).
func (c *Client) checkAllowList(p parsedURL) error {
	if len(c.allowList) == 0 {
		return nil
	}
	for _, entry := range c.allowList {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if strings.HasPrefix(p.raw, entry) {
				return nil
			}
			continue
		}
		if p.host == entry || strings.HasSuffix(p.host, "."+entry) {
			return nil
		}
	}
	return newError(ReasonSourceNotAllowed,
		fmt.Sprintf("source url %q is not permitted by the operator source allow-list", redact(p.raw)))
}
