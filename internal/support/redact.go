// Package support builds the redacted diagnostics bundle behind
// `game-narrator support-bundle` (docs/spec/13 "Logging/privacy": "Provide
// export support bundle with automated scrubber"). It is the automated
// scrubber: everything it writes to the archive passes through the redaction
// rules in this package, and the tests prove the rules hold, including a
// content guard that builds a bundle from a data dir seeded with a
// distinctive profile and asserts none of it survives.
//
// What a bundle must never contain (docs/spec/13, AGENTS.md rule 5):
// credentials, gateway hostnames, private IPs, home paths, email addresses,
// game text, frames, crops, waveforms or the database itself. Diagnostics
// cite content by hash and count; user data never leaves the machine.
package support

import (
	"net"
	"regexp"
	"strings"
)

// Redacted replaces any value that must not leave the machine. It is a fixed
// sentinel, not the original value with parts removed, so a redacted bundle
// never carries a prefix of a real secret.
const Redacted = "<redacted>"

// RedactedHost is the host placeholder shared by RedactURL and the URL rule.
const RedactedHost = "<redacted-host>"

// RedactText scrubs one free-text string in a fixed order. URLs first: an
// absolute URL swallows its host, port, path, query and any userinfo in one
// step, so a gateway hostname with a token in the query string cannot leak
// through a partial rule. Then bare tokens and keys, then private addresses,
// home paths and emails.
func RedactText(s string) string {
	s = urlPattern.ReplaceAllString(s, "${1}://"+RedactedHost)
	// Header-shaped secrets before bare keys: an "Authorization: Bearer sk-…"
	// line must be consumed whole by the header rule, or the key rule would
	// truncate the value first and leave a bare word behind.
	s = authorizationPattern.ReplaceAllString(s, "${1}: "+Redacted)
	s = bearerPattern.ReplaceAllString(s, "${1} "+Redacted)
	s = apiKeyPattern.ReplaceAllString(s, Redacted)
	s = tokenKeyPattern.ReplaceAllString(s, "token="+Redacted)
	s = redactIPs(s)
	// /home/<user>[/rest] becomes ~[/rest] so a host path never identifies the
	// operator's account (docs/release.md "Hard rules" fails builds on
	// /home/ paths).
	s = homePathPattern.ReplaceAllString(s, "~${2}")
	s = emailPattern.ReplaceAllString(s, Redacted)
	return s
}

// RedactURL reduces any URL to its scheme and a redacted host. The gateway
// hostname is private by policy (docs/release.md "Hard rules"), and a port or
// path in it would let a support recipient probe the machine, so everything
// after the scheme is dropped. A value that is not an absolute URL is left
// to RedactText.
func RedactURL(raw string) string {
	if m := urlPattern.FindString(raw); m != "" {
		scheme := m
		if i := strings.Index(m, "://"); i >= 0 {
			scheme = m[:i]
		}
		return scheme + "://" + RedactedHost
	}
	if !strings.Contains(raw, "://") {
		return raw
	}
	// Looks like a URL but the scheme is odd: treat the whole value as
	// untrusted and redact it rather than guess.
	return Redacted
}

// RedactValue redacts one configuration value (a string read from the
// environment or resolved into a role). URL-shaped values are reduced to
// scheme + redacted host; everything else is scrubbed as free text. The
// value's position in the config (its key) is handled by RedactConfig, which
// redacts credential keys outright and keeps environment variable *names*.
func RedactValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return v
	}
	if u, ok := looksLikeURL(v); ok {
		return u
	}
	return RedactText(v)
}

// credentialKeyPattern matches the key names whose values are credentials.
// RedactConfig replaces the *value* of a matching key with Redacted; the key
// name itself is kept, because the name of the variable holding a secret is
// exactly what an operator needs to re-check the configuration (the doctor
// report follows the same rule: it prints the env var name, never the value).
var credentialKeyPattern = regexp.MustCompile(`(?i)(^|[_-])(token|secret|password|passwd|credential|api[_-]?key|auth)([_-]|$)`)

// RedactConfig redacts a resolved configuration tree (maps, slices, strings,
// numbers, bools). String values are scrubbed with RedactText; values whose
// key names a credential are replaced with Redacted even when they look
// harmless. Environment variable *names* are keys, so they survive.
func RedactConfig(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if credentialKeyPattern.MatchString(k) {
				out[k] = Redacted
				continue
			}
			out[k] = RedactConfig(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = RedactConfig(item)
		}
		return out
	case string:
		return RedactText(t)
	default:
		return v
	}
}

// looksLikeURL reports whether v is an absolute URL and, if so, returns it in
// reduced form (scheme + redacted host).
func looksLikeURL(v string) (string, bool) {
	i := strings.Index(v, "://")
	if i <= 0 {
		return "", false
	}
	scheme := v[:i]
	for j, r := range scheme {
		if j == 0 {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return "", false
			}
			continue
		}
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '.' || r == '-') {
			return "", false
		}
	}
	return RedactURL(v), true
}

// urlPattern matches any absolute URL. Only the scheme survives; host, port,
// path, query and (any) userinfo are replaced, so a private gateway hostname
// can never leave the machine inside a report line (docs/spec/13,
// docs/release.md "Hard rules": the scanner fails builds on gateway
// hostnames). "<" is excluded so a redaction sentinel already in the text
// cannot be swallowed into a later match.
var urlPattern = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://[^"\s'<>,;()\[\]]+`)

// homePathPattern matches /home/<user> and anything under it. The user
// segment must look like a real account name; /homebrew/ or /homepage/ must
// not collapse into ~brew/.
var homePathPattern = regexp.MustCompile(`(?i)/home/([a-z0-9][a-z0-9._-]*)((?:/[^ ]*)?)`)

// emailPattern matches an RFC-5322-lite local@domain with a TLD.
var emailPattern = regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`)

// apiKeys matches provider-style secret keys. The game-narrator token prefix
// gn_ is included: the admin command and the release scanner both treat it as
// a secret (docs/manuals/11 "Redaction rules").
var apiKeyPattern = regexp.MustCompile(`(?i)\b(?:sk[-_][a-z0-9_-]{8,}|gh[pousr]_[a-z0-9]{8,}|github_pat_[a-z0-9_]{8,}|gn_[a-z0-9]{8,})\b`)

// bearerPattern matches a "Bearer <value>" header fragment. The value class
// includes underscore so gn_/sk_ tokens are consumed whole, not truncated.
var bearerPattern = regexp.MustCompile(`(?i)\b(bearer)\s+[a-z0-9._~+/_=-]+`)

// tokenKeyPattern matches token=<value> in a header or query string.
var tokenKeyPattern = regexp.MustCompile(`(?i)\btoken=[a-z0-9._~+/_=-]+`)

// authorizationPattern matches an Authorization header with its value, with
// or without the Bearer scheme. The value is a single token: gateway tokens
// and API keys contain no whitespace.
var authorizationPattern = regexp.MustCompile(`(?i)\b(authorization)\s*:\s*(?:bearer\s+)?[a-z0-9._~+/_=-]+`)

// ipv4Pattern finds IPv4 candidates; each is validated with net.ParseIP
// before it is replaced, so 999.999.999.999 is left alone.
var ipv4Pattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// ipv6Pattern finds IPv6 candidates (possibly with a %zone suffix).
var ipv6Pattern = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}`)

// redactIPs replaces private IPv4 and IPv6 literals with Redacted. Public
// and loopback addresses are kept: they identify no machine on the operator's
// network, and the loopback bind address is useful diagnostic context.
func redactIPs(s string) string {
	s = ipv4Pattern.ReplaceAllStringFunc(s, func(m string) string {
		ip := net.ParseIP(m)
		if ip == nil || !isPrivateV4(ip) {
			return m
		}
		return Redacted
	})
	s = ipv6Pattern.ReplaceAllStringFunc(s, func(m string) string {
		tok := m
		zone := ""
		if i := strings.IndexByte(tok, '%'); i >= 0 {
			tok, zone = tok[:i], tok[i:]
		}
		ip := net.ParseIP(tok)
		if ip == nil || !isPrivateV6(ip) {
			return m
		}
		return Redacted + zone
	})
	return s
}

// isPrivateV4 reports whether ip falls in the RFC 1918 ranges.
func isPrivateV4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return (v4[0] == 10) ||
		(v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) ||
		(v4[0] == 192 && v4[1] == 168)
}

// isPrivateV6 reports whether ip is a unique-local (fc00::/7) or
// link-local (fe80::/10) address.
func isPrivateV6(ip net.IP) bool {
	v6 := ip.To16()
	if v6 == nil {
		return false
	}
	return v6[0] == 0xfd || (v6[0] == 0xfe && v6[1] == 0x80)
}

// IsPrivateNet reports whether ip (v4 or v6) falls in a private range. Tests
// pin the range boundary through this predicate.
func IsPrivateNet(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	if v4 := p.To4(); v4 != nil {
		return isPrivateV4(v4)
	}
	return isPrivateV6(p)
}
