package api

// The cast audit endpoint on the wire (ADR-0171): GET
// /api/v1/profiles/{id}/cast/audit reads each roster row's approved voice,
// measures the reference's median F0 with the YIN estimator and compares it
// with the class the character's profile data expects. The measurement leg is
// synthetic tones at 110 Hz (male-range) and 220 Hz (female-range) — no
// recorded audio, no game text beyond the fixture roster names (rule 5).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// auditEnv is the catalogue env with one approved voice planted per character
// it names: the reference is a synthetic tone, the design instruction carries
// the gender/age descriptor the audit must match against.
func TestCastAuditEndpoint(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "cast-audit-flow", "castaudit")
	e.importCSV(auth.RoleCurator, profileID)

	chars, err := e.st.ListCharacters(e.ctx, profileID)
	if err != nil || len(chars) < 2 {
		t.Fatalf("roster after import = %d (err %v): the fixture must create characters", len(chars), err)
	}
	one, two := chars[0].Name, chars[1].Name

	// plant stores a synthetic tone in the asset store and an approved
	// revision for the character whose brief states the expected class.
	plant := func(character, brief string, freq float64) string {
		t.Helper()
		wav := speech.MakeToneWAV(freq, 1.5, 44100)
		stg, _, err := e.as.PutStaged(e.ctx, bytes.NewReader(wav), "")
		if err != nil {
			t.Fatalf("PutStaged(%s): %v", character, err)
		}
		info, err := e.as.Commit(e.ctx, stg)
		if err != nil {
			t.Fatalf("Commit(%s): %v", character, err)
		}
		id := games.NewID()
		err = e.st.CreateVoiceRevision(e.ctx, store.VoiceProfile{
			ID:                id,
			Character:         character,
			Revision:          1,
			State:             store.VoiceStateApproved,
			DesignInstruction: brief,
			ReferenceAssetID:  info.SHA256,
			Fingerprint:       strings.Repeat("f", 64),
			ApprovedBy:        "test-harness",
		})
		if err != nil {
			t.Fatalf("CreateVoiceRevision(%s): %v", character, err)
		}
		return id
	}
	// One character is expected female but carries a 110 Hz (male) reference:
	// MISMATCH. The other is expected male and carries a 110 Hz reference: OK.
	plant(one, "a calm, steady adult woman with a warm voice", 110)
	plant(two, "a broad-shouldered adult man with a deep voice", 110)

	// The denial path (ADR-0171): the audit is viewer-scoped; an unauthenticated
	// caller must get 401 (and a role below viewer none exists), not a report.
	if rec := e.get(auth.RoleNone, "/api/v1/profiles/"+profileID+"/cast/audit"); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated cast audit = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}

	body := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/cast/audit")
	if body["profile"] != profileID {
		t.Fatalf("report.profile = %v, want %s", body["profile"], profileID)
	}
	raw, err := json.Marshal(body["rows"])
	if err != nil {
		t.Fatalf("rows not JSON: %v", err)
	}
	var rows []struct {
		Character    string  `json:"character"`
		Expected     string  `json:"expected"`
		MeasuredF0Hz float64 `json:"measured_f0_hz"`
		Measured     string  `json:"measured"`
		Verdict      string  `json:"verdict"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("rows: %v (%s)", err, raw)
	}
	if len(rows) != len(chars) {
		t.Fatalf("report carries %d rows, want one per roster row (%d): %s", len(rows), len(chars), raw)
	}
	find := func(name string) (struct {
		Character    string  `json:"character"`
		Expected     string  `json:"expected"`
		MeasuredF0Hz float64 `json:"measured_f0_hz"`
		Measured     string  `json:"measured"`
		Verdict      string  `json:"verdict"`
	}, bool) {
		var got struct {
			Character    string  `json:"character"`
			Expected     string  `json:"expected"`
			MeasuredF0Hz float64 `json:"measured_f0_hz"`
			Measured     string  `json:"measured"`
			Verdict      string  `json:"verdict"`
		}
		for _, r := range rows {
			if r.Character == name {
				got = r
				return r, true
			}
		}
		return got, false
	}

	bad, ok := find(one)
	if !ok {
		t.Fatalf("no audit row for %q: %s", one, raw)
	}
	if bad.Expected != "mid (adult female)" {
		t.Errorf("%s expected class = %q, want the female class the brief states", one, bad.Expected)
	}
	if bad.Verdict != "MISMATCH" {
		t.Errorf("%s verdict = %q, want MISMATCH (female brief, 110 Hz male reference)", one, bad.Verdict)
	}
	if bad.MeasuredF0Hz < 90 || bad.MeasuredF0Hz > 140 {
		t.Errorf("%s measured F0 = %.1f Hz, want the 110 Hz tone in range", one, bad.MeasuredF0Hz)
	}
	if bad.Measured != "low (adult male)" {
		t.Errorf("%s measured class = %q, want the male class", one, bad.Measured)
	}

	good, ok := find(two)
	if !ok {
		t.Fatalf("no audit row for %q: %s", two, raw)
	}
	if good.Expected != "low (adult male)" || good.Verdict != "OK" {
		t.Errorf("%s = expected %q verdict %q, want male/OK", two, good.Expected, good.Verdict)
	}

	// A roster row with no assigned voice still gets a row: the audit reports
	// absence, never invents a measurement.
	if len(chars) > 2 {
		none, ok := find(chars[2].Name)
		if !ok {
			t.Fatalf("no audit row for %q: %s", chars[2].Name, raw)
		}
		if none.Verdict != "UNKNOWN" || none.MeasuredF0Hz != 0 {
			t.Errorf("%s (no voice) = %q @ %.1f Hz, want UNKNOWN @ 0", chars[2].Name, none.Verdict, none.MeasuredF0Hz)
		}
	}
}
