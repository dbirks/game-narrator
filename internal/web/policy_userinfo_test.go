package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A URL with a user:password userinfo component must be rejected before the
// request is built: net/http would send it as an Authorization: Basic header
// to whatever host names itself, and the fetch's debug log records the raw
// URL, so credentials would land in the log and on the wire. ValidURL is the
// one choke point (initial URL and every redirect hop), so the rejection
// belongs there.
func TestValidURLRejectsUserinfo(t *testing.T) {
	cases := []string{
		"https://user:secret@example.com/",
		"https://user@example.com/",
		"http://@example.com/",
		"https://a:b:c@d.example/",
	}
	for _, raw := range cases {
		if err := (Policy{}).ValidURL(raw); !errors.Is(err, ErrUserinfo) {
			t.Errorf("ValidURL(%q) = %v, want errors.Is(ErrUserinfo)", raw, err)
		}
	}
	// The no-credential form still passes.
	if err := (Policy{}).ValidURL("https://example.com/page?x=1"); err != nil {
		t.Errorf("ValidURL of a plain public URL = %v, want nil", err)
	}
}

// Redirects are revalidated through ValidURL, so a hop that gains credentials
// is refused with no request sent to the redirect target.
func TestFetchRejectsRedirectThatAddsUserinfo(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the redirect target with userinfo: %s", r.URL)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	targetPort := serverPort(target)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", fmt.Sprintf("http://user:pw@target.webtest:%d/", targetPort))
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	// Both loopback listeners behind public-looking names (tests resolve
	// names to 127.0.0.1; IP literals would be refused by the policy).
	hosts := map[string][]string{"origin.webtest": {"127.0.0.1"}, "target.webtest": {"127.0.0.1"}}
	p := (Policy{}).normalize()
	p.Resolver = staticResolver(hosts)
	for _, s := range []*httptest.Server{origin, target} {
		if port := serverPort(s); !containsPort(p.AllowPorts, port) {
			p.AllowPorts = append(p.AllowPorts, port)
		}
	}
	f := NewFetcher(p)
	f.DialCheck = dialCheckForBoth(origin, target)
	t.Cleanup(f.Client.CloseIdleConnections)

	_, err := f.Fetch(context.Background(), fmt.Sprintf("http://origin.webtest:%d/", serverPort(origin)))
	if !errors.Is(err, ErrUserinfo) {
		t.Fatalf("error = %v, want ErrUserinfo from the redirect revalidation", err)
	}
}

// dialCheckForBoth allows dialing exactly the given loopback servers.
func dialCheckForBoth(servers ...*httptest.Server) func(net.IP, string, int) error {
	return func(ip net.IP, _ string, port int) error {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			for _, s := range servers {
				if serverPort(s) == port {
					return nil
				}
			}
		}
		return ErrPrivateAddress
	}
}
