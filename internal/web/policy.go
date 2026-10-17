// Package web provides the SSRF-safe web fetcher and untrusted-content
// handling used by the onboarding agent's web_search/fetch_page tools
// (docs/spec/03, docs/spec/14 "Trust zones" D and "Mandatory controls").
//
// Web content is hostile data: this package never interprets page content,
// rejects private and reserved addresses at connect time (including via
// redirects and DNS rebinding), bounds size/time/MIME, and exposes
// QuoteUntrusted so fetched text enters the agent loop wrapped as quoted
// data, never as instructions.
package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Defaults for an empty Policy (docs/spec/06 "Timeout and cancellation
// defaults": agent browsing is 15-30 s per fetch; examples/config uses
// max_fetch_bytes: 4000000).
const (
	// DefaultMaxBytes is the raw response body cap (4 MiB).
	DefaultMaxBytes = 4 << 20
	// DefaultMaxDecompressed is the decompressed body cap (16 MiB), which
	// bounds gzip/deflate bombs (docs/spec/14 "reject Content-Encoding bombs").
	DefaultMaxDecompressed = 16 << 20
	// DefaultTimeout bounds a whole fetch including every redirect.
	DefaultTimeout = 20 * time.Second
	// DefaultMaxRedirects is the maximum number of redirects followed.
	DefaultMaxRedirects = 5
	// DefaultMaxText caps extracted text length (200 KB) before truncation.
	DefaultMaxText = 200 << 10
)

// Default media types the fetcher will accept (docs/spec/14 SSRF controls:
// "response size and MIME limits").
var DefaultMediaTypes = []string{
	"text/html",
	"text/plain",
	"application/json",
	"application/xml",
	"application/xhtml+xml",
}

// Error is a typed fetcher/search error carrying a stable machine-readable
// code, per the tool schemas in docs/spec/03 ("reliability and error codes")
// and the failure taxonomy in docs/spec/06.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string {
	if e == nil {
		return "web: <nil>"
	}
	return "web: " + e.Code + ": " + e.Err.Error()
}

// Unwrap supports errors.Is against the sentinel errors below.
func (e *Error) Unwrap() error { return e.Err }

// Is supports errors.Is matching by stable code: any typed error carrying
// the same code matches the sentinel, so callers need not unwrap.
func (e *Error) Is(target error) bool {
	if e == target {
		return true
	}
	var t *Error
	if errors.As(target, &t) && t != nil && e.Code == t.Code {
		return true
	}
	return false
}

// Errs are the sentinel errors; compare with errors.Is.
var (
	ErrScheme           = &Error{Code: "scheme", Err: errors.New("url scheme not allowed")}
	ErrPort             = &Error{Code: "port", Err: errors.New("url port not allowed")}
	ErrBlockedName      = &Error{Code: "blocked_name", Err: errors.New("hostname is blocked by policy")}
	ErrNumericHost      = &Error{Code: "numeric_host", Err: errors.New("hostname is numeric but not a valid IP literal (possible encoded IP)")}
	ErrPrivateAddress   = &Error{Code: "private_address", Err: errors.New("resolved address is private or reserved and not allowlisted")}
	ErrUnsupportedMedia = &Error{Code: "unsupported_media", Err: errors.New("response media type not allowed")}
	ErrBodyTooLarge     = &Error{Code: "body_too_large", Err: errors.New("response body exceeds byte limit")}
	ErrDecompTooLarge   = &Error{Code: "decompressed_too_large", Err: errors.New("decompressed body exceeds byte limit (compression bomb)")}
	ErrUnsupportedEncod = &Error{Code: "unsupported_encoding", Err: errors.New("content encoding not supported")}
	ErrTooManyRedirects = &Error{Code: "too_many_redirects", Err: errors.New("redirect limit exceeded")}
	ErrTimeout          = &Error{Code: "timeout", Err: errors.New("fetch timed out")}
	ErrNoAddresses      = &Error{Code: "no_addresses", Err: errors.New("hostname resolved to no addresses")}
)

// CodeOf extracts the stable error code, or "unknown" if err is not a *Error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "unknown"
}

// errf returns a typed error with sentinel's code and details formatted by
// format/args. Matching works by code (see (*Error).Is); the message uses
// the sentinel's plain description to avoid a doubled "web: code:" prefix.
func errf(sentinel *Error, format string, args ...any) error {
	return &Error{Code: sentinel.Code, Err: fmt.Errorf("%s: %s", fmt.Sprintf(format, args...), sentinel.Err)}
}

// Policy is the security policy applied to every URL the fetcher touches:
// the initial URL, every redirect hop, and every IP actually dialed.
type Policy struct {
	// Schemes are the allowed URL schemes. Empty means http and https.
	Schemes []string

	// AllowPorts are the allowed destination ports. Empty means 80 and 443.
	// An explicit port in the URL must be listed here.
	AllowPorts []int

	// AllowPrivate enables operator-facing development mode: addresses
	// matching PrivateAllow may be private. This is for explicitly
	// configured dev endpoints (e.g. a local dev proxy), NEVER for agent
	// browsing (docs/spec/14; ADR 0018).
	AllowPrivate bool

	// PrivateAllow is an explicit allowlist of private dev endpoints,
	// honored only when AllowPrivate is true. An entry is one of:
	//   - "host" or "host:port"  (exact hostname; resolved IP must be private)
	//   - "ip" or "ip:port"      (exact address)
	//   - "cidr"                 (e.g. "192.168.77.0/24")
	PrivateAllow []string

	// MaxBytes caps raw response bytes. Zero means DefaultMaxBytes.
	MaxBytes int64
	// MaxDecompressed caps decompressed bytes. Zero means DefaultMaxDecompressed.
	MaxDecompressed int64
	// Timeout bounds the whole fetch including redirects. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxRedirects bounds redirect hops. Zero means DefaultMaxRedirects.
	MaxRedirects int
	// UserAgent is sent on every request.
	UserAgent string
	// AllowedMediaTypes are the accepted response MIME types (matched
	// case-insensitively, without parameters). Empty means DefaultMediaTypes.
	AllowedMediaTypes []string

	// Resolver, if non-nil, replaces default DNS resolution for every dial
	// and is the seam tests use to inject a rebinding host. Production
	// code should leave it nil (uses net.DefaultResolver).
	Resolver func(ctx context.Context, host string) ([]net.IP, error)
}

// normalize fills defaults; returns a copy so callers may keep zero values.
func (p Policy) normalize() Policy {
	if len(p.Schemes) == 0 {
		p.Schemes = []string{"http", "https"}
	}
	if len(p.AllowPorts) == 0 {
		p.AllowPorts = []int{80, 443}
	}
	if p.MaxBytes <= 0 {
		p.MaxBytes = DefaultMaxBytes
	}
	if p.MaxDecompressed <= 0 {
		p.MaxDecompressed = DefaultMaxDecompressed
	}
	if p.Timeout <= 0 {
		p.Timeout = DefaultTimeout
	}
	if p.MaxRedirects <= 0 {
		p.MaxRedirects = DefaultMaxRedirects
	}
	if p.UserAgent == "" {
		p.UserAgent = defaultUserAgent
	}
	if len(p.AllowedMediaTypes) == 0 {
		p.AllowedMediaTypes = DefaultMediaTypes
	}
	return p
}

// ValidURL validates a URL against the policy before any I/O: scheme, port,
// hostname class, and (for IP literals) the address itself. It does not
// perform DNS lookups; rebinding hosts therefore also fail at dial time,
// where the actually-resolved IP is re-checked (see fetcher.go).
func (p Policy) ValidURL(raw string) error {
	p = p.normalize()
	u, err := url.Parse(raw)
	if err != nil {
		return errf(ErrScheme, "unparseable URL %q: %v", raw, err)
	}
	if !p.schemeAllowed(u.Scheme) {
		return errf(ErrScheme, "scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errf(ErrScheme, "URL has no host: %q", raw)
	}
	port := u.Port()
	var portN int
	if port != "" {
		fmt.Sscanf(port, "%d", &portN)
		// An explicit dev-endpoint allowlist entry may carry a non-80/443
		// port (dev proxies); everything else must be allowlisted.
		if !p.portAllowed(port) &&
			!(p.AllowPrivate && p.allowlistAuthorizedForName(host, portN)) {
			return errf(ErrPort, "port %s", port)
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if !p.AddressAllowed(ip, host, p.effectivePort(u)) {
			return errf(ErrPrivateAddress, "IP literal %s", host)
		}
		return nil
	}
	// Numeric-only hosts that are not valid IP literals (decimal, octal,
	// hex encodings such as 2130706433 or 0x7f.1) are rejected rather than
	// left to be interpreted by a dialer or browser.
	if isNumericHost(host) {
		return errf(ErrNumericHost, "host %q", host)
	}
	if p.AllowPrivate && p.allowlistAuthorizedForName(host, portN) {
		return nil
	}
	if isBlockedName(host) {
		return errf(ErrBlockedName, "host %q", host)
	}
	return nil
}

// allowlistAuthorizedForName reports whether a name-based PrivateAllow
// entry matches host (and port when the entry carries one). Used at URL
// level before any IP is known.
func (p Policy) allowlistAuthorizedForName(host string, port int) bool {
	for _, entry := range p.PrivateAllow {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" || strings.Contains(entry, "/") {
			continue
		}
		if net.ParseIP(entry) != nil {
			continue
		}
		entryHost, entryPort, splitErr := net.SplitHostPort(entry)
		if splitErr != nil {
			entryHost, entryPort = entry, ""
		}
		if !strings.EqualFold(entryHost, host) {
			continue
		}
		if entryPort != "" && entryPort != portStr(port) {
			continue
		}
		return true
	}
	return false
}

func (p Policy) schemeAllowed(scheme string) bool {
	scheme = strings.ToLower(scheme)
	for _, s := range p.Schemes {
		if strings.EqualFold(s, scheme) {
			return true
		}
	}
	return false
}

func (p Policy) portAllowed(port string) bool {
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil || n <= 0 || n > 65535 {
		return false
	}
	for _, p2 := range p.AllowPorts {
		if p2 == n {
			return true
		}
	}
	return false
}

// effectivePort returns the explicit port, or the scheme default.
func (p Policy) effectivePort(u *url.URL) int {
	if port := u.Port(); port != "" {
		n, _ := fmt.Sscanf(port, "%d", new(int))
		return n
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}

// AddressAllowed reports whether the actually-dialed address may be
// connected to. It is evaluated at connect time for every resolved IP so a
// hostname that looks public but resolves to a private address (DNS
// rebinding, or a redirect to a resolved name) is refused. A name-based
// allowlist entry only authorizes private IPs: a public resolution of an
// allowlisted dev host is refused rather than treated as a public host.
func (p Policy) AddressAllowed(ip net.IP, host string, port int) bool {
	p = p.normalize()
	if ip == nil {
		return false
	}
	if p.AllowPrivate {
		if allowed, matchedName := p.allowlistVerdict(host, port, ip); matchedName || allowed {
			return allowed
		}
	}
	return !isBlockedIP(ip)
}

// allowlistVerdict reports whether (host, port, ip) is authorized by
// PrivateAllow. Returns (allowed, matchedName): matchedName is true when a
// name-based entry matched the host; in that case the resolved IP must be
// private (dev endpoints are private by definition), so a public resolution
// of an allowlisted name is denied rather than falling back to public.
// IP/CIDR entries authorize the matching address unconditionally.
func (p Policy) allowlistVerdict(host string, port int, ip net.IP) (allowed, matchedName bool) {
	for _, entry := range p.PrivateAllow {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, ipNet, e := net.ParseCIDR(entry)
			if e == nil && ip != nil && ipNet.Contains(ip) {
				return true, false
			}
			continue
		}
		if e := net.ParseIP(entry); e != nil {
			if ip != nil && ip.Equal(e) {
				return true, false
			}
			continue
		}
		entryHost, entryPort, splitErr := net.SplitHostPort(entry)
		if splitErr != nil {
			entryHost, entryPort = entry, ""
		}
		if !strings.EqualFold(entryHost, host) {
			continue
		}
		if entryPort != "" && entryPort != portStr(port) {
			continue
		}
		// Name entry matched the host: authorize only private IPs.
		return isBlockedIP(ip), true
	}
	return false, false
}

func portStr(p int) string { return fmt.Sprintf("%d", p) }

// isBlockedName reports whether the hostname is reserved for local use
// regardless of what it resolves to.
func isBlockedName(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" ||
		h == "ip6-localhost" || h == "ip6-loopback" || h == "ipv6local" ||
		h == "ip4-local-host" ||
		strings.HasSuffix(h, ".local") ||
		strings.HasSuffix(h, ".internal") {
		return true
	}
	return false
}

// isNumericHost reports whether the host consists only of digits, hex
// letters, dots and colons — the shape an IP takes when encoded in
// decimal/octal/hex (e.g. 2130706433, 0177.0.0.1, 0x7f.1). Such hosts are
// rejected in ValidURL rather than handed to a resolver or dialer.
func isNumericHost(host string) bool {
	if host == "" {
		return false
	}
	for _, r := range host {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == 'x' || r == 'X':
		case r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}

// isBlockedIP reports whether the IP is loopback, private, link-local
// (including the 169.254.169.254 metadata range), CGNAT, ULA, multicast,
// or otherwise reserved. The default posture is deny-list by special range;
// global unicast addresses pass.
func isBlockedIP(ip net.IP) bool {
	v4 := ip.To4()
	if v4 != nil {
		if v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() ||
			v4.IsMulticast() || v4.IsUnspecified() {
			return true
		}
		// 0.0.0.0/8 "this network", CGNAT 100.64.0.0/10, 240.0.0.0/4 reserved.
		if v4[0] == 0 ||
			(v4[0] == 100 && v4[1]&0xc0 == 64) ||
			v4[0]&0xf0 == 240 {
			return true
		}
		return false
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	// Unique Local Address fc00::/7 (also caught by IsPrivate on recent
	// Go; checked explicitly for clarity).
	if ip16[0]&0xfe == 0xfc {
		return true
	}
	// IPv4-mapped/v4-compatible forms are already reduced by To4 above,
	// so any 16-byte form here is native IPv6: pass (global unicast).
	return false
}

// lookupIPs resolves host using the policy resolver, or net.DefaultResolver
// when none is configured.
func (p Policy) lookupIPs(ctx context.Context, host string) ([]net.IP, error) {
	if p.Resolver != nil {
		return p.Resolver(ctx, host)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}
