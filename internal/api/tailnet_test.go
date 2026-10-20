package api

// The tailnet detection (UX-13, ADR-0110 §4): the interface scan for a
// 100.64.0.0/10 address, the bounded `tailscale ip` fallback, the explicit
// "not configured" state, the URL built from the bound port, and the two
// surfaces that carry the value (the settings security section and the
// system-info route) — all from the startup cache, never re-detected at
// request time.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
)

// fakeTailscaleErr is the not-found shape of the default CLI reader (an
// exec.Error with no output): DetectTailnet must degrade to the absent state,
// not treat it as a failure worth retrying at request time.
var fakeTailscaleErr = errors.New("exec: \"tailscale\": executable file not found in $PATH")

// depInterfaces is the injectable interface seam: a fixed list and per-
// interface addresses, so the tests run against synthetic interfaces instead
// of the host's network namespace. The list is sorted by name, so the first-
// match-wins answer is deterministic.
func depInterfaces(ifaces map[string][]string) TailnetDeps {
	names := make([]string, 0, len(ifaces))
	for name := range ifaces {
		names = append(names, name)
	}
	sort.Strings(names)
	return TailnetDeps{
		Interfaces: func() ([]net.Interface, error) {
			out := make([]net.Interface, 0, len(names))
			for i, name := range names {
				out = append(out, net.Interface{Index: i + 1, Name: name})
			}
			return out, nil
		},
		Addrs: func(ni net.Interface) ([]net.Addr, error) {
			var addrs []net.Addr
			for _, s := range ifaces[ni.Name] {
				ip := net.ParseIP(strings.Split(s, "/")[0])
				if ip == nil {
					continue
				}
				addrs = append(addrs, &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)})
			}
			return addrs, nil
		},
	}
}

// TestDetectTailnetFromInterface pins the first detection half: a machine
// interface with an address in 100.64.0.0/10 is the tailnet's own address —
// found without the CLI, the source word "interface".
func TestDetectTailnetFromInterface(t *testing.T) {
	deps := depInterfaces(map[string][]string{
		"eth0":       {"192.168.1.40"},
		"tailscale0": {"100.75.42.9"},
	})
	// The CLI must not even be consulted when the scan found the address.
	deps.TailscaleIP = func(ctx context.Context) (string, error) {
		t.Error("the CLI fallback must not run when the interface scan found the tailnet")
		return "", nil
	}
	v := DetectTailnet(context.Background(), deps)
	if !v.Present || v.Address != "100.75.42.9" || v.Source != tailnetSourceInterface {
		t.Fatalf("detection = %+v, want the interface answer 100.75.42.9 with source %q", v, tailnetSourceInterface)
	}
}

// TestDetectTailnetFallsBackToTheCLI pins the second half: no 100.64.0.0/10
// interface address, the CLI answers — the detection is the CLI's, source
// "tailscale".
func TestDetectTailnetFallsBackToTheCLI(t *testing.T) {
	deps := depInterfaces(map[string][]string{"eth0": {"192.168.1.40"}, "lo": {"127.0.0.1"}})
	deps.TailscaleIP = func(ctx context.Context) (string, error) { return "100.99.1.7", nil }
	v := DetectTailnet(context.Background(), deps)
	if !v.Present || v.Address != "100.99.1.7" || v.Source != tailnetSourceTailscale {
		t.Fatalf("detection = %+v, want the CLI answer 100.99.1.7 with source %q", v, tailnetSourceTailscale)
	}
}

// TestDetectTailnetAbsentStates pins the explicit "not configured" state in
// each of its causes: a CLI failure, a missing CLI binary, and nothing at
// all — present=false with the note, never a blank field, never a panic.
func TestDetectTailnetAbsentStates(t *testing.T) {
	absent := func(cli func(ctx context.Context) (string, error)) TailnetDeps {
		d := depInterfaces(map[string][]string{"eth0": {"192.168.1.40"}, "lo": {"127.0.0.1"}})
		d.TailscaleIP = cli
		return d
	}
	cases := []struct {
		name      string
		cli       func(ctx context.Context) (string, error)
		noteWants string
	}{
		{"a CLI failure degrades to the absent state",
			func(ctx context.Context) (string, error) { return "", errors.New("tailscale: not connected") },
			"not connected"},
		{"a missing CLI binary degrades to the absent state",
			func(ctx context.Context) (string, error) { return "", fakeTailscaleErr },
			"tailscale"},
		{"nothing at all is the explicit not-configured answer",
			func(ctx context.Context) (string, error) { return "", fakeTailscaleErr },
			"Tailscale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := DetectTailnet(context.Background(), absent(tc.cli))
			if v.Present {
				t.Fatalf("detection = %+v, want the absent state", v)
			}
			if v.Address != "" || v.URL != "" || v.Source != "" {
				t.Fatalf("the absent state carries leftover values: %+v", v)
			}
			if !strings.Contains(v.Note, tc.noteWants) {
				t.Fatalf("note %q does not carry %q (the explicit reason, never a blank field)", v.Note, tc.noteWants)
			}
		})
	}
}

// TestDetectTailnetBoundsTheCLI pins the bounded half of the fallback: a CLI
// that hangs is cancelled at the timeout (like the exec.CommandContext it
// stands in for: the process is killed and the read errors), not held by —
// the detection degrades to the absent state and the startup is not blocked
// past the bound.
func TestDetectTailnetBoundsTheCLI(t *testing.T) {
	deps := depInterfaces(map[string][]string{"eth0": {"192.168.1.40"}})
	cancelled := make(chan struct{})
	deps.TailscaleIP = func(ctx context.Context) (string, error) {
		<-ctx.Done() // the CLI that hangs until it is cancelled
		close(cancelled)
		return "", errors.New("tailscale: context canceled")
	}
	start := time.Now()
	v := DetectTailnet(context.Background(), deps)
	elapsed := time.Since(start)
	if v.Present {
		t.Fatalf("a cancelled CLI must not report a tailnet: %+v", v)
	}
	if v.Note == "" {
		t.Fatal("the absent state after a cancelled CLI carries no note; the operator needs the reason")
	}
	if elapsed < tailnetIPTimeout {
		t.Fatalf("the CLI was cut before its bound (%v < %v); the bound is the wait, not a skip", elapsed, tailnetIPTimeout)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("the CLI's context was never cancelled; the bound is decorative")
	}
}

// TestTailnetPort parses the port out of the bound listen address for the
// URL: a host:port, a bare port, and the malformed answer (no URL, not an
// invented one).
func TestTailnetPort(t *testing.T) {
	for listen, want := range map[string]int{
		"127.0.0.1:8080": 8080,
		"0.0.0.0:9090":   9090,
		"8080":           8080,
		"":               0,
		"not-a-port":     0,
	} {
		if got := TailnetPort(listen); got != want {
			t.Fatalf("TailnetPort(%q) = %d, want %d", listen, got, want)
		}
	}
}

// TestSettingsSecurityCarriesTheCachedTailnet pins the settings surface: the
// security section renders the bound listen address and the tailnet block
// from the startup cache — the value the operator's page renders, never
// re-detected at request time.
func TestSettingsSecurityCarriesTheCachedTailnet(t *testing.T) {
	a := newTestAuth(t)
	tv := TailnetView{
		Present: true,
		Address: "100.75.42.9",
		Port:    8081,
		URL:     "http://100.75.42.9:8081",
		Source:  tailnetSourceInterface,
		Note:    "detected from the machine's interfaces (100.64.0.0/10)",
	}
	h := a.handler(WithStore(a.st), WithListen("0.0.0.0:8081"), WithTailnet(tv))

	rec := a.get(t, h, auth.RoleAdmin, "/api/v1/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var v struct {
		Security struct {
			Listen  string `json:"listen"`
			Tailnet *struct {
				Present bool   `json:"present"`
				Address string `json:"address"`
				URL     string `json:"url"`
				Source  string `json:"source"`
			} `json:"tailnet"`
		} `json:"security"`
	}
	decodeCapture(t, rec, &v)
	if v.Security.Listen != "0.0.0.0:8081" {
		t.Fatalf("security.listen = %q, want the bound address", v.Security.Listen)
	}
	if v.Security.Tailnet == nil || !v.Security.Tailnet.Present ||
		v.Security.Tailnet.Address != "100.75.42.9" ||
		v.Security.Tailnet.URL != "http://100.75.42.9:8081" ||
		v.Security.Tailnet.Source != tailnetSourceInterface {
		t.Fatalf("security.tailnet = %+v, want the cached detection (the startup value, never re-run)", v.Security.Tailnet)
	}
}

// TestSystemInfoAndSettingsShareOneTailnetSpelling pins the one-spelling rule
// (ADR-0110 §4): the same cached value on both surfaces — the settings page
// and the monitoring client cannot drift.
func TestSystemInfoAndSettingsShareOneTailnetSpelling(t *testing.T) {
	a := newTestAuth(t)
	tv := TailnetView{Present: true, Address: "100.66.3.2", Port: 8080, URL: "http://100.66.3.2:8080", Source: tailnetSourceTailscale}
	h := a.handler(WithStore(a.st), WithListen("127.0.0.1:8080"), WithTailnet(tv))

	rec := a.get(t, h, auth.RoleAdmin, "/api/v1/system/info")
	var si struct {
		Tailnet *struct {
			Address string `json:"address"`
			URL     string `json:"url"`
		} `json:"tailnet"`
	}
	decodeCapture(t, rec, &si)

	rec = a.get(t, h, auth.RoleAdmin, "/api/v1/settings")
	var st struct {
		Security struct {
			Tailnet *struct {
				Address string `json:"address"`
				URL     string `json:"url"`
			} `json:"tailnet"`
		} `json:"security"`
	}
	decodeCapture(t, rec, &st)
	if si.Tailnet == nil || st.Security.Tailnet == nil {
		t.Fatalf("a surface dropped the tailnet block: system/info %+v, settings %+v", si.Tailnet, st.Security.Tailnet)
	}
	if si.Tailnet.Address != st.Security.Tailnet.Address || si.Tailnet.URL != st.Security.Tailnet.URL {
		t.Fatalf("the two surfaces disagree: system/info %s/%s vs settings %s/%s",
			si.Tailnet.Address, si.Tailnet.URL, st.Security.Tailnet.Address, st.Security.Tailnet.URL)
	}
}
