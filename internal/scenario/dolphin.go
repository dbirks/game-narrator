package scenario

// dolphin.go is the captured-frame replay class for dolphin-origin scenarios
// (the ADR-0037 vocabulary: "captured-frame replay" — recorded frames through
// the real pipeline, here with the OCR answers recorded by the real Tesseract
// at capture time, docs/spec/11 "Fixture origins").
//
// The frames are real Dolphin screenshots recorded by the internal/dolphin
// fixture recorder while a drive ran against the user-supplied game image.
// A captured fixture of a commercial game is NEVER committed (AGENTS.md rule
// 5): it lives in the external, user-configured fixture dir
// ($GAME_NARRATOR_FIXTURE_DIR/<id>, default the gitignored artifacts/
// fixtures/) and a committed scenario references it by id + manifest SHA-256
// only. Replaying a missing or altered fixture reports NOT RUN with setup
// instructions — never a pass, never a silent skip (ADR-0037 "blocked ≠
// pass", applied to a recording that is absent).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/dolphin"
)

// fixtureIDRe is the recorder's fixture-id shape: one path segment, no dots
// (the loader enforces it; this keeps the error message honest).
const fixtureRootDefault = "artifacts/fixtures"

// fixtureRoot resolves the external fixture root: the env var wins (an
// absolute or CWD-relative path the operator chose), otherwise the
// gitignored artifacts/ tree under the repository root.
func fixtureRoot(repoRoot string) string {
	if v := os.Getenv(dolphin.EnvFixtureDir); v != "" {
		return v
	}
	return filepath.Join(repoRoot, fixtureRootDefault)
}

// notRunReason renders the NOT RUN verdict a dolphin scenario owes when its
// fixture is not (or no longer) what it claims to be: the failure plus the
// setup instructions that unblock it.
func notRunReason(sc *Scenario, fxDir string, cause error) string {
	return fmt.Sprintf(
		"NOT RUN: fixture %q is not usable at %s (%s). Setup: record it with the fixture recorder against the real image — "+
			"set %s, put dolphin-emu, Xvfb, ffmpeg, x11-utils and tesseract on PATH, then run "+
			"`go test -run TestRealRecordFixture ./internal/dolphin/` (or `make test-dolphin`); the fixture lands in "+
			"$%s/%s and this scenario's manifest_sha256 must be updated to the new manifest's SHA-256 if it was re-recorded.",
		sc.Input.Fixture, fxDir, cause, dolphin.EnvGameImage, dolphin.EnvFixtureDir, sc.Input.Fixture)
}

// RunDolphinScenario runs one dolphin-origin scenario: it verifies the
// fixture (manifest present, every frame hashes, the scenario's manifest
// reference matches), materializes the scenario's frame slice as a replay
// dir, and runs it through the SAME two-layer harness as the synthetic
// scenarios (real frame decoding, presence detection, the region tracker,
// the matcher, the session reducer, the decision/TTS clients and fences).
//
// The returned RunResult is StatusNotRun (never an error) when the fixture
// is absent, altered or re-recorded: the gate reports NOT RUN with setup
// instructions. Harness defects (a slice out of range, a bad profile) are
// errors.
//
// The materialized replay dir is caller-owned and removed when the scenario
// finishes — success, failure, or panic (a crashed worker must not leave a
// 200+ MB gn-scenario-dolphin-* dir under /tmp): runDolphinScenario defers
// the cleanup func materializeDolphinReplay returns.
func RunDolphinScenario(ctx context.Context, repoRoot string, sc *Scenario) (RunResult, error) {
	return runDolphinScenario(ctx, repoRoot, sc, "")
}

// runDolphinScenario is the core of RunDolphinScenario. baseDir is where the
// materialized replay dir is created ("" = the system temp dir, the
// os.MkdirTemp contract); tests pass a t.TempDir()-rooted base so the
// materialized dir lands where they can assert on it — and assert it is gone
// when the scenario finishes.
func runDolphinScenario(ctx context.Context, repoRoot string, sc *Scenario, baseDir string) (RunResult, error) {
	if sc.Origin != OriginDolphin {
		return RunResult{}, fmt.Errorf("scenario %s: RunDolphinScenario requires origin %q (got %q)", sc.ID, OriginDolphin, sc.Origin)
	}
	if sc.Status == StatusBlocked {
		return RunResult{ID: sc.ID, Catalog: sc.Catalog, Status: "blocked", Reason: sc.Reason, Faults: sc.Faults}, nil
	}
	if sc.Inference.TTS != InferenceNone && sc.Inference.Voice == "" {
		return RunResult{}, fmt.Errorf("scenario %s: inference.tts is %q but inference.voice is empty", sc.ID, sc.Inference.TTS)
	}

	fxDir := filepath.Join(fixtureRoot(repoRoot), sc.Input.Fixture)

	// Integrity gate: the manifest must exist and every frame must hash to
	// its claim. Any failure means "the recording is not here (or not this
	// one)" → NOT RUN with setup instructions.
	if _, err := dolphin.VerifyFixture(fxDir); err != nil {
		return RunResult{ID: sc.ID, Catalog: sc.Catalog, Status: StatusNotRun, Reason: notRunReason(sc, fxDir, err), Faults: sc.Faults}, nil
	}
	sha, err := dolphin.ManifestSHA256(fxDir)
	if err != nil {
		return RunResult{ID: sc.ID, Catalog: sc.Catalog, Status: StatusNotRun, Reason: notRunReason(sc, fxDir, err), Faults: sc.Faults}, nil
	}
	if strings.ToLower(sha) != strings.ToLower(sc.Input.ManifestSHA256) {
		return RunResult{ID: sc.ID, Catalog: sc.Catalog, Status: StatusNotRun, Reason: fmt.Sprintf(
			"NOT RUN: fixture %q at %s no longer matches the scenario's reference (manifest sha256 %s, recorded %s). "+
				"The fixture was re-recorded: update the scenario's manifest_sha256 against the new manifest (or restore the recording).",
			sc.Input.Fixture, fxDir, sha, sc.Input.ManifestSHA256), Faults: sc.Faults}, nil
	}

	// The scenario's frame slice (0-based, inclusive; absent = the whole
	// fixture) as a replay dir the harness machinery consumes unchanged. The
	// dir is caller-owned: materializeDolphinReplay hands back a cleanup func
	// and deferring it here removes the dir when the scenario finishes —
	// success, failure, or panic.
	replayDir, first, last, man, cleanup, err := materializeDolphinReplay(fxDir, sc.Input, baseDir)
	if err != nil {
		cleanup()
		return RunResult{}, fmt.Errorf("scenario %s: %w", sc.ID, err)
	}
	defer cleanup()

	profilePath := filepath.Join(fxDir, "profile.json")
	res, err := runScenarioCore(ctx, repoRoot, sc, replayDir, profilePath)
	if err != nil {
		return res, err
	}
	// The report material needs the fixture's manifest (frame labels, the
	// slice range) — carried on the result for the HTML report writer.
	res.Fixture = &DolphinFixtureInfo{
		ID:             sc.Input.Fixture,
		Dir:            fxDir,
		ManifestSHA256: sha,
		First:          first,
		Last:           last,
		Manifest:       man,
	}
	return res, nil
}

// DolphinFixtureInfo is the run's provenance for a dolphin scenario: the
// fixture the frames came from and the slice the scenario replayed. The
// report links the frames back to this fixture.
type DolphinFixtureInfo struct {
	ID             string
	Dir            string
	ManifestSHA256 string
	First          int // 0-based, inclusive
	Last           int // 0-based, inclusive
	Manifest       dolphin.FixtureManifest
}

// materializeDolphinReplay copies one slice of the fixture into a temp dir in
// the replay source's shape: the frame files, a sliced timestamps.json and a
// sliced truth.json (rows for the slice, in manifest order). The copy is the
// honest boundary: the harness reads the slice exactly as recorded, and the
// fixture itself is never mutated.
//
// The dir is created under baseDir ("" = os.TempDir(), the os.MkdirTemp
// contract: the production path materializes under the system temp dir) and
// is caller-owned. The returned cleanup removes the dir and must be called
// exactly once by the caller — deferred at the call site so the dir is gone
// when the scenario finishes, success, failure, or panic. cleanup is
// idempotent (a second call is a no-op) and a no-op when the dir was never
// created; the error paths remove a partially written dir before returning,
// so the caller's call only ever meets a dir that is already gone.
func materializeDolphinReplay(fxDir string, in Input, baseDir string) (dir string, first, last int, man dolphin.FixtureManifest, cleanup func(), err error) {
	man, err = dolphin.VerifyFixture(fxDir)
	if err != nil {
		cleanup = func() {}
		return "", 0, 0, man, cleanup, err
	}
	first, last = in.First, len(man.Frames)-1
	if in.Last != 0 {
		last = in.Last
	}
	if first < 0 || first > last || last >= len(man.Frames) {
		cleanup = func() {}
		return "", 0, 0, man, cleanup, fmt.Errorf("fixture %s: frame slice [%d..%d] is out of range (the fixture has %d frames)", man.ID, first, last, len(man.Frames))
	}

	dir, err = os.MkdirTemp(baseDir, "gn-scenario-dolphin-")
	if err != nil {
		cleanup = func() {}
		return "", 0, 0, man, cleanup, err
	}
	cleanup = func() { os.RemoveAll(dir) }

	rows := man.Frames[first : last+1]
	type tsEntry struct {
		File string `json:"file"`
		TsNS int64  `json:"ts_ns"`
	}
	ts := make([]tsEntry, 0, len(rows))
	for _, fr := range rows {
		ts = append(ts, tsEntry{File: fr.File, TsNS: fr.TsNS})
		if err := copyFile(filepath.Join(fxDir, "frames", fr.File), filepath.Join(dir, fr.File)); err != nil {
			cleanup()
			return "", 0, 0, man, cleanup, err
		}
	}
	if err := writeJSONFile(filepath.Join(dir, "timestamps.json"), ts); err != nil {
		cleanup()
		return "", 0, 0, man, cleanup, err
	}
	truthRaw, err := os.ReadFile(filepath.Join(fxDir, "truth.json"))
	if err != nil {
		cleanup()
		return "", 0, 0, man, cleanup, fmt.Errorf("fixture truth: %w", err)
	}
	var truthRows []map[string]any
	if err := json.Unmarshal(truthRaw, &truthRows); err != nil {
		cleanup()
		return "", 0, 0, man, cleanup, fmt.Errorf("fixture truth: parse: %w", err)
	}
	if len(truthRows) != len(man.Frames) {
		cleanup()
		return "", 0, 0, man, cleanup, fmt.Errorf("fixture %s: truth.json has %d rows, the manifest names %d frames", man.ID, len(truthRows), len(man.Frames))
	}
	if err := writeJSONFile(filepath.Join(dir, "truth.json"), truthRows[first:last+1]); err != nil {
		cleanup()
		return "", 0, 0, man, cleanup, err
	}
	return dir, first, last, man, cleanup, nil
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}

func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// LoadDolphinScenarios loads every committed scenario under
// testdata/scenarios/dolphin (the authoring surface for the M6
// captured-frame scenarios).
func LoadDolphinScenarios(repoRoot string) ([]string, error) {
	dir := filepath.Join(repoRoot, "testdata", "scenarios", "dolphin")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("dolphin scenarios: read %s: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths, nil
}

// DolphinRunBudget bounds the scenario runner test's wall clock: a dolphin
// slice paces the sidecar on the recording's clock, so a full-fixture replay
// takes roughly as long as the recorded span.
const DolphinRunBudget = 20 * time.Minute
