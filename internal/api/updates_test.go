package api

// The update notification's handler tests (ADR-0088, docs/spec/09 /settings
// "software updates"). The release endpoint is a fake server on localhost —
// the hermetic half of the spec: no test reaches the real GitHub, and the
// offline behaviour (cache fallback, disabled check, unreachable endpoint)
// is asserted on the same surface the page renders.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/updates"
)

// fakeReleaseServer serves one GitHub-compatible release answer.
func fakeReleaseServer(t *testing.T, tag, url string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":%q,"name":"Release %s","html_url":%q,"published_at":"2026-10-12T10:00:00Z"}`, tag, tag, url)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeChecker builds a checker aimed at the given endpoint (a live server,
// or a dead URL for the offline tests).
func fakeChecker(cachePath, baseURL string) *updates.Checker {
	return updates.New(updates.Config{
		Enabled:   true,
		Repo:      "acme/widget",
		BaseURL:   baseURL,
		Timeout:   time.Second,
		CachePath: cachePath,
	})
}

// settingsUpdateSection pulls the /settings page's update section out of a
// settings response body.
func settingsUpdateSection(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := settingsBody(t, rec)
	sec, ok := body["update"].(map[string]any)
	if !ok {
		t.Fatalf("the settings body has no update section: %s", rec.Body.String())
	}
	return sec
}

// TestSettingsUpdateSectionRendersTheCheck pins the page's section from a
// live (fake) endpoint: current against latest, the release-notes URL, the
// available flag, the check time, and the fixed no-auto-update posture.
func TestSettingsUpdateSectionRendersTheCheck(t *testing.T) {
	srv := fakeReleaseServer(t, "v0.2.0", "https://github.com/acme/widget/releases/tag/v0.2.0")
	cache := t.TempDir() + "/latest.json"
	// The first check runs at "boot": by the time the page loads, Last()
	// holds the answer.
	ck := fakeChecker(cache, srv.URL)
	_ = ck.Check(context.Background())

	a := newTestAuth(t)
	clearSettingsEnv(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st), WithUpdates(ck))

	rec := doReq(t, h, a.as(auth.RoleAdmin, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	sec := settingsUpdateSection(t, rec)
	if sec["auto_update"] != false {
		t.Errorf("auto_update = %v, want false: the check informs, it never installs", sec["auto_update"])
	}
	if sec["current"] != "v0.0.1-test" {
		t.Errorf("current = %v, want this build's stamp", sec["current"])
	}
	if sec["available"] != true {
		t.Errorf("available = %v, want true: v0.2.0 is strictly newer than v0.0.1-test", sec["available"])
	}
	if sec["disabled"] == true || sec["from_cache"] == true {
		t.Errorf("disabled/from_cache = %v/%v, want both false on a fresh live answer", sec["disabled"], sec["from_cache"])
	}
	if sec["checking"] == true {
		t.Error("checking = true, want false: a completed check is not still checking")
	}
	latest, _ := sec["latest"].(map[string]any)
	if latest == nil || latest["version"] != "v0.2.0" ||
		latest["url"] != "https://github.com/acme/widget/releases/tag/v0.2.0" {
		t.Errorf("latest = %v, want the fake release with its notes URL", latest)
	}
	if sec["checked_at"] == nil {
		t.Error("checked_at is absent: the section must say when it determined the answer")
	}
	// The control variables are named (docs/spec/09: configurable, and the
	// settings page names where a change would have to go).
	envs, _ := sec["env"].([]any)
	if len(envs) != 3 {
		t.Errorf("env = %v, want the three check variables named", envs)
	}
}

// TestSettingsUpdateSectionDisabled: a disabled check answers the fact, with
// no network attempt and no latest.
func TestSettingsUpdateSectionDisabled(t *testing.T) {
	ck := updates.New(updates.Config{Enabled: false, Repo: "acme/widget"})
	a := newTestAuth(t)
	clearSettingsEnv(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st), WithUpdates(ck))

	rec := doReq(t, h, a.as(auth.RoleAdmin, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	sec := settingsUpdateSection(t, rec)
	if sec["disabled"] != true {
		t.Errorf("disabled = %v, want true", sec["disabled"])
	}
	if sec["latest"] != nil || sec["available"] == true {
		t.Errorf("a disabled check must not claim a release: latest=%v available=%v", sec["latest"], sec["available"])
	}
	if sec["note"] == nil {
		t.Error("the note is absent: the page must say the check is off and what that means")
	}
}

// TestSettingsUpdatesNoCheckerKeepsTheStaticSection: a process with no
// checker wired (the test servers) still renders the section's fixed facts —
// and the refresh route answers 503 instead of pretending.
func TestSettingsUpdatesNoCheckerKeepsTheStaticSection(t *testing.T) {
	a := newTestAuth(t)
	clearSettingsEnv(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))

	rec := doReq(t, h, a.as(auth.RoleAdmin, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings = %d, want 200", rec.Code)
	}
	sec := settingsUpdateSection(t, rec)
	if sec["auto_update"] != false || sec["current"] != "v0.0.1-test" {
		t.Errorf("the static section must keep its facts: %v", sec)
	}

	rec = doReq(t, h, a.as(auth.RoleAdmin, httptest.NewRequest(http.MethodPost, "/api/v1/settings/updates/refresh", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("refresh without a checker = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestSettingsUpdatesRefresh is the manual re-check: one fresh check now,
// rendered in the section's own shape — so the page and the refresh answer
// cannot disagree about the same fact.
func TestSettingsUpdatesRefresh(t *testing.T) {
	srv := fakeReleaseServer(t, "v0.3.0", "https://github.com/acme/widget/releases/tag/v0.3.0")
	ck := fakeChecker(t.TempDir()+"/latest.json", srv.URL)

	a := newTestAuth(t)
	clearSettingsEnv(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st), WithUpdates(ck))

	// The gate first: only admin may trigger a network check on the
	// appliance's behalf.
	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleCurator, auth.RoleOperator, auth.RoleDeveloper} {
		rec := doReq(t, h, a.as(role, httptest.NewRequest(http.MethodPost, "/api/v1/settings/updates/refresh", nil)))
		if rec.Code != http.StatusForbidden {
			t.Errorf("refresh as %s = %d, want 403", role, rec.Code)
		}
	}

	rec := doReq(t, h, a.as(auth.RoleAdmin, httptest.NewRequest(http.MethodPost, "/api/v1/settings/updates/refresh", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Current   string `json:"current"`
		Available bool   `json:"available"`
		Latest    *struct {
			Version string `json:"version"`
			URL     string `json:"url"`
		} `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Current != "v0.0.1-test" || !body.Available || body.Latest == nil ||
		body.Latest.Version != "v0.3.0" || body.Latest.URL == "" {
		t.Errorf("refresh answer = %+v, want the fresh release rendered in the section's shape", body)
	}
}

// TestSettingsUpdatesRefreshFallsBackToTheCache is the offline guarantee:
// the fresh check fails (the endpoint is gone), and the answer is the cached
// release with the error reported — the page never loses a known answer
// because the network left, and never invents one.
func TestSettingsUpdatesRefreshFallsBackToTheCache(t *testing.T) {
	srv := fakeReleaseServer(t, "v0.2.0", "https://github.com/acme/widget/releases/tag/v0.2.0")
	cache := t.TempDir() + "/latest.json"
	ck := fakeChecker(cache, srv.URL)
	// One good answer lands in the cache…
	if rep := ck.Check(context.Background()); rep.Release == nil || rep.Error != "" {
		t.Fatalf("the seeding check failed: %+v", rep)
	}
	// …then the endpoint is gone.
	srv.Close()

	a := newTestAuth(t)
	clearSettingsEnv(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st), WithUpdates(ck))

	rec := doReq(t, h, a.as(auth.RoleAdmin, httptest.NewRequest(http.MethodPost, "/api/v1/settings/updates/refresh", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d, want 200 (the offline answer is a fact, not an error status)", rec.Code)
	}
	var body struct {
		FromCache bool   `json:"from_cache"`
		Error     string `json:"error"`
		Latest    *struct {
			Version string `json:"version"`
		} `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.FromCache {
		t.Error("from_cache = false, want true: the answer came from the offline cache")
	}
	if body.Error == "" {
		t.Error("the error is absent: the page must say the fresh check failed")
	}
	if body.Latest == nil || body.Latest.Version != "v0.2.0" {
		t.Errorf("latest = %+v, want the cached v0.2.0", body.Latest)
	}
}
