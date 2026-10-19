// Package providers — inference-transport security controls shared by the
// OpenAI-compatible, System One, TTS and ASR clients (docs/spec/14 "Mandatory
// controls": trust zone E is "authenticated but potentially changing", and the
// security acceptance tests require that a model-provider redirect and a wrong
// TLS certificate are rejected, not followed).
//
// This file owns the redirect policy. The TLS half of the acceptance test is
// the Go default: the clients never set InsecureSkipVerify, so a gateway that
// presents a certificate the system roots do not trust is refused by the
// handshake — the per-client tests pin that default so nobody can "fix" it
// away.

package providers

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ErrRedirectRefused is the stable sentinel a provider client returns when its
// redirect policy refuses a hop. Callers classify it as a non-retryable
// configuration failure: the gateway answered from a different host than the
// one the operator configured, which no retry against the same URL will fix.
var ErrRedirectRefused = errors.New("providers: redirect refused: the inference endpoint redirected the request to a different host or exceeded the redirect budget; the request was not sent to the redirect target")

// MaxRedirects bounds the hops one provider request may follow, even when
// every hop stays on the configured host. A gateway that redirects in a loop
// on its own host must not spin the request around until the context
// deadline; the budget makes the loop a classified error in one place.
const MaxRedirects = 3

// SafeRedirect returns an http.Client.CheckRedirect that confines one
// provider request to the host its BaseURL names (docs/spec/14: the gateway
// is operator configuration, so a request addressed to it must not be carried
// to another host — a redirect to 127.0.0.1, a link-local metadata endpoint or
// any internal service would turn the inference call into an SSRF primitive,
// with the internal response flowing back into the model context).
//
// The policy, per hop:
//
//   - budget: more than MaxRedirects hops is refused (loop protection);
//   - host: the target host (default port normalised) must equal the base
//     host; only an http→https upgrade may change the scheme, so the request
//     is never silently re-sent to a plaintext host;
//   - scheme: only http and https targets are acceptable at all.
//
// A nil base or an unparseable base is treated as "no host is known", and the
// first hop is already a change away from it, so every redirect is refused:
// a client built without a valid base URL must not gain the ability to
// follow redirects to hosts nobody configured.
func SafeRedirect(baseURL string) func(req *http.Request, via []*http.Request) error {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Host == "" {
		return func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("%w: the client has no valid base URL to compare the redirect against", ErrRedirectRefused)
		}
	}
	return func(req *http.Request, via []*http.Request) error {
		if err := hopBudget(via); err != nil {
			return err
		}
		return hopAllowed(base, req.URL)
	}
}

// RedirectConfinedToOrigin is a CheckRedirect for clients that cannot bind a
// base URL at construction time (the recorded cassette harness dials whatever
// the test or gate addresses): the same per-hop policy as SafeRedirect, with
// the configured host taken from the original request. The first element of
// via is the request the client was originally pointed at, so a gateway that
// answers it may only redirect within that same host.
func RedirectConfinedToOrigin(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || via[0] == nil || via[0].URL == nil {
		return fmt.Errorf("%w: no original request to compare the redirect against", ErrRedirectRefused)
	}
	if err := hopBudget(via); err != nil {
		return err
	}
	return hopAllowed(via[0].URL, req.URL)
}

// hopBudget applies the MaxRedirects loop protection in one place.
func hopBudget(via []*http.Request) error {
	if len(via) > MaxRedirects {
		return fmt.Errorf("%w: more than %d redirects", ErrRedirectRefused, MaxRedirects)
	}
	return nil
}

// hopAllowed is the per-hop policy shared by SafeRedirect and
// RedirectConfinedToOrigin: http(s) only, same host (default port
// normalised), and no downgrade from an https base.
func hopAllowed(base, target *url.URL) error {
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q is not http or https", ErrRedirectRefused, target.Scheme)
	}
	baseHost, basePort := splitHostPort(base.Host)
	host, port := splitHostPort(target.Host)
	if !strings.EqualFold(host, baseHost) || port != basePort {
		return fmt.Errorf("%w: request was addressed to %s but the endpoint redirected to %s", ErrRedirectRefused, base.Host, target.Host)
	}
	// Same host; a scheme change is allowed only as an upgrade.
	if base.Scheme == "https" && target.Scheme != "https" {
		return fmt.Errorf("%w: the https endpoint %s redirected down to %s://%s", ErrRedirectRefused, base.Host, target.Scheme, target.Host)
	}
	return nil
}

// splitHostPort mirrors net.SplitHostPort but normalises away a default port
// (80 or 443) so "gateway.lan" and "gateway.lan:443" name the same destination.
func splitHostPort(host string) (string, string) {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return host, ""
	}
	if p == "80" || p == "443" {
		p = ""
	}
	return h, p
}
