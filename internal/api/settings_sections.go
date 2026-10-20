package api

// The section builders behind GET /api/v1/settings (settings.go). Each builder
// reads exactly one surface of the configuration — the provider roles, the
// durable directory, the audio sink, the security posture, the import
// policies — so a section that cannot be answered honestly degrades to
// "unknown" without dragging the rest of the page down with it.
//
// Role availability is deliberately not rendered from
// providers.RoleConfig.Status(): that method still consults the preflight
// table's per-role "deployed" flags, which predate the adapters that landed
// afterwards (System One for the decision role, vLLM-Omni speech for the
// voice roles, OpenAI transcriptions for speech_asr). The page derives each
// role's state from the same facts the runtime wiring and the doctor's role
// probes use: the role's configured model, and whether this build ships an
// adapter for the protocol the role speaks (protocolAdapters below).

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/asr"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/updates"
)

// protocolAdapters answers "does this build ship an adapter for protocol p"
// by referencing the adapter's own NewFromRole constructor — the same
// constructor cmd/game-narrator's role wiring (newDecisionWiring,
// newVoiceWiring, the OCR fallback and ASR QA wiring) and the doctor's role
// probes call to build the client a configured role actually speaks
// (docs/spec/06 "Provider registry"). A protocol without an entry has no
// adapter in this build, and deleting or renaming an adapter package breaks
// the build here rather than leaving a stale flag behind.
//
// The keys are protocols, not roles: the runtime wires a role on the
// protocol it speaks (newDecisionWiring accepts the decision role only on
// System One; the tts and asr constructors reject any other protocol), so
// the page answers "will this build run this role" from the protocol
// contract, the way the runtime and doctor do.
var protocolAdapters = map[providers.Protocol]func(providers.RoleConfig) (any, error){
	providers.ProtocolOpenAIChat: func(cfg providers.RoleConfig) (any, error) {
		return openai.NewFromRole(cfg)
	},
	providers.ProtocolOpenAIChatVision: func(cfg providers.RoleConfig) (any, error) {
		return openai.NewFromRole(cfg)
	},
	providers.ProtocolSystemOne: func(cfg providers.RoleConfig) (any, error) {
		return systemone.NewFromRole(cfg)
	},
	providers.ProtocolOpenAITranscriptions: func(cfg providers.RoleConfig) (any, error) {
		return asr.NewFromRole(cfg)
	},
	providers.ProtocolVLLMOmniSpeech: func(cfg providers.RoleConfig) (any, error) {
		return tts.NewFromRole(cfg)
	},
}

// settingsRoleState renders one role row's state from the same two facts the
// runtime wiring and the doctor's role probes use: the role is configured (it
// has a model), and this build ships the adapter for the protocol the role
// speaks (protocolAdapters). The result is one of the three states the page's
// contract names (docs/status/ui-coverage.md): NOT CONFIGURED, CONFIGURED or
// NOT AVAILABLE — never the credential, and never a claim the build cannot
// keep.
func settingsRoleState(cfg providers.RoleConfig) string {
	if !cfg.Configured() {
		modelEnv, _, _, _ := providers.RoleEnvVars(cfg.Role)
		return fmt.Sprintf("NOT CONFIGURED: no model configured (set %s)", modelEnv)
	}
	if _, ok := protocolAdapters[cfg.Protocol]; !ok {
		return fmt.Sprintf("NOT AVAILABLE: a model is configured, but protocol %q has no adapter implemented in this build", cfg.Protocol)
	}
	return "CONFIGURED"
}

// osLookupEnv is the environment the sections read. A variable indirection so
// a handler test can point the whole page at a fixture environment without
// mutating the process.
var osLookupEnv = os.LookupEnv

// settingsDataDir resolves the durable data directory the page reports. The
// catalogue store's own path is the answer when a store is wired (production
// and the e2e binary both wire one); without a store the bootstrap
// resolution of the environment is the fallback, and without either the page
// says unknown rather than guessing a path.
func (s *Server) settingsDataDir() string {
	if s.store != nil {
		if p := s.store.Path(); p != "" {
			return filepath.Dir(p)
		}
	}
	return bootstrap.ResolveDataDir("", osLookupEnv)
}

// settingsStorage builds the storage section: the data directory's base name
// (the absolute path is a host path and stays server-side, docs/spec/08),
// the free space the kernel reports for it, and the asset store's byte total.
func (s *Server) settingsStorage(ctx context.Context, dataDir string) storageView {
	view := storageView{
		AssetStoreBytes: -1,
		SpeechCache: speechView{
			Bytes: nil,
			Note: "The speech cache stores its WAVs as content-addressed blobs inside the " +
				"asset store (ADR-0023), so its share of the byte total is not distinguishable by size.",
		},
		Env: []string{bootstrap.EnvDataDir},
	}
	if dataDir == "" {
		return view // unknown: reported, not invented
	}
	view.DataDir = filepath.Base(dataDir)
	view.DataDirKnown = true

	assetRoot := filepath.Join(dataDir, "assets")
	if size, ok := dirSizeBytes(assetRoot); ok {
		view.AssetStoreBytes = size
	}

	if free, ok := dirFreeSpace(dataDir); ok {
		view.FreeSpaceBytes = free
		view.FreeSpaceKnown = true
	} else {
		view.FreeSpaceBytes = -1
	}
	return view
}

// dirSizeBytes sums the file bytes under dir. A missing directory reports a
// legitimate 0 (the store is simply empty); a read error reports ok=false so
// the caller can distinguish "measured 0" from "could not measure".
func dirSizeBytes(dir string) (int64, bool) {
	var total int64
	ok := true
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			ok = false
			return fs.SkipAll
		}
		if d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if !ok {
		return 0, false
	}
	return total, true
}

// dirFreeSpace asks the kernel how many bytes are free on the filesystem
// holding dir. Platforms without a statfs the stdlib can call report
// ok=false and the page shows the gap instead of a fabricated number.
func dirFreeSpace(dir string) (int64, bool) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * st.Bsize, true
}

// settingsGateway reports the shared-gateway state by presence only: which
// variable carries the endpoint, which carries the token, and whether each is
// non-empty. The values themselves never leave the process.
func (s *Server) settingsGateway(roles map[providers.Role]providers.RoleConfig) modelsGatewayView {
	tokenEnv := providers.EnvGatewayToken
	if v, ok := osLookupEnv(providers.EnvGatewayTokenEnv); ok && strings.TrimSpace(v) != "" {
		tokenEnv = strings.TrimSpace(v)
	}
	urlSet := false
	if v, ok := osLookupEnv(providers.EnvGatewayURL); ok {
		urlSet = strings.TrimSpace(v) != ""
	}
	tokenSet := false
	if v, ok := osLookupEnv(tokenEnv); ok {
		tokenSet = strings.TrimSpace(v) != ""
	}
	return modelsGatewayView{
		URLEnv:          providers.EnvGatewayURL,
		TokenEnvName:    tokenEnv,
		URLConfigured:   urlSet,
		TokenConfigured: tokenSet,
	}
}

// settingsRoles renders one row per model role, in providers.AllRoles'
// declaration order. The base URL collapses to "<configured>" (docs/spec/14),
// the token to a presence boolean, and the capabilities field to null: the
// API layer holds no live client in this build, so a capability finding would
// be a claim nobody probed.
func (s *Server) settingsRoles(roles map[providers.Role]providers.RoleConfig) []modelRoleView {
	out := make([]modelRoleView, 0, len(providers.AllRoles))
	for _, role := range providers.AllRoles {
		cfg := roles[role]
		envModel, envURL, envProto, envTimeout := providers.RoleEnvVars(role)
		base := ""
		if strings.TrimSpace(cfg.BaseURL) != "" {
			base = configuredMarker
		}
		out = append(out, modelRoleView{
			Role:            string(role),
			State:           settingsRoleState(cfg),
			Configured:      cfg.Configured(),
			Model:           cfg.Model,
			Protocol:        string(cfg.Protocol),
			BaseURL:         base,
			ReasoningEffort: string(cfg.ReasoningEffort),
			Timeout:         cfg.Timeout.String(),
			TokenConfigured: cfg.Token() != "",
			Capabilities:    nil,
			Env:             []string{envModel, envURL, envProto, envTimeout, providers.RoleReasoningEffortEnv(role)},
		})
	}
	return out
}

// settingsAudio renders the audio sink section from the same two variables the
// session runtime reads at wiring time (cmd/game-narrator's audioSinkFromEnv):
// the kind, defaulting to filesink, and the named device, defaulting to the
// platform's "default" sink. A kind the runtime would reject at boot shows up
// here with a note, so the page and the process cannot disagree silently.
func settingsAudio() audioView {
	const sinkEnv = "GAME_NARRATOR_AUDIO_SINK"
	const deviceEnv = "GAME_NARRATOR_AUDIO_DEVICE"
	kind, _ := osLookupEnv(sinkEnv)
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "filesink"
	}
	device, _ := osLookupEnv(deviceEnv)
	device = strings.TrimSpace(device)
	if device == "" {
		device = "default"
	}
	view := audioView{Sink: kind, Device: device, Env: []string{sinkEnv, deviceEnv}}
	switch kind {
	case "dummy", "filesink", "pipewire":
	default:
		view.Note = "The value named is not one this build accepts (dummy, filesink, pipewire); " +
			"the service would have failed to start with it."
	}
	return view
}

// settingsSecurity renders the listen address, the tailnet URL and the
// authentication posture. The address prefers the WithListen value (the
// bound address, when the host wired it) and falls back to the environment
// resolution; the tailnet is the startup-cached detection (never re-run at
// request time, never forwarded); the posture reports whether an admin
// account exists so a page rendered by a locked-out operator can say "no
// admin has claimed this appliance yet" instead of a bare 403.
func (s *Server) settingsSecurity(ctx context.Context) securityView {
	listen := s.listenAddr
	if listen == "" {
		listen = bootstrap.ResolveListen("", osLookupEnv)
	}
	view := securityView{Listen: listen, ListenEnv: bootstrap.EnvListen,
		Auth:    securityAuth{Mode: "cookie sessions and API tokens"},
		Tailnet: s.tailnetView()}
	if s.users != nil {
		if has, err := s.users.HasRole(ctx, store.RoleAdmin); err == nil {
			view.Auth.AdminConfigured = has
		}
	}
	return view
}

// settingsImports renders the import-policy section. Web search: the
// onboarding agent's web_search and fetch_page tools are registered in this
// build and their results are untrusted by the loop's trust rules
// (docs/spec/14); the private-endpoint allowlist is operator opt-in, and only
// its *count* crosses this boundary (the host:port entries are the operator's
// network topology, not a browser's concern). Agent limits: the budget the
// agent wiring pins, reported as a build constant — it has no environment
// form in this build, and pretending otherwise would be a mutable setting
// that does not exist.
func settingsImports() importsView {
	privateList, _ := osLookupEnv("GAME_NARRATOR_WEB_PRIVATE_ALLOW")
	hosts := 0
	for _, part := range strings.Split(privateList, ",") {
		if strings.TrimSpace(part) != "" {
			hosts++
		}
	}
	return importsView{
		WebSearch: webSearchView{
			Enabled:  true,
			Provider: "exa-mcp (keyless public endpoint, ADR-0018)",
			Note: "The onboarding agent's web_search and fetch_page tools are registered in this build; " +
				"web text and fetched pages are untrusted input the agent may quote but never execute (docs/spec/14).",
			EnvPrivateAllow:     "GAME_NARRATOR_WEB_ALLOW_PRIVATE / GAME_NARRATOR_WEB_PRIVATE_ALLOW",
			PrivateHostsAllowed: hosts,
		},
		Agent: agentView{
			MaxSteps:     24,
			MaxToolCalls: 48,
			Source:       "build constant (cmd/game-narrator's agent wiring; no environment override in this build)",
		},
	}
}

// mutableSettingEnv lists the variables that control the values the page
// renders, in a stable order, so the PUT answer and the UI notice name the
// same set.
func mutableSettingEnv() []string {
	envs := []string{
		providers.EnvGatewayURL,
		providers.EnvGatewayTokenEnv,
		bootstrap.EnvListen,
		bootstrap.EnvDataDir,
		"GAME_NARRATOR_AUDIO_SINK",
		"GAME_NARRATOR_AUDIO_DEVICE",
		"GAME_NARRATOR_WEB_ALLOW_PRIVATE",
		"GAME_NARRATOR_WEB_PRIVATE_ALLOW",
		// The update check (ADR-0088): disabled by GAME_NARRATOR_UPDATE_CHECK=0,
		// pointed at a mirror by the repo/base-URL pair.
		updates.EnvCheck,
		updates.EnvRepo,
		updates.EnvBaseURL,
	}
	for _, role := range providers.AllRoles {
		envModel, envURL, envProto, envTimeout := providers.RoleEnvVars(role)
		envs = append(envs, envModel, envURL, envProto, envTimeout, providers.RoleReasoningEffortEnv(role))
	}
	sort.Strings(envs)
	return envs
}

// joinStrings is the one place this file needs a comma-joined string.
func joinStrings(parts []string) string { return strings.Join(parts, ", ") }
