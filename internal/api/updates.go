package api

// The update notification (ADR-0088, docs/spec/09 /settings "software
// updates", gap G11): the /settings page's version-and-updates section and the
// manual re-check route. The check itself lives in internal/updates — a
// bounded HTTP check against the releases of the repository this build belongs
// to, with the offline cache in the data directory — so this file is the
// render half: it turns the checker's last answer into the section's view and
// runs the on-demand refresh.
//
// The posture is fixed: the check informs, it never installs. auto_update is
// always false and stays so (docs/release.md's manual replacement is the
// upgrade path; the section's note says exactly that).

import (
	"net/http"
	"time"

	"github.com/dbirks/game-narrator/internal/updates"
)

// updateLatestView is the latest release as the section renders it: the
// version tag, the release-notes page (the "release notes link" of the spec
// line) and the publication time. No body text: the notes live on the page
// the URL names, and shipping release markdown through the API would be the
// update endpoint's business, not the appliance's.
type updateLatestView struct {
	Version     string    `json:"version"`
	Name        string    `json:"name,omitempty"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
}

// updateView is the /settings page's version-and-updates section. current is
// the running build; latest is what the check last determined (fresh or from
// the offline cache); available is true only when latest is strictly newer
// than current and both parsed (a dev build never claims "an update exists").
type updateView struct {
	AutoUpdate bool   `json:"auto_update"`
	Docs       string `json:"docs"`
	Note       string `json:"note"`
	// Current is this build's version stamp (the "current" of current-vs-
	// latest).
	Current string `json:"current"`
	// Latest is nil when the check knows no release (disabled, never ran, or
	// failed with no cache).
	Latest *updateLatestView `json:"latest"`
	// Available is true when Latest is strictly newer than Current.
	Available bool `json:"available"`
	// CheckedAt is when Latest was determined; zero (absent) when unknown.
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	// FromCache marks the offline-cache fallback: the fresh check failed and
	// this is the last good answer, from CheckedAt.
	FromCache bool `json:"from_cache,omitempty"`
	// Checking is true while no check has completed yet (the first one runs
	// at boot in the background).
	Checking bool `json:"checking,omitempty"`
	// NoReleases: the endpoint answered and has no releases yet.
	NoReleases bool `json:"no_releases,omitempty"`
	// Disabled: the operator turned the check off (EnvCheck).
	Disabled bool `json:"disabled,omitempty"`
	// Error is set when the fresh check failed (with or without a cached
	// answer): one line, no credentials.
	Error string `json:"error,omitempty"`
	// Env names the variables that control the check.
	Env []string `json:"env"`
}

// settingsUpdate builds the section from the wired checker. Without one (test
// servers) the section keeps its static note rather than claiming a state the
// process cannot have.
func (s *Server) settingsUpdate() updateView {
	view := updateView{
		AutoUpdate: false,
		Docs:       "docs/release.md",
		Current:    s.build.Version,
		Env:        updatesEnv(),
		Note: "There is no auto-update in this build. Replacing the binary is a " +
			"documented manual step (make release-snapshot on the release host, copy, " +
			"restart); the version and commit above are what you compare against.",
	}
	if s.updatesChecker == nil {
		return view
	}
	rep, _ := s.updatesChecker.Last()
	// The disabled state comes from the configuration, not the last report:
	// a disabled checker never runs, so its report is the zero value and
	// would never carry the flag.
	if !s.updatesChecker.Config().Enabled {
		view.Disabled = true
		view.Note = "The update check is disabled (" + updates.EnvCheck + "=0); " + view.Note
		return view
	}
	// No answer yet: the first check is still running in the background.
	if rep.Release == nil && !rep.NoReleases && rep.Error == "" {
		view.Checking = true
		view.Note = "The first update check is still running in the background; " + view.Note
		return view
	}
	view.FromCache = rep.FromCache
	view.NoReleases = rep.NoReleases
	view.Error = rep.Error
	if !rep.CheckedAt.IsZero() {
		at := rep.CheckedAt
		view.CheckedAt = &at
	}
	if rep.Release != nil {
		rel := *rep.Release
		view.Latest = &updateLatestView{
			Version:     rel.Version,
			Name:        rel.Name,
			URL:         rel.URL,
			PublishedAt: rel.PublishedAt,
		}
		if newer, ok := updates.Newer(s.build.Version, rel.Version); ok && newer {
			view.Available = true
		}
	}
	if view.Available {
		view.Note = "A newer version is available; replacing the binary is a documented manual step (docs/release.md)."
	} else if view.Error != "" {
		if view.FromCache {
			view.Note = "The last fresh check failed; the answer shown is the cached one from " +
				view.CheckedAt.Format(time.DateOnly) + "."
		} else {
			view.Note = "The update check could not reach the release endpoint; no cached answer is available yet."
		}
	}
	return view
}

// updatesEnv is the section's variable list: the check's own variables,
// named so a change has somewhere to go (the settings page's redaction rule
// is names only).
func updatesEnv() []string {
	return []string{
		updates.EnvCheck,
		updates.EnvRepo,
		updates.EnvBaseURL,
	}
}

// handleSettingsUpdatesRefresh answers POST /api/v1/settings/updates/refresh
// (admin role): one fresh check now, bypassing the background interval. The
// checker's own timeout bounds it, so an unreachable endpoint answers with
// its error (and the cached answer, if any) instead of holding the page open.
func (s *Server) handleSettingsUpdatesRefresh(w http.ResponseWriter, r *http.Request) {
	if s.updatesChecker == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "update-check-unwired",
			"Update Check Unavailable", "no update check is wired into this process")
		return
	}
	rep := s.updatesChecker.Check(r.Context())
	writeJSON(w, http.StatusOK, struct {
		updateView
	}{s.updateFromReport(rep)})
}

// updateFromReport renders one report the same way settingsUpdate renders the
// checker's last answer, so the refresh answer and the section never disagree
// about the same fact.
func (s *Server) updateFromReport(rep updates.Report) updateView {
	view := updateView{
		AutoUpdate: false,
		Docs:       "docs/release.md",
		Current:    s.build.Version,
		Env:        updatesEnv(),
		Note:       "There is no auto-update in this build; replacing the binary is a documented manual step (docs/release.md).",
		Disabled:   rep.Disabled,
		FromCache:  rep.FromCache,
		NoReleases: rep.NoReleases,
		Error:      rep.Error,
	}
	if !rep.CheckedAt.IsZero() {
		at := rep.CheckedAt
		view.CheckedAt = &at
	}
	if rep.Release != nil {
		rel := *rep.Release
		view.Latest = &updateLatestView{
			Version:     rel.Version,
			Name:        rel.Name,
			URL:         rel.URL,
			PublishedAt: rel.PublishedAt,
		}
		if newer, ok := updates.Newer(s.build.Version, rel.Version); ok && newer {
			view.Available = true
		}
	}
	if view.Available {
		view.Note = "A newer version is available; replacing the binary is a documented manual step (docs/release.md)."
	}
	return view
}
