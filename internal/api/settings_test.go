package api

// The /settings page's handler tests (docs/spec/09 "/settings", docs/spec/14
// "secrets never reach the browser"). The coverage owed here: the admin gate
// on every route, the secret redaction (a token or endpoint host in the JSON
// is a docs/spec/14 violation, asserted by substring absence), the 405
// contract of PUT, and the backup download's SQLite header.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
)

func settingsBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("settings body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return body
}

// clearSettingsEnv makes a settings test hermetic: the page renders the
// effective configuration of *this* process, and the developer's live
// environment (gateway URL, the token variable GAME_NARRATOR_GATEWAY_TOKEN_ENV
// points at, every GAME_NARRATOR_ROLE_* variable) would otherwise decide the
// state a test asserts on. mutableSettingEnv() is the page's own list of the
// variables it renders, so clearing it clears everything the page reads.
func clearSettingsEnv(t *testing.T) {
	t.Helper()
	for _, e := range mutableSettingEnv() {
		t.Setenv(e, "")
	}
}

// roleEnv is the per-role GAME_NARRATOR_ROLE_<ROLE>_MODEL variable.
func roleEnv(role string) string {
	return "GAME_NARRATOR_ROLE_" + strings.ToUpper(role) + "_MODEL"
}

// settingsRoleRows parses the GET body into one map per role row.
func settingsRoleRows(t *testing.T, rec *httptest.ResponseRecorder) map[string]map[string]any {
	t.Helper()
	body := settingsBody(t, rec)
	roles, _ := body["models"].(map[string]any)["roles"].([]any)
	out := make(map[string]map[string]any, len(roles))
	for _, r := range roles {
		row, _ := r.(map[string]any)
		if name, _ := row["role"].(string); name != "" {
			out[name] = row
		}
	}
	return out
}

// TestSettingsRequiresAdmin pins the scope table's grant: every role below
// admin is refused on all three routes, and the refusal is a 403 problem, not
// a 200 with an empty configuration.
func TestSettingsRequiresAdmin(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))
	for _, tc := range []struct {
		method, target string
	}{
		{http.MethodGet, "/api/v1/settings"},
		{http.MethodPut, "/api/v1/settings"},
		{http.MethodPost, "/api/v1/admin/backup"},
	} {
		for _, role := range []auth.Role{auth.RoleViewer, auth.RoleCurator, auth.RoleOperator, auth.RoleDeveloper} {
			rec := doReq(t, h, a.as(role, httptest.NewRequest(tc.method, tc.target, strings.NewReader(`{}`))))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s = %d, want 403 (body %s)",
					tc.method, tc.target, role, rec.Code, rec.Body.String())
			}
		}
	}
}

// TestSettingsRedactsSecrets is the docs/spec/14 assertion the task names
// explicitly: with a gateway URL and token in the environment, neither the
// host string nor the token string may appear anywhere in the JSON — and a
// configured endpoint must render as the literal marker.
func TestSettingsRedactsSecrets(t *testing.T) {
	const secretHost = "gateway.internal.example:9443"
	const secretToken = "super-secret-bearer-token-abc123"
	clearSettingsEnv(t)
	t.Setenv("GAME_NARRATOR_GATEWAY_URL", "https://"+secretHost+"/v1")
	t.Setenv("GAME_NARRATOR_GATEWAY_TOKEN", secretToken)
	t.Setenv("GAME_NARRATOR_ROLE_FAST_REASONING_MODEL", "fast-model-x")

	a := newTestAuth(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))
	rec := a.get(t, h, auth.RoleAdmin, "/api/v1/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/settings as admin = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	if strings.Contains(raw, secretToken) {
		t.Error("the response body contains the bearer token")
	}
	if strings.Contains(raw, secretHost) {
		t.Error("the response body contains the gateway host")
	}
	body := settingsBody(t, rec)
	models, _ := body["models"].(map[string]any)
	roles, _ := models["roles"].([]any)
	var fast map[string]any
	for _, r := range roles {
		role, _ := r.(map[string]any)
		if role["role"] == "fast_reasoning" {
			fast = role
		}
	}
	if fast == nil {
		t.Fatal("the fast_reasoning role row is missing")
	}
	if fast["base_url"] != configuredMarker {
		t.Errorf("configured base_url = %v, want %q", fast["base_url"], configuredMarker)
	}
	if fast["model"] != "fast-model-x" {
		t.Errorf("model = %v, want the configured model id (the id is displayable, the endpoint is not)", fast["model"])
	}
	gw, _ := models["gateway"].(map[string]any)
	if gw["token_configured"] != true {
		t.Errorf("gateway token_configured = %v, want true (presence boolean)", gw["token_configured"])
	}
}

// TestSettingsNotConfiguredAndImmutable pins the two states the page renders:
// a role with no model reports NOT CONFIGURED with the variable to set in
// its state string, and the mutable section says 0 with the controlling
// variables named. The test is hermetic: every variable the page reads is
// cleared first, so a developer's live environment (a configured decision
// role and gateway included) cannot flip the expected state.
func TestSettingsNotConfiguredAndImmutable(t *testing.T) {
	clearSettingsEnv(t)
	a := newTestAuth(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))
	rec := a.get(t, h, auth.RoleAdmin, "/api/v1/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := settingsBody(t, rec)
	raw := rec.Body.String()

	var decision map[string]any
	roles, _ := body["models"].(map[string]any)["roles"].([]any)
	for _, r := range roles {
		role, _ := r.(map[string]any)
		if role["role"] == "decision" {
			decision = role
		}
	}
	if decision == nil {
		t.Fatal("the decision role row is missing")
	}
	state, _ := decision["state"].(string)
	if !strings.HasPrefix(state, "NOT CONFIGURED") {
		t.Errorf("decision state = %q, want a NOT CONFIGURED state naming GAME_NARRATOR_ROLE_DECISION_MODEL", state)
	}
	if !strings.Contains(state, "GAME_NARRATOR_ROLE_DECISION_MODEL") {
		t.Errorf("decision state does not name the variable to set: %q", state)
	}

	mutable, _ := body["mutable"].(map[string]any)
	if mutable["count"] != float64(0) {
		t.Errorf("mutable.count = %v, want 0 (this build has no runtime-mutable setting)", mutable["count"])
	}
	env, _ := mutable["env"].([]any)
	found := false
	for _, e := range env {
		if e == "GAME_NARRATOR_GATEWAY_URL" {
			found = true
		}
	}
	if !found {
		t.Error("mutable.env does not name GAME_NARRATOR_GATEWAY_URL")
	}

	version, _ := body["version"].(map[string]any)
	if version["commit"] != testBuild.Commit {
		t.Errorf("version.commit = %v, want the build's commit", version["commit"])
	}
	update, _ := body["update"].(map[string]any)
	if update["auto_update"] != false {
		t.Errorf("update.auto_update = %v, want false", update["auto_update"])
	}
	_ = raw
}

// TestSettingsConfiguredAndImplemented pins the third state: a role whose
// model is set and whose protocol this build ships an adapter for reports
// CONFIGURED — for every role, including the decision role (System One,
// wired by newDecisionWiring) and speech_asr (the OpenAI transcription wire),
// whose adapters the preflight table still lists as undeployed. The page
// derives availability from the protocol contract (protocolAdapters), not
// from that stale flag, so a configured System One role must not render NOT
// AVAILABLE. The negative arm (a model configured over a protocol with no
// adapter) pins the NOT AVAILABLE state the page keeps for genuinely
// unimplemented protocols.
func TestSettingsConfiguredAndImplemented(t *testing.T) {
	t.Run("configured and implemented reports CONFIGURED for every role", func(t *testing.T) {
		clearSettingsEnv(t)
		t.Setenv("GAME_NARRATOR_GATEWAY_URL", "http://127.0.0.1:19443/v1")
		for _, role := range []string{
			"agent_reasoning", "fast_reasoning", "vision_ocr_fallback",
			"decision", "voice_design", "voice_clone", "speech_asr",
		} {
			t.Setenv(roleEnv(role), "model-"+role)
		}
		t.Setenv("GAME_NARRATOR_ROLE_DECISION_PROTOCOL", "systemone")

		a := newTestAuth(t)
		h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))
		rec := a.get(t, h, auth.RoleAdmin, "/api/v1/settings")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		rows := settingsRoleRows(t, rec)
		for _, role := range []string{
			"agent_reasoning", "fast_reasoning", "vision_ocr_fallback",
			"decision", "voice_design", "voice_clone", "speech_asr",
		} {
			row, ok := rows[role]
			if !ok {
				t.Fatalf("the %s role row is missing", role)
			}
			if state, _ := row["state"].(string); state != "CONFIGURED" {
				t.Errorf("%s state = %q, want CONFIGURED (this build ships the adapter for its protocol)", role, state)
			}
		}
	})

	t.Run("configured over an unimplemented protocol reports NOT AVAILABLE", func(t *testing.T) {
		clearSettingsEnv(t)
		t.Setenv("GAME_NARRATOR_GATEWAY_URL", "http://127.0.0.1:19443/v1")
		t.Setenv("GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL", "model-voice_design")
		// probe_required names a protocol the build records but ships no
		// adapter for, so a configured role over it must report NOT AVAILABLE.
		t.Setenv("GAME_NARRATOR_ROLE_VOICE_DESIGN_PROTOCOL", "probe_required")

		a := newTestAuth(t)
		h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))
		rec := a.get(t, h, auth.RoleAdmin, "/api/v1/settings")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		rows := settingsRoleRows(t, rec)
		row, ok := rows["voice_design"]
		if !ok {
			t.Fatal("the voice_design role row is missing")
		}
		state, _ := row["state"].(string)
		if !strings.Contains(state, `protocol "probe_required" has no adapter implemented`) {
			t.Errorf("voice_design state = %q, want a NOT AVAILABLE state naming the unimplemented protocol", state)
		}
		if clone, ok := rows["voice_clone"]; ok {
			if state, _ := clone["state"].(string); !strings.HasPrefix(state, "NOT CONFIGURED") {
				t.Errorf("voice_clone state = %q, want NOT CONFIGURED (the protocol override is per role)", state)
			}
		}
	})
}

// TestSettingsPutDeclinesWithTheEnvList pins the 405 contract: PUT exists so
// the UI can learn the answer, and the answer names the variables the change
// would have to go through.
func TestSettingsPutDeclinesWithTheEnvList(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))
	rec := doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPut, "/api/v1/settings", `{"listen":"127.0.0.1:1"}`)))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/v1/settings = %d, want 405 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow = %q, want GET", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, ContentTypeProblem) {
		t.Errorf("Content-Type = %q, want problem+json", ct)
	}
	var problem map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("405 body is not JSON: %v", err)
	}
	detail, _ := problem["detail"].(string)
	if !strings.Contains(detail, "GAME_NARRATOR_GATEWAY_URL") {
		t.Errorf("the 405 detail does not name the controlling variables: %q", detail)
	}
}

// TestAdminBackupDownloadsSQLite pins the backup download: the admin gate,
// the content type, and the SQLite file header on the bytes — a download that
// is not a real database file would be the exact failure a restore test
// exists to catch.
func TestAdminBackupDownloadsSQLite(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))

	viewer := doReq(t, h, a.as(auth.RoleViewer, httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup", nil)))
	if viewer.Code != http.StatusForbidden {
		t.Fatalf("backup as viewer = %d, want 403", viewer.Code)
	}

	rec := doReq(t, h, a.as(auth.RoleAdmin, httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("backup as admin = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-sqlite3" {
		t.Errorf("Content-Type = %q, want application/x-sqlite3", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, ".sqlite") {
		t.Errorf("Content-Disposition = %q, want a .sqlite download name", cd)
	}
	header := rec.Body.Bytes()
	if len(header) < 16 || !bytes.HasPrefix(header, []byte("SQLite format 3\x00")) {
		t.Fatalf("the backup does not start with the SQLite header (%d bytes)", len(header))
	}
}
