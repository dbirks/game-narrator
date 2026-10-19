package providers

// The redirect policy behind the spec-14 acceptance line "model provider
// redirect/wrong TLS certificate rejected": a mutable inference gateway
// (trust zone E) must not be able to carry a request to another host —
// loopback, link-local metadata or any internal service — because the
// response would flow back into the model context.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestSafeRedirectRefusesOffHost(t *testing.T) {
	check := SafeRedirect("http://127.0.0.1:1/anything")
	u, _ := url.Parse("http://192.168.77.10:8080/steal")
	if err := check(&http.Request{URL: u}, []*http.Request{{}}); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("off-host redirect: err = %v, want ErrRedirectRefused", err)
	}
}

// TestSafeRedirectRefusesOffHostThroughRealClient is the acceptance shape:
// gateway A answers a request to host A with a redirect to host B; the client
// must refuse the hop and B must never be contacted.
func TestSafeRedirectRefusesOffHostThroughRealClient(t *testing.T) {
	var bHits int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer b.Close()

	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", b.URL+"/inner")
		w.WriteHeader(http.StatusFound)
	}))
	defer a.Close()

	client := &http.Client{CheckRedirect: SafeRedirect(a.URL)}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, a.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("off-host redirect was followed (server B saw %d hits): the gateway must not be able to move the request", bHits)
	}
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("client error = %v, want ErrRedirectRefused", err)
	}
	if got := atomic.LoadInt32(&bHits); got != 0 {
		t.Fatalf("server B was reached %d times, want 0: the redirect target must never be contacted", got)
	}
}

func TestSafeRedirectAllowsSameHost(t *testing.T) {
	var hops int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hops, 1)
		if r.URL.Path == "/final" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Location", srv.URL+"/final")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: SafeRedirect(srv.URL)}
	resp, err := client.Get(srv.URL + "/v1")
	if err != nil {
		t.Fatalf("same-host redirect refused: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&hops) != 2 {
		t.Fatalf("hops=%d status=%d, want the redirect followed once on the same host", atomic.LoadInt32(&hops), resp.StatusCode)
	}
}

func TestSafeRedirectBudget(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A same-host loop: every hop redirects to the next path, forever.
		next := map[string]string{"/a": "/b", "/b": "/c", "/c": "/d", "/d": "/a"}
		w.Header().Set("Location", srv.URL+next[r.URL.Path])
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: SafeRedirect(srv.URL)}
	_, err := client.Get(srv.URL + "/a")
	if err == nil {
		t.Fatal("a same-host redirect loop was followed without bound: the budget must stop it")
	}
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("loop error = %v, want ErrRedirectRefused", err)
	}
}

func TestSafeRedirectSchemeRules(t *testing.T) {
	check := SafeRedirect("https://gateway.lan:8443/v1")
	mkReq := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	via := []*http.Request{}

	// Downgrade on the same host is refused: the token would cross plaintext.
	if err := check(mkReq("http://gateway.lan:8443/v1"), via); !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("https→http downgrade: err = %v, want ErrRedirectRefused", err)
	}
	// A different port on the same host is a different destination.
	if err := check(mkReq("https://gateway.lan:9443/v1"), via); !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("same host, other port: err = %v, want ErrRedirectRefused", err)
	}
	// The same destination (default-port normalisation) passes.
	if err := check(mkReq("https://gateway.lan:8443/v1"), via); err != nil {
		t.Errorf("identical destination: err = %v, want nil", err)
	}

	// An http base may upgrade to https on the same host…
	up := SafeRedirect("http://gateway.lan:8443/v1")
	if err := up(mkReq("https://gateway.lan:8443/v1"), via); err != nil {
		t.Errorf("http→https upgrade: err = %v, want nil", err)
	}
	// …and a non-http(s) scheme is refused outright.
	if err := up(mkReq("file:///etc/passwd"), via); !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("file: redirect: err = %v, want ErrRedirectRefused", err)
	}
}

func TestSafeRedirectNoBaseRefusesEverything(t *testing.T) {
	check := SafeRedirect("not a url at all")
	u, _ := url.Parse("http://127.0.0.1:6000/")
	req := &http.Request{URL: u}
	if err := check(req, nil); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("client without a valid base URL followed a redirect: err = %v, want ErrRedirectRefused", err)
	}
}

// TestRedirectConfinedToOrigin is the generic variant the recorded cassette
// harness uses (its client has no base bound at construction time): the hop
// is judged against the original request's host (via[0], "oldest first").
func TestRedirectConfinedToOrigin(t *testing.T) {
	origin, _ := url.Parse("http://127.0.0.1:9000/v1/x")
	via := []*http.Request{{URL: origin}}
	mkReq := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	if err := RedirectConfinedToOrigin(mkReq("http://127.0.0.1:9000/v1/y"), via); err != nil {
		t.Errorf("same-host hop: err = %v, want nil", err)
	}
	if err := RedirectConfinedToOrigin(mkReq("http://10.0.0.4:8080/steal"), via); !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("off-host hop: err = %v, want ErrRedirectRefused", err)
	}
	if err := RedirectConfinedToOrigin(mkReq("file:///etc/passwd"), via); !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("file: hop: err = %v, want ErrRedirectRefused", err)
	}
	// A same-host loop over the budget is refused by the shared budget check.
	loopVia := make([]*http.Request, MaxRedirects+1)
	for i := range loopVia {
		loopVia[i] = &http.Request{URL: origin}
	}
	if err := RedirectConfinedToOrigin(mkReq("http://127.0.0.1:9000/again"), loopVia); !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("over-budget hop: err = %v, want ErrRedirectRefused", err)
	}
	// No original request at all: every hop is refused.
	if err := RedirectConfinedToOrigin(mkReq("http://127.0.0.1:9000/x"), nil); !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("empty via: err = %v, want ErrRedirectRefused", err)
	}
}
