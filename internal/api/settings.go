package api

// The /settings page's API (docs/spec/09 "/settings": model endpoints, storage
// limits, security, import policies, audio sinks, backup/export, software
// updates).
//
// The configuration surface of this build is entirely environment variables —
// providers.LoadRoles for the model roles, GAME_NARRATOR_LISTEN,
// GAME_NARRATOR_DATA_DIR, GAME_NARRATOR_AUDIO_SINK / GAME_NARRATOR_AUDIO_DEVICE,
// GAME_NARRATOR_WEB_ALLOW_PRIVATE / GAME_NARRATOR_WEB_PRIVATE_ALLOW — plus the
// build constants cmd/game-narrator pins for the agent budget. Nothing is
// runtime-mutable: GET renders every value next to the variable (or constant)
// that controls it, and PUT answers 405 with that same list rather than
// inventing a write path for a setting that cannot change (AGENTS.md "no
// simulation theater": a save button that saves nothing is worse than the
// absence of one).
//
// Secrets never reach the browser (docs/spec/14): a bearer token is reported
// only as a presence boolean, and a configured gateway endpoint is reported as
// the literal "<configured>" — the host, port and path of the endpoint the
// appliance talks to are an operational detail, and docs/spec/08's system-info
// contract carries the same "no hostnames, no paths" rule.

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// configuredMarker is the only form a configured endpoint may take on this
// route: docs/spec/14 keeps the actual host, port and path server-side.
const configuredMarker = "<configured>"

// settingsResponse is the GET /api/v1/settings body. Every leaf carries the
// source of its value (an env var name or a build-constant label) so the UI's
// "nothing is mutable" notice can name where a change would have to go.
type settingsResponse struct {
	Models   modelsView   `json:"models"`
	Storage  storageView  `json:"storage"`
	Audio    audioView    `json:"audio"`
	Security securityView `json:"security"`
	Imports  importsView  `json:"imports"`
	Version  versionView  `json:"version"`
	Update   updateView   `json:"update"`
	Mutable  mutableView  `json:"mutable"`
}

type modelsView struct {
	Gateway modelsGatewayView `json:"gateway"`
	Roles   []modelRoleView   `json:"roles"`
}

type modelsGatewayView struct {
	// URLEnv / TokenEnvName are the variable names; the values never cross
	// this boundary.
	URLEnv          string `json:"url_env"`
	TokenEnvName    string `json:"token_env_name"`
	URLConfigured   bool   `json:"url_configured"`
	TokenConfigured bool   `json:"token_configured"`
}

type modelRoleView struct {
	Role string `json:"role"`
	// State is providers.RoleConfig.Status() verbatim: CONFIGURED,
	// NOT CONFIGURED or NOT AVAILABLE with its note.
	State      string `json:"state"`
	Configured bool   `json:"configured"`
	Model      string `json:"model"`
	Protocol   string `json:"protocol"`
	// BaseURL is "<configured>" when the role has an endpoint, "" when it
	// does not. The host itself never appears (docs/spec/14).
	BaseURL string `json:"base_url"`
	// ReasoningEffort "" means "omit the field, the model's default applies".
	ReasoningEffort string `json:"reasoning_effort"`
	Timeout         string `json:"timeout"`
	TokenConfigured bool   `json:"token_configured"`
	// Capabilities would carry openai.Capabilities() findings for the roles
	// served by the OpenAI-compatible adapters. The API layer holds no live
	// client in this build, so it is null and the UI says "not exposed" —
	// a capability claim nobody probed is the bug docs/spec/13 writes about.
	Capabilities any      `json:"capabilities"`
	Env          []string `json:"env"`
}

type storageView struct {
	// DataDir is the directory's *base name*; the absolute path is a host
	// path and stays server-side (docs/spec/08, system info contract).
	DataDir        string `json:"data_dir"`
	DataDirKnown   bool   `json:"data_dir_known"`
	FreeSpaceBytes int64  `json:"free_space_bytes"`
	FreeSpaceKnown bool   `json:"free_space_known"`
	// AssetStoreBytes is the sum of the asset store's blob bytes.
	AssetStoreBytes int64      `json:"asset_store_bytes"`
	SpeechCache     speechView `json:"speech_cache"`
	Env             []string   `json:"env"`
}

type speechView struct {
	// Bytes is nil: the speech cache stores its WAVs as content-addressed
	// blobs inside the same asset store (ADR-0023), so its share of the byte
	// total is not distinguishable by size. The note says so instead of
	// printing a number that would read as measured.
	Bytes *int64 `json:"bytes"`
	Note  string `json:"note"`
}

type audioView struct {
	Sink   string   `json:"sink"`
	Device string   `json:"device"`
	Note   string   `json:"note,omitempty"`
	Env    []string `json:"env"`
}

type securityView struct {
	Listen    string       `json:"listen"`
	ListenEnv string       `json:"listen_env"`
	Auth      securityAuth `json:"auth"`
}

type securityAuth struct {
	Mode            string `json:"mode"`
	AdminConfigured bool   `json:"admin_configured"`
}

type importsView struct {
	WebSearch webSearchView `json:"web_search"`
	Agent     agentView     `json:"agent"`
}

type webSearchView struct {
	// Enabled: the onboarding agent's web_search and fetch_page tools are
	// registered in this build (tools.DefaultTools); their output is
	// untrusted by the loop's trust rules (docs/spec/14).
	Enabled             bool   `json:"enabled"`
	Provider            string `json:"provider"`
	Note                string `json:"note"`
	EnvPrivateAllow     string `json:"env_private_allow"`
	PrivateHostsAllowed int    `json:"private_hosts_allowed"`
}

type agentView struct {
	MaxSteps     int `json:"max_steps"`
	MaxToolCalls int `json:"max_tool_calls"`
	// Source names where the numbers live: they are build constants of
	// cmd/game-narrator's agent wiring, not environment.
	Source string `json:"source"`
}

type versionView struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

type updateView struct {
	AutoUpdate bool   `json:"auto_update"`
	Docs       string `json:"docs"`
	Note       string `json:"note"`
}

type mutableView struct {
	Count int      `json:"count"`
	Note  string   `json:"note"`
	Env   []string `json:"env"`
}

// handleSettingsGet answers GET /api/v1/settings (admin role): the effective
// configuration of the running process, read-only. A value that cannot be
// determined honestly (free space on a platform without statfs) is reported
// as unknown with the reason, never as a guess.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	roles, err := providers.LoadRoles(osLookupEnv)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "provider-config-invalid",
			"Internal Error",
			"the process's own provider configuration is malformed: "+err.Error())
		return
	}
	dataDir := s.settingsDataDir()
	storage := s.settingsStorage(ctx, dataDir)
	writeJSON(w, http.StatusOK, settingsResponse{
		Models:   modelsView{Gateway: s.settingsGateway(roles), Roles: s.settingsRoles(roles)},
		Storage:  storage,
		Audio:    settingsAudio(),
		Security: s.settingsSecurity(ctx),
		Imports:  settingsImports(),
		Version: versionView{
			Version: s.build.Version, Commit: s.build.Commit, GoVersion: s.build.GoVersion,
			OS: s.build.GOOS, Arch: s.build.GOARCH,
		},
		Update: updateView{
			AutoUpdate: false,
			Docs:       "docs/release.md",
			Note: "There is no auto-update in this build. Replacing the binary is a " +
				"documented manual step (make release-snapshot on the release host, copy, " +
				"restart); the version and commit above are what you compare against.",
		},
		Mutable: mutableView{
			Count: 0,
			Note: "Nothing is mutable at runtime in this build: every value comes from an " +
				"environment variable or a build constant. Change the named variable and " +
				"restart the service; PUT answers 405 on purpose.",
			Env: mutableSettingEnv(),
		},
	})
}

// handleSettingsPut answers PUT /api/v1/settings (admin role). No setting in
// this build is runtime-mutable, so the handler declines the method with the
// full list of variables that control the values: a 405 that is also the
// documentation.
func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	s.writeProblem(w, r, http.StatusMethodNotAllowed, "no-mutable-settings",
		"Method Not Allowed",
		"no setting of this build is runtime-mutable; the effective configuration is "+
			"read from the environment at start. Change one of: "+joinStrings(mutableSettingEnv())+
			" (or a role's GAME_NARRATOR_ROLE_<ROLE>_MODEL / _URL / _PROTOCOL / _TIMEOUT / "+
			"_REASONING_EFFORT override) and restart the service.")
}

// handleAdminBackup answers POST /api/v1/admin/backup (admin role): a
// consistent snapshot of the live SQLite database taken through the store's
// online-backup API (read-only use: the live connection keeps serving) and
// served as a file download. Restore stays CLI-only — the UI documents it as
// `game-narrator db restore` rather than offering a button, because a restore
// is a process-lifecycle act (close the store, replace the file, reopen).
func (s *Server) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "store-unavailable",
			"Store Unavailable", "no catalogue store is wired into this process")
		return
	}
	// The backup lands in a private temp file first: the online-backup API
	// writes a real SQLite file, and the download streams it back. The name
	// carries the UTC timestamp so the file is unambiguous when saved.
	stamp := time.Now().UTC().Format("20060102T150405Z")
	name := "game-narrator-" + stamp + ".sqlite"
	tmp, err := os.CreateTemp("", "gn-backup-*")
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "backup-temp-failed",
			"Internal Error", "creating the temporary backup file failed; see the server log")
		return
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := s.store.Backup(r.Context(), tmpPath); err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "backup-failed",
			"Internal Error", "the SQLite online backup failed; see the server log")
		return
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "backup-open-failed",
			"Internal Error", "reading the finished backup file failed")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "backup-stat-failed",
			"Internal Error", "stat'ing the finished backup file failed")
		return
	}
	w.Header().Set("Content-Type", "application/x-sqlite3")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("backup download aborted before completion", "error", err)
	}
}
