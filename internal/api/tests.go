package api

// The /tests page's API (docs/spec/09 "/tests": the scenario catalog with run
// controls, Arch Dolphin readiness, the frame-replay mode, live-provider and
// hardware test status, and the HTML report viewer/download).
//
// Everything this page renders is *state of this machine*: the scenarios the
// checkout under testdata/scenarios actually contains, the gate results this
// host recorded under artifacts/gates, whether the Dolphin binaries and game
// image exist here, and which model roles this process has configured. The
// page never presents a scenario as run until the job that ran it says so,
// and never presents a gate as passed until its recorded exit code says so
// (AGENTS.md "no simulation theater").

import (
	"encoding/xml"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/scenario"
)

// devTestsResponse is the GET /api/v1/dev/tests body.
type devTestsResponse struct {
	Scenarios []scenarioView   `json:"scenarios"`
	Gates     []gateView       `json:"gates"`
	Dolphin   dolphinReadyView `json:"dolphin"`
	Providers providerReady    `json:"providers"`
	// RepoRootKnown is false when no repository root could be resolved (the
	// page then renders the actionable "point the harness at a checkout"
	// state instead of an empty catalog that would read as "no scenarios").
	RepoRootKnown bool   `json:"repo_root_known"`
	RepoRootEnv   string `json:"repo_root_env"`
	ScenarioDir   string `json:"scenario_dir,omitempty"`
	GatesDir      string `json:"gates_dir,omitempty"`
}

type providerReady struct {
	Configured []string `json:"configured"`
	Missing    []string `json:"missing"`
}

type dolphinReadyView struct {
	DevModeEnabled  bool     `json:"dev_mode_enabled"`
	DolphinBinary   bool     `json:"dolphin_binary_present"`
	Xvfb            bool     `json:"xvfb_present"`
	GameImageEnvSet bool     `json:"game_image_env_set"`
	Env             []string `json:"env"`
}

type gateView struct {
	Name        string `json:"name"`
	Result      string `json:"result"` // PASS | FAIL | NOT RUN
	ExitCode    *int   `json:"exit_code"`
	FinishedAt  string `json:"finished_at,omitempty"`
	Tests       *int   `json:"tests"`
	Failures    *int   `json:"failures"`
	Errors      *int   `json:"errors"`
	Skipped     *int   `json:"skipped"`
	Instruction string `json:"instruction"`
}

type scenarioView struct {
	ID       string `json:"id"`
	Catalog  string `json:"catalog,omitempty"`
	File     string `json:"file"`
	Origin   string `json:"origin"`
	Mode     string `json:"mode"`
	Profile  string `json:"profile"`
	Decision string `json:"decision"`
	TTS      string `json:"tts"`
	Voice    string `json:"voice,omitempty"`
	Faults   int    `json:"faults"`
	Status   string `json:"status"` // runnable | blocked | load_error
	Reason   string `json:"reason,omitempty"`
}

// knownGates is the gate set docs/spec/16's gate list runs through
// artifacts/gates: a gate without a recorded .rc file here reports NOT RUN
// with its make instruction, which is the actionable form of "not run"
// (docs/spec/16: unavailable gates are reported, never silently skipped).
var knownGates = []struct {
	name, instruction string
}{
	{"test-unit", "make test-unit"},
	{"test-replay", "make test-replay"},
	{"test-ui", "make test-ui (needs `make build` first: the spec runs the packaged binary)"},
	{"test-dolphin", "make test-dolphin — needs dolphin-emu on PATH, Xvfb, a game image (GAME_NARRATOR_GAME_IMAGE_PATH) and GAME_NARRATOR_DEV_DOLPHIN=1"},
	{"test-models", "make test-models — needs the live provider endpoints (GAME_NARRATOR_ROLE_*_MODEL)"},
	{"test-hardware", "make test-hardware — needs the UNO Q hardware (UVC capture + USB audio)"},
	{"test-full", "make test-full"},
}

// handleDevTests answers GET /api/v1/dev/tests (developer role). Without a
// resolvable repository root the catalog and gates sections are empty and the
// response says the harness is not wired — the same honest-degradation rule
// the rest of the API applies to unwired subsystems.
func (s *Server) handleDevTests(w http.ResponseWriter, r *http.Request) {
	root, ok := s.harnessRoot()
	resp := devTestsResponse{
		RepoRootKnown: ok,
		RepoRootEnv:   EnvRepoRoot,
	}
	if ok {
		scenariosDir := filepath.Join(root, "testdata", "scenarios")
		gatesDir := filepath.Join(root, "artifacts", "gates")
		resp.ScenarioDir = "testdata/scenarios"
		resp.GatesDir = "artifacts/gates"
		var err error
		resp.Scenarios, err = listScenarios(scenariosDir)
		if err != nil {
			s.writeProblem(w, r, http.StatusInternalServerError, "scenario-catalog-unreadable",
				"Internal Error", "reading the scenario catalog failed: "+err.Error())
			return
		}
		resp.Gates = readGates(gatesDir)
	}
	resp.Dolphin = dolphinReadiness()
	resp.Providers = providerReadiness()
	writeJSON(w, http.StatusOK, resp)
}

// listScenarios walks the scenarios tree and loads every YAML. A file the
// loader rejects is cataloged with status load_error and the loader's message
// (which carries the line), so a broken fixture is visible on the page
// instead of silently absent from the catalog.
func listScenarios(dir string) ([]scenarioView, error) {
	var out []scenarioView
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".yaml") {
			return nil
		}
		view := scenarioView{File: filepath.ToSlash(strings.TrimPrefix(path, dir+string(filepath.Separator)))}
		sc, loadErr := scenario.Load(path)
		if loadErr != nil {
			view.Status = "load_error"
			view.Reason = loadErr.Error()
		} else {
			view.ID = sc.ID
			view.Catalog = sc.Catalog
			view.Origin = sc.Origin
			view.Mode = sc.Mode
			view.Profile = sc.Profile
			view.Decision = sc.Inference.Decision
			view.TTS = sc.Inference.TTS
			view.Voice = sc.Inference.Voice
			view.Faults = len(sc.Faults)
			if sc.Status == scenario.StatusBlocked {
				view.Status = "blocked"
				view.Reason = sc.Reason
			} else {
				view.Status = "runnable"
			}
		}
		out = append(out, view)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readGates renders the recorded gate results: every known gate gets a row,
// PASS/FAIL from its .rc exit code (0 = PASS; the gate script records the
// real code) and NOT RUN with its make instruction when the file is absent.
// The matching JUnit .xml supplies the test counts when present.
func readGates(dir string) []gateView {
	out := make([]gateView, 0, len(knownGates))
	for _, g := range knownGates {
		view := gateView{Name: g.name, Result: "NOT RUN", Instruction: g.instruction}
		rcPath := filepath.Join(dir, g.name+".rc")
		if data, err := os.ReadFile(rcPath); err == nil {
			if code, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				view.ExitCode = &code
				if code == 0 {
					view.Result = "PASS"
				} else {
					view.Result = "FAIL"
				}
			}
			if fi, statErr := os.Stat(rcPath); statErr == nil {
				view.FinishedAt = fi.ModTime().UTC().Format("2006-01-02T15:04:05Z")
			}
		}
		if xmlc, ok := readGateXML(filepath.Join(dir, g.name+".xml")); ok {
			view.Tests, view.Failures, view.Errors, view.Skipped = &xmlc.Tests, &xmlc.Failures, &xmlc.Errors, &xmlc.Skipped
		}
		out = append(out, view)
	}
	return out
}

// gateXML is the subset of the JUnit testsuites root this page renders.
type gateXML struct {
	XMLName  xml.Name `xml:"testsuites"`
	Tests    int      `xml:"tests,attr"`
	Failures int      `xml:"failures,attr"`
	Errors   int      `xml:"errors,attr"`
	Skipped  int      `xml:"skipped,attr"`
}

func readGateXML(path string) (gateXML, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return gateXML{}, false
	}
	var doc gateXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return gateXML{}, false
	}
	return doc, true
}

// dolphinReadiness reports the Arch-side prerequisites the Dolphin gate needs:
// the dev opt-in flag, the emulator and Xvfb binaries on PATH (presence only —
// the resolved path is an operator detail, the same rule as the image), and
// the game-image variable being set. No path, no image content: readiness is
// booleans and variable names (docs/spec/14).
func dolphinReadiness() dolphinReadyView {
	dev := false
	if v, ok := osLookupEnv(dolphin.EnvDevDolphin); ok && (v == "1" || strings.EqualFold(v, "true")) {
		dev = true
	}
	_, dolphinErr := lookPath("dolphin-emu")
	_, xvfbErr := lookPath("Xvfb")
	_, imageSet := osLookupEnv(dolphin.EnvGameImage)
	return dolphinReadyView{
		DevModeEnabled:  dev,
		DolphinBinary:   dolphinErr == nil,
		Xvfb:            xvfbErr == nil,
		GameImageEnvSet: imageSet,
		Env:             []string{dolphin.EnvDevDolphin, dolphin.EnvGameImage},
	}
}

// providerReadiness lists, by role name only, which model roles this process
// has configured and which it has not. The live-provider gate (test-models)
// is runnable only with the roles it exercises configured; the page shows the
// gap instead of a run that would fail with "no model".
func providerReadiness() providerReady {
	roles, err := providers.LoadRoles(osLookupEnv)
	if err != nil {
		return providerReady{}
	}
	out := providerReady{Configured: []string{}, Missing: []string{}}
	for _, role := range providers.AllRoles {
		if roles[role].Configured() {
			out.Configured = append(out.Configured, string(role))
		} else {
			out.Missing = append(out.Missing, string(role))
		}
	}
	return out
}
