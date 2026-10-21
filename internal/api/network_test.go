package api

// The /settings network section's handler tests (UX-13, ADR-0142): the
// tailnet URL is the running system's own answer — the operator's pin wins,
// the live tailscale derivation is labelled, and an absent Tailscale renders
// an explicit not-configured note, never a blank field or an invented URL.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
)

func networkSection(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := settingsBody(t, rec)
	network, _ := body["network"].(map[string]any)
	if network == nil {
		t.Fatalf("settings body has no network section: %s", rec.Body.String())
	}
	return network
}

// getSettingsAsAdmin runs GET /api/v1/settings with the given options and
// returns the recorded response.
func getSettingsAsAdmin(t *testing.T, a *testAuth, opts ...Option) *httptest.ResponseRecorder {
	t.Helper()
	h := a.handler(append([]Option{WithWebAssets(uiBuild()), WithStore(a.st)}, opts...)...)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	return doReq(t, h, a.as(auth.RoleAdmin, req))
}

// TestSettingsNetworkTailnetPin: the operator's pin (GAME_NARRATOR_TAILNET_URL)
// wins over the live probe and is rendered verbatim with source "env".
func TestSettingsNetworkTailnetPin(t *testing.T) {
	t.Setenv(EnvTailnetURL, "http://uno-q.stable-name.ts.net:8080")
	a := newTestAuth(t)
	rec := getSettingsAsAdmin(t, a)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	network := networkSection(t, rec)
	url, _ := network["tailnet_url"].(string)
	if url != "http://uno-q.stable-name.ts.net:8080" {
		t.Fatalf("tailnet_url = %q, want the pin verbatim", url)
	}
	if source, _ := network["source"].(string); source != "env" {
		t.Fatalf("source = %q, want \"env\" (the pin beats the live probe)", source)
	}
	if !strings.Contains(network["note"].(string), EnvTailnetURL) {
		t.Fatalf("note = %q, want it to name the pin's variable", network["note"])
	}
}

// TestSettingsNetworkTailscaleDerived: without the pin, the section derives
// the URL from the live status record (Self.DNSName) with the bound listen
// port, labelled source "tailscale".
func TestSettingsNetworkTailscaleDerived(t *testing.T) {
	t.Setenv(EnvTailnetURL, "")
	t.Setenv("GAME_NARRATOR_LISTEN", "")
	orig := tailscaleStatusProbe
	tailscaleStatusProbe = func(ctx context.Context) (*tailscaleStatusSelf, string) {
		return &tailscaleStatusSelf{DNSName: "uno-q.e2e-tailnet.ts.net"}, ""
	}
	t.Cleanup(func() { tailscaleStatusProbe = orig })

	a := newTestAuth(t)
	rec := getSettingsAsAdmin(t, a, WithListen("127.0.0.1:18090"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	network := networkSection(t, rec)
	url, _ := network["tailnet_url"].(string)
	if url != "http://uno-q.e2e-tailnet.ts.net:18090" {
		t.Fatalf("tailnet_url = %q, want the derived MagicDNS name with the bound port", url)
	}
	if source, _ := network["source"].(string); source != "tailscale" {
		t.Fatalf("source = %q, want \"tailscale\"", source)
	}
}

// TestSettingsNetworkNotConfigured: no pin and no tailscale answer — the URL
// is null (never an invented address) and the note carries the machine's
// reason, which the UI renders as the explicit not-configured state.
func TestSettingsNetworkNotConfigured(t *testing.T) {
	t.Setenv(EnvTailnetURL, "")
	orig := tailscaleStatusProbe
	tailscaleStatusProbe = func(ctx context.Context) (*tailscaleStatusSelf, string) {
		return nil, "no tailscale CLI on PATH"
	}
	t.Cleanup(func() { tailscaleStatusProbe = orig })

	a := newTestAuth(t)
	rec := getSettingsAsAdmin(t, a, WithListen("127.0.0.1:18090"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	network := networkSection(t, rec)
	if url, present := network["tailnet_url"]; present && url != nil {
		t.Fatalf("tailnet_url = %v, want null (an absent Tailscale is not an address to invent)", url)
	}
	if source, present := network["source"]; present && source != nil {
		t.Fatalf("source = %v, want null alongside the null URL", source)
	}
	note, _ := network["note"].(string)
	if !strings.Contains(note, "Tailscale is not configured") || !strings.Contains(note, "no tailscale CLI on PATH") {
		t.Fatalf("note = %q, want the explicit not-configured state with the machine's reason", note)
	}
	if !strings.Contains(note, EnvTailnetURL) {
		t.Fatalf("note = %q, want the unblock step to name the pin's variable", note)
	}
}

// TestSettingsNetworkJSONShape pins the wire contract the OpenAPI schema
// mirrors: the field is present and null-typed, so a client (the web UI)
// distinguishes "not configured" from "absent" by the null, not the key.
func TestSettingsNetworkJSONShape(t *testing.T) {
	t.Setenv(EnvTailnetURL, "")
	orig := tailscaleStatusProbe
	tailscaleStatusProbe = func(ctx context.Context) (*tailscaleStatusSelf, string) {
		return nil, "the tailscale CLI did not answer (is tailscaled running?)"
	}
	t.Cleanup(func() { tailscaleStatusProbe = orig })

	var view networkView
	if err := json.Unmarshal([]byte(`{"tailnet_url":null,"source":null,"note":"x"}`), &view); err != nil {
		t.Fatalf("unmarshal the wire shape: %v", err)
	}
	if view.TailnetURL != nil || view.Source != nil {
		t.Fatalf("null fields must decode as nil pointers (TailnetURL %v, Source %v)", view.TailnetURL, view.Source)
	}
}
