package doctor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleReport() Report {
	checks := []Check{
		{ID: "storage.data_dir", Group: "Storage", Status: StatusOK, Summary: "data directory is writable and private", Duration: time.Millisecond},
		{ID: "tools.ffmpeg", Group: "Tools", Status: StatusBlocked, Summary: "ffmpeg is not installed",
			Detail: "exec: \"ffmpeg\": executable file not found in $PATH", Remedy: "run scripts/bootstrap-arch.sh (pacman -S ffmpeg)"},
		{ID: "providers.decision", Group: "Providers", Status: StatusNotConfigured, Summary: "role decision has no model configured",
			Remedy: "set GAME_NARRATOR_ROLE_DECISION_MODEL to a deployed model id, then re-run doctor"},
	}
	rep := Report{
		GeneratedAt: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		Checks:      checks,
	}
	rep.Totals = countTotals(checks)
	rep.Status = overallStatus(checks)
	return rep
}

func TestFormatPrintsAlignedGroupedTableAndTotals(t *testing.T) {
	var buf bytes.Buffer
	if err := Format(&buf, sampleReport()); err != nil {
		t.Fatalf("Format: %v", err)
	}
	got := buf.String()

	want := `Storage
OK              storage.data_dir    data directory is writable and private

Tools
BLOCKED         tools.ffmpeg        ffmpeg is not installed
                exec: "ffmpeg": executable file not found in $PATH
                → run scripts/bootstrap-arch.sh (pacman -S ffmpeg)

Providers
NOT CONFIGURED  providers.decision  role decision has no model configured
                → set GAME_NARRATOR_ROLE_DECISION_MODEL to a deployed model id, then re-run doctor

Totals: 1 OK, 0 DEGRADED, 1 BLOCKED, 1 NOT CONFIGURED, 0 FAIL — 3 checks, overall BLOCKED
`
	if got != want {
		t.Fatalf("human report =\n%s\nwant\n%s", got, want)
	}
}

func TestFormatDoesNotAdvertiseDetailForWorkingChecks(t *testing.T) {
	var buf bytes.Buffer
	rep := sampleReport()
	rep.Checks[0].Detail = "/home/user/.local/share/game-narrator"
	if err := Format(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "/home/user") {
		t.Fatal("OK rows printed their detail; the table would be unreadable and paths would leak to terminals")
	}
}

func TestFormatJSONCarriesEveryField(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	var decoded struct {
		GeneratedAt time.Time `json:"generated_at"`
		Status      string    `json:"status"`
		Totals      struct {
			OK            int `json:"ok"`
			Degraded      int `json:"degraded"`
			Blocked       int `json:"blocked"`
			NotConfigured int `json:"not_configured"`
			Fail          int `json:"fail"`
			Checks        int `json:"checks"`
		} `json:"totals"`
		Checks []struct {
			ID       string `json:"id"`
			Group    string `json:"group"`
			Status   string `json:"status"`
			Summary  string `json:"summary"`
			Detail   string `json:"detail"`
			Remedy   string `json:"remedy"`
			Duration int64  `json:"duration_ns"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON is not the documented shape: %v\n%s", err, buf.String())
	}
	if decoded.Status != StatusBlocked || decoded.Totals.Blocked != 1 || decoded.Totals.Checks != 3 {
		t.Fatalf("decoded totals/status = %+v", decoded)
	}
	if len(decoded.Checks) != 3 {
		t.Fatalf("checks = %d, want 3", len(decoded.Checks))
	}
	if decoded.Checks[1].Detail == "" || decoded.Checks[1].Remedy == "" {
		t.Fatalf("a blocked check lost its evidence or remedy: %+v", decoded.Checks[1])
	}
	if decoded.Checks[0].Duration != int64(time.Millisecond) {
		t.Fatalf("duration_ns = %d, want 1000000", decoded.Checks[0].Duration)
	}
	if decoded.GeneratedAt.IsZero() {
		t.Fatal("generated_at missing")
	}
}
