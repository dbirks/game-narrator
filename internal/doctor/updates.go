package doctor

// The updates.check row (ADR-0088, docs/spec/09 /settings "software
// updates"): the preflight's honest answer about the appliance's version
// relative to the repository's releases.
//
// The row never FAILs: an offline appliance is a healthy appliance, and an
// available update is an operator action, not a broken component. The states
// it can take —
//
//	NOT CONFIGURED  the check is disabled (GAME_NARRATOR_UPDATE_CHECK=0);
//	OK              up to date, or no releases published yet;
//	DEGRADED        a new version is available, the answer came from the
//	               offline cache, or the fresh check could not run at all —
//	               each with the concrete next step in Remedy;
//
// — are the spec-13 states the table is built from (docs/spec/13 "the service
// that has not been probed is never reported as working": a check that
// cannot run says so, with the reason, instead of an OK it did not earn).

import (
	"context"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/updates"
)

// UpdatesState is the answer the updates.check row renders. The CLI builds it
// from the environment (probeFromEnv in doctor.go's caller); tests inject a
// fixed state.
type UpdatesState struct {
	// Enabled is false when the operator disabled the check.
	Enabled bool
	// Repo is the repository the check looks at (the remedy names it).
	Repo string
	// Report is the check's answer (a failed fresh check carries its Error
	// and, when the cache held, the cached Release with FromCache set).
	Report updates.Report
}

// UpdatesProbe resolves the update check's state for the row. It gets the
// row's own context deadline, so a check that ignores its budget cannot
// hold the report open (the add() engine abandons it at deadline+grace).
type UpdatesProbe func(ctx context.Context) UpdatesState

// ProbeFromEnv builds the production probe: a checker over the environment's
// configuration with its cache in the data directory — the same cache the
// serve process writes, so the CLI and the appliance agree on the offline
// answer.
func ProbeFromEnv(lookupEnv func(string) (string, bool), cachePath string) UpdatesProbe {
	cfg := updates.ConfigFromEnv(lookupEnv, cachePath)
	c := updates.New(cfg)
	return func(ctx context.Context) UpdatesState {
		return UpdatesState{
			Enabled: cfg.Enabled,
			Repo:    cfg.Repo,
			Report:  c.Check(ctx),
		}
	}
}

// checkUpdates renders the row (see the file header for the state table).
func (r *runner) checkUpdates(ctx context.Context) Check {
	if r.opts.Updates == nil {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "the update check is not wired into this run",
			Remedy:  "wire the updates probe (the CLI builds it from the environment)",
		}
	}
	st := r.opts.Updates(ctx)
	if !st.Enabled || st.Report.Disabled {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "the update check is disabled",
			Detail:  updates.EnvCheck + "=0; set it to 1 to check " + st.Repo + " for new releases",
		}
	}
	current := bootstrap.Version().Version

	if st.Report.NoReleases {
		return Check{
			Status:  StatusOK,
			Summary: "no releases published yet",
			Detail:  "the endpoint for " + st.Repo + " answered but has no releases; the check keeps asking",
		}
	}
	if st.Report.Release == nil {
		// The fresh check failed and no cache answered: the honest state is
		// "could not check", not a guess.
		return Check{
			Status:  StatusDegraded,
			Summary: "the update check could not run",
			Detail:  st.Report.Error,
			Remedy: "check the network or the " + updates.EnvBaseURL + " / " + updates.EnvRepo +
				" variables; the appliance runs fine offline",
		}
	}
	rel := st.Report.Release
	if st.Report.FromCache {
		return Check{
			Status:  StatusDegraded,
			Summary: fmt.Sprintf("newest known release is %s (from the offline cache)", rel.Version),
			Detail: "the fresh check failed: " + st.Report.Error + "; the cached answer is from " +
				st.Report.CheckedAt.Format(time.RFC3339),
			Remedy: "once the network is back the check refreshes automatically; or run the check from the settings page",
		}
	}
	switch newer, ok := updates.Newer(current, rel.Version); {
	case !ok:
		// A dev build (or an untagged git-describe version) cannot be
		// compared: report the fact, not a verdict.
		return Check{
			Status:  StatusOK,
			Summary: fmt.Sprintf("dev build %s; latest published release is %s", current, rel.Version),
			Detail:  rel.URL,
			Remedy:  "install a tagged release build to track updates against it",
		}
	case newer:
		return Check{
			Status:  StatusDegraded,
			Summary: fmt.Sprintf("a new version is available: %s", rel.Version),
			Detail:  current + " → " + rel.Version + " — release notes: " + rel.URL,
			Remedy:  "replace the binary per docs/release.md (release-snapshot on the release host, copy, restart); this build has no auto-update",
		}
	default:
		return Check{
			Status:  StatusOK,
			Summary: fmt.Sprintf("up to date (%s)", rel.Version),
			Detail:  rel.URL,
		}
	}
}
