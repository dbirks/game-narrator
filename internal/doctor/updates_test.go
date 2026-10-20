package doctor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/updates"
)

// The updates.check row (ADR-0088). The probe is injected — the row's job is
// to render the check's answer in the report's states, and a doctor test that
// hit the network would not pass on an offline host (docs/spec/12 "Unit"
// mode).

// updatesProbe returns a probe answering with the given state.
func updatesProbe(enabled bool, repo string, rep updates.Report) UpdatesProbe {
	return func(context.Context) UpdatesState {
		return UpdatesState{Enabled: enabled, Repo: repo, Report: rep}
	}
}

func TestUpdatesRowRendersEveryState(t *testing.T) {
	cases := []struct {
		name       string
		probe      UpdatesProbe
		wantStatus string
		wantIn     string
	}{
		{
			name:       "not wired",
			probe:      nil,
			wantStatus: StatusNotConfigured,
			wantIn:     "not wired",
		},
		{
			name:       "disabled by configuration",
			probe:      updatesProbe(false, "dbirks/game-narrator", updates.Report{}),
			wantStatus: StatusNotConfigured,
			wantIn:     "disabled",
		},
		{
			name:       "no releases yet",
			probe:      updatesProbe(true, "dbirks/game-narrator", updates.Report{NoReleases: true, CheckedAt: time.Now().UTC()}),
			wantStatus: StatusOK,
			wantIn:     "no releases",
		},
		{
			name:       "failed with no cache",
			probe:      updatesProbe(true, "dbirks/game-narrator", updates.Report{Error: "reaching the release endpoint: connection refused"}),
			wantStatus: StatusDegraded,
			wantIn:     "could not run",
		},
		{
			name: "cached answer after a failed fresh check",
			probe: updatesProbe(true, "dbirks/game-narrator", updates.Report{
				Release:   &updates.Release{Version: "v0.1.0", URL: "https://github.com/dbirks/game-narrator/releases/tag/v0.1.0"},
				CheckedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
				FromCache: true,
				Error:     "reaching the release endpoint: dial tcp: i/o timeout",
			}),
			wantStatus: StatusDegraded,
			wantIn:     "offline cache",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t)
			h.opts.Updates = tc.probe
			c := h.check(h.run(), "updates.check")
			mustStatus(t, c, tc.wantStatus)
			if tc.wantIn != "" && !strings.Contains(c.Summary+" "+c.Detail, tc.wantIn) {
				t.Errorf("summary/detail = %q / %q, want it to contain %q", c.Summary, c.Detail, tc.wantIn)
			}
		})
	}
}

// TestUpdatesRowMatchesTheVersionComparison: the row's verdict must be what
// the comparison of the running binary's stamp with the published release
// says — DEGRADED naming the version when the release is strictly newer,
// the up-to-date fact when it is not, and the dev-build fact when the stamp
// does not parse (a unit-test binary is stamped "dev", which is the normal
// case here). The remedy in every "newer" case points at the manual
// replacement doc; the row never FAILs.
func TestUpdatesRowMatchesTheVersionComparison(t *testing.T) {
	current := bootstrap.Version().Version
	for _, rel := range []string{"v99.0.0", "v0.0.1"} {
		t.Run(rel, func(t *testing.T) {
			h := newFakeHost(t)
			h.opts.Updates = updatesProbe(true, "dbirks/game-narrator", updates.Report{
				Release:   &updates.Release{Version: rel, URL: "https://github.com/dbirks/game-narrator/releases/tag/" + rel},
				CheckedAt: time.Now().UTC(),
			})
			c := h.check(h.run(), "updates.check")
			newer, ok := updates.Newer(current, rel)
			switch {
			case !ok:
				mustStatus(t, c, StatusOK)
				if !strings.Contains(c.Summary, "dev build") {
					t.Errorf("an unparseable current stamp must say dev build: %q", c.Summary)
				}
			case newer:
				mustStatus(t, c, StatusDegraded)
				if !strings.Contains(c.Summary, rel) {
					t.Errorf("the newer release must name its version: %q", c.Summary)
				}
				if !strings.Contains(c.Remedy, "docs/release.md") {
					t.Errorf("the remedy must point at the manual replacement doc: %q", c.Remedy)
				}
			default:
				mustStatus(t, c, StatusOK)
				if !strings.Contains(c.Summary, "up to date") {
					t.Errorf("an at-or-below latest must say up to date: %q", c.Summary)
				}
			}
		})
	}
}

// TestUpdatesRowNeverFails: no combination of check answers may make the row
// FAIL — an offline or outdated appliance is not a broken one (the row's
// states are pinned above; this guards the worst-state rollup, which a FAIL
// would drag the whole report into).
func TestUpdatesRowNeverFails(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Updates = updatesProbe(true, "dbirks/game-narrator", updates.Report{Error: "offline"})
	rep := h.run()
	c := h.check(rep, "updates.check")
	if c.Status == StatusFail {
		t.Fatal("the updates row must not FAIL: an offline check is not a broken component")
	}
}
