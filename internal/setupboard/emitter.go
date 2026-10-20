package setupboard

// The emitter (ADR-0108): the single publisher of setup.* events. It sits at
// the two places a stage transition is real and durable — the onboarding
// agent's succeeded tool calls (the loop's SetupStages hook) and the voice
// workflow's take/approval moments (the service's StageObserver) — and it
// re-derives the affected character's stage from durable rows before every
// emission. The guard is what makes the stream replayable: a transition is
// emitted only when the derived stage advances, so a stale retry, a re-run
// of an old call or a duplicate event can never move a card backwards, and
// re-applying a replayed prefix is a no-op.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// Stage-relevant onboarding tools: the tool name maps to the stage its
// success is the durable fact for (ADR-0108's transition table). A tool
// outside this table is not a stage transition and is observed, then
// dropped.
var stageTools = map[string]Stage{
	"stage_character":   StageFinding, // discovered → finding
	"analyze_character": StageBrief,   // brief written → brief
	"design_voice":      StageVoicing, // TTS take requested → voicing
}

// Emitter is the setup board's event publisher and snapshot assembler.
// Every collaborator may be nil, and a nil collaborator degrades the emitter
// the way the rest of the appliance degrades: no hub means no events (the
// durable rows still advance, and the snapshot still answers); no store
// means no derivation (nothing is emitted, because nothing can be
// verified). An emitter that silently published an unverified transition
// would be a board that lies, so degradation is toward silence, never toward
// guesses.
//
// The monotonic guard is two-part: the emitted stage is the stage DERIVED
// from durable rows (never the tool's claim alone), and a card is re-emitted
// only when that derived stage is newer than the stage last emitted for it
// (lastEmitted, per run and character, for the process's lifetime). A stale
// retry, a re-run of an old call or a duplicate event therefore moves a card
// forward at most once per real transition, and a fresh process re-anchors
// at the derived stage (one replayable event, then silence).
type Emitter struct {
	hub   *events.Hub
	store *store.Store
	runs  *store.AgentStore
	// eventStore answers the snapshot's per-card `at` (the character's newest
	// setup.character_stage row in the durable log). Nil: cards carry no at.
	eventStore events.Store
	log        *slog.Logger

	mu          sync.Mutex
	lastEmitted map[string]Stage // run id + "\x00" + character id → last emitted stage
}

// New builds an emitter. hub, st, runs and evStore may be nil (see
// Emitter); log may be nil.
func New(hub *events.Hub, st *store.Store, runs *store.AgentStore, evStore events.Store, log *slog.Logger) *Emitter {
	if log == nil {
		log = slog.New(discardHandler{})
	}
	return &Emitter{hub: hub, store: st, runs: runs, eventStore: evStore, log: log,
		lastEmitted: map[string]Stage{}}
}

// runPlan is the subset of the onboarding run's frozen plan the board reads
// (the API's onbPlan shape, mirrored here so the board has no api import).
type runPlan struct {
	Title string `json:"title"`
}

// runFacts is one run's durable inputs, loaded once per transition and per
// board: everything a stage or rail derivation reads, so the derivation is a
// pure function of the facts and the snapshot and the events agree by
// construction.
type runFacts struct {
	run        store.AgentRun
	plan       runPlan
	profile    *games.Profile // nil: the run's profile row is gone
	characters []games.Character
	// toolSuccess[toolName][characterID] = a succeeded call of that tool
	// named that character in this run's log.
	toolSuccess map[string]map[string]bool
	// importSucceeded is the dialogue cell's durable signal: a script
	// entering tool (import_user_asset / import_web_source) succeeded on
	// the run. It is tracked outside toolSuccess on purpose: toolSuccess
	// only holds the stage-relevant tools, and the rail needs the import
	// row whether or not it moves a card.
	importSucceeded bool
	// lines[characterID] is the character's cited-line count.
	lines      map[string]int
	totalLines int
	// voiceRevisions[characterName] is that name's voice revisions.
	voiceRevisions       map[string][]store.VoiceProfile
	revisionHasAuditions map[string]bool // voice revision id → ≥1 audition
}

// ObserveTool is the onboarding agent loop's SetupStages hook (ADR-0108):
// the loop calls it after each SUCCEEDED tool call with the run id, the run's
// profile (once known) and the result envelope. It maps the three
// stage-relevant tools to their transition and emits when the derived stage
// advances. A tool that is not stage-relevant, a result without a character,
// and a transition the derived state already reflects are all silent by
// design: the board moves on evidence, and only forward.
//
// It carries no request context, the same shape the loop's own event
// emissions use (render.go): the observer runs in the loop's critical
// section under a bounded background deadline, so a wedged store read cannot
// hold the run hostage while a cancelled run simply stops observing.
func (e *Emitter) ObserveTool(runID, profileID, toolName string, res agent.Result) {
	want, ok := stageTools[toolName]
	if !ok || e.store == nil || e.runs == nil {
		return
	}
	charID, name, ok := characterFromResult(res.Results)
	if !ok || charID == "" {
		e.log.Debug("setupboard: tool result carries no character; no stage transition",
			"run_id", runID, "tool", toolName)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if profileID == "" {
		if run, err := e.runs.GetAgentRun(ctx, runID); err == nil {
			profileID = run.ProfileID
		}
	}
	e.transition(ctx, runID, profileID, charID, name, want)
}

// AuditionsReady is the voices workflow's take transition (ADR-0108):
// the durable job recorded the revision's auditions, which is the moment a
// TTS take was actually requested and generated on the queue-backed path.
// It satisfies the voices.Service StageObserver seam.
func (e *Emitter) AuditionsReady(ctx context.Context, revisionID string) {
	e.voiceTransition(ctx, revisionID, StageVoicing)
}

// VoiceApproved is the voices workflow's approval transition (ADR-0108):
// a take is frozen as the character's current voice, by a human or by the
// auto-cast default (UX-03 re-uses this same consent-gated Approve, so the
// board sees both as the same fact). It satisfies the voices.Service
// StageObserver seam.
func (e *Emitter) VoiceApproved(ctx context.Context, revisionID string) {
	e.voiceTransition(ctx, revisionID, StageDone)
}

// voiceTransition resolves a name-keyed voice revision to a board run and
// emits the transition when the derived stage advances. Voice work outside
// any setup run (a Cast page on a published game) resolves to no run and is
// skipped: the board is the setup page, and the Cast page reads its state
// from the voices API (ADR-0108).
func (e *Emitter) voiceTransition(ctx context.Context, revisionID string, want Stage) {
	if e.store == nil || e.runs == nil {
		return
	}
	rev, err := e.store.GetVoiceRevision(ctx, revisionID)
	if err != nil {
		e.log.Warn("setupboard: cannot load voice revision; no stage transition",
			"voice_id", revisionID, "error", err)
		return
	}
	name := strings.TrimSpace(rev.Character)
	run, ok, err := e.runs.LatestAgentRunByCharacter(ctx, name)
	if err != nil {
		e.log.Warn("setupboard: run resolution failed; no stage transition",
			"character", name, "error", err)
		return
	}
	if !ok {
		e.log.Debug("setupboard: voice transition outside any setup run; no board to update",
			"character", name)
		return
	}
	// The character id comes from the run's own roster (the voice row is
	// name-keyed; the board's cards are id-keyed).
	var charID string
	for _, c := range listCharacters(e.store, ctx, run.ProfileID) {
		if strings.EqualFold(c.Name, name) {
			charID = c.ID
			break
		}
	}
	if charID == "" {
		e.log.Debug("setupboard: character name not on the run's roster; no stage transition",
			"run_id", run.ID, "character", name)
		return
	}
	e.transition(ctx, run.ID, run.ProfileID, charID, name, want)
}

// listCharacters is the roster read the voice path uses to turn a name-keyed
// voice revision back into an id-keyed card. A store fault degrades to an
// empty roster (the caller then answers "no card for this name", which is
// the honest shape of "I cannot see the roster").
func listCharacters(st *store.Store, ctx context.Context, profileID string) []games.Character {
	if profileID == "" {
		return nil
	}
	chars, err := st.ListCharacters(ctx, profileID)
	if err != nil {
		return nil
	}
	return chars
}

// characterFromResult pulls the identity a stage event names out of a
// tool result envelope. The three stage tools all answer with
// character_id + a name field (character_name for the voice tools, name for
// stage_character's staged row), so both shapes are read.
func characterFromResult(results json.RawMessage) (id, name string, ok bool) {
	if len(results) == 0 {
		return "", "", false
	}
	var out struct {
		CharacterID   string `json:"character_id"`
		CharacterName string `json:"character_name"`
		Name          string `json:"name"`
	}
	if err := json.Unmarshal(results, &out); err != nil {
		return "", "", false
	}
	name = strings.TrimSpace(out.CharacterName)
	if name == "" {
		name = strings.TrimSpace(out.Name)
	}
	return strings.TrimSpace(out.CharacterID), name, out.CharacterID != ""
}

// loadFacts assembles one run's durable facts. A missing run or a store
// fault is a nil (with the error), because an unverified board is worse than
// no board.
func (e *Emitter) loadFacts(ctx context.Context, runID, profileID string) (*runFacts, error) {
	if e == nil || e.store == nil || e.runs == nil {
		return nil, errors.New("setupboard: emitter has no store wired")
	}
	run, err := e.runs.GetAgentRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	if profileID == "" {
		profileID = run.ProfileID
	}
	f := &runFacts{
		run:                  run,
		toolSuccess:          map[string]map[string]bool{},
		lines:                map[string]int{},
		voiceRevisions:       map[string][]store.VoiceProfile{},
		revisionHasAuditions: map[string]bool{},
	}
	if len(run.PlanJSON) > 0 {
		_ = json.Unmarshal(run.PlanJSON, &f.plan)
	}
	if profileID != "" {
		chars, err := e.store.ListCharacters(ctx, profileID)
		if err != nil {
			return nil, fmt.Errorf("list characters: %w", err)
		}
		f.characters = chars
		sort.Slice(f.characters, func(i, j int) bool {
			if f.characters[i].Name != f.characters[j].Name {
				return f.characters[i].Name < f.characters[j].Name
			}
			return f.characters[i].ID < f.characters[j].ID
		})
		if f.totalLines, err = e.store.CountGraphNodes(ctx, profileID, store.MessageFilter{}); err != nil {
			return nil, fmt.Errorf("count lines: %w", err)
		}
		if profile, _, err := e.store.GetProfile(ctx, profileID); err == nil {
			f.profile = &profile
		}
	}
	// The run's succeeded stage tools, by character (brief/voicing signals):
	// the tool call log is the durable record of what the agent did.
	calls, err := e.runs.ListAgentToolCalls(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list tool calls: %w", err)
	}
	for _, c := range calls {
		if c.Status != "succeeded" {
			continue
		}
		if c.Name == "import_user_asset" || c.Name == "import_web_source" {
			f.importSucceeded = true
		}
		if _, relevant := stageTools[c.Name]; !relevant {
			continue
		}
		var id string
		if c.ResultJSON != nil {
			if id, _, _ = characterFromResult(c.ResultJSON); id == "" {
				var args struct {
					CharacterID string `json:"character_id"`
				}
				if json.Unmarshal(c.ArgsJSON, &args) == nil {
					id = args.CharacterID
				}
			}
		}
		if id == "" {
			continue
		}
		if f.toolSuccess[c.Name] == nil {
			f.toolSuccess[c.Name] = map[string]bool{}
		}
		f.toolSuccess[c.Name][id] = true
	}
	// Cited lines per character and the voice revisions per name: the
	// done/voicing signals. Bounded by the roster and the run's calls.
	for _, c := range f.characters {
		n, err := e.store.CountGraphNodes(ctx, profileID, store.MessageFilter{SpeakerID: c.ID})
		if err == nil {
			f.lines[c.ID] = clampLines(n)
		}
	}
	nameSet := map[string]bool{}
	for _, c := range f.characters {
		nameSet[c.Name] = true
	}
	for name := range nameSet {
		revs, err := e.store.ListVoiceProfiles(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("list voice profiles: %w", err)
		}
		f.voiceRevisions[name] = revs
		for _, r := range revs {
			if f.revisionHasAuditions[r.ID] {
				continue
			}
			auds, err := e.store.ListAuditions(ctx, r.ID)
			if err != nil {
				return nil, fmt.Errorf("list auditions: %w", err)
			}
			f.revisionHasAuditions[r.ID] = len(auds) > 0
		}
	}
	return f, nil
}

// stageFor derives one character's column from the durable facts
// (ADR-0108's derivation table). It is the single function both the emitter
// and the snapshot use, which is why "snapshot equals replayed events" is a
// property of the code rather than of a test run.
func stageFor(f *runFacts, ch games.Character) Stage {
	// The voice facts, by the character's display name (the voice surface's
	// own identity). A regeneration opens a newer draft above the approved
	// revision: the approved take is no longer the current one, so the card
	// reads voicing again until the new take is approved (UX-03d).
	var (
		latestRev, approvedRev int
		takeExists             bool
	)
	for _, r := range f.voiceRevisions[ch.Name] {
		if r.Revision > latestRev {
			latestRev = r.Revision
		}
		if r.State == store.VoiceStateApproved && r.Revision > approvedRev {
			approvedRev = r.Revision
		}
		if f.revisionHasAuditions[r.ID] {
			takeExists = true
		}
	}
	switch {
	case approvedRev > 0 && approvedRev == latestRev:
		return StageDone
	case approvedRev > 0:
		return StageVoicing // a newer take supersedes the approved one
	case takeExists:
		return StageVoicing
	}
	if id := ch.ID; f.toolSuccess["design_voice"][id] {
		return StageVoicing
	}
	if f.toolSuccess["analyze_character"][ch.ID] {
		return StageBrief
	}
	return StageFinding
}

// transition is the monotonic guard plus the emission. It loads the run's
// facts, derives the character's stage from the durable rows, and emits
// setup.character_stage (then setup.progress) only when the derived stage is
// newer than the stage last emitted for this character in this process. The
// emitted stage is the derivation, not the tool's claim: the claim (want) is
// what woke the observer, the rows are what it publishes.
func (e *Emitter) transition(ctx context.Context, runID, profileID, charID, name string, want Stage) {
	f, err := e.loadFacts(ctx, runID, profileID)
	if err != nil {
		e.log.Warn("setupboard: cannot derive stage; no event emitted",
			"run_id", runID, "error", err)
		return
	}
	var ch *games.Character
	for i := range f.characters {
		if f.characters[i].ID == charID {
			ch = &f.characters[i]
			break
		}
	}
	if ch == nil {
		// The tool said it staged the character but the roster does not show
		// it: the write and the read disagree, and a card the store cannot
		// see is not emitted (the durable row is the authority).
		e.log.Debug("setupboard: character not on the run's roster; no stage transition",
			"run_id", runID, "character_id", charID)
		return
	}
	cur := stageFor(f, *ch)
	if cur.rank() < want.rank() {
		// The tool claims a stage the durable rows do not yet reflect (the
		// call's row landed after the hook fired, or the write did not
		// stick). The claim is not yet a fact, so nothing is emitted and the
		// next observation (or the snapshot) catches up from the rows.
		return
	}
	key := runID + "\x00" + charID
	e.mu.Lock()
	last := e.lastEmitted[key]
	if cur.rank() <= last.rank() {
		e.mu.Unlock()
		return // same column or backwards: the board does not move
	}
	e.lastEmitted[key] = cur
	e.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	if e.hub != nil {
		payload := CharacterStage{
			RunID: runID, CharacterID: ch.ID, Name: clipName(name),
			Stage: cur, Lines: clampLines(f.lines[ch.ID]), At: now,
		}
		e.emit(ctx, EventCharacterStage, payload)
	}
	// The run-level tile/rail state follows every card move: the board's
	// three stat tiles and the six-cell rail are one progress event behind
	// the move that changed them.
	e.emitProgress(ctx, f)
}

// Board is the GET /api/v1/onboarding/runs/{id}/board snapshot (ADR-0108).
// Every field is derived from the same durable facts the events carry — the
// characters' stages and lines and the rail and the tiles — and `at` is the
// character's newest setup.character_stage row in the durable event log, the
// same rows the SSE Last-Event-ID path replays.
func (e *Emitter) Board(ctx context.Context, runID string) (Board, error) {
	if e.store == nil || e.runs == nil {
		return Board{}, fmt.Errorf("setupboard: no store wired; the board cannot be derived")
	}
	f, err := e.loadFacts(ctx, runID, "")
	if err != nil {
		return Board{}, err
	}
	atByChar := e.lastStageTimes(ctx, f.run.ID)
	board := Board{
		RunID:     f.run.ID,
		State:     f.run.State,
		CreatedAt: f.run.CreatedAt.UTC().Format(time.RFC3339),
	}
	for i := range f.characters {
		ch := f.characters[i]
		board.Characters = append(board.Characters, BoardCharacter{
			CharacterID: ch.ID,
			Name:        ch.Name,
			Stage:       stageFor(f, ch),
			Lines:       clampLines(f.lines[ch.ID]),
			At:          atByChar[ch.ID],
		})
	}
	board.Progress = e.progress(f)
	return board, nil
}

// lastStageTimes reads the durable event log's setup.character_stage rows and
// keeps the newest per character. A log with no rows (the hub was unwired
// when the transitions happened, or the run is young) answers empty times.
func (e *Emitter) lastStageTimes(ctx context.Context, runID string) map[string]string {
	out := map[string]string{}
	if e.eventStore == nil {
		return out
	}
	envs, err := e.eventStore.After(ctx, 0, events.Filter{
		Names: []string{EventCharacterStage},
	}, 0)
	if err != nil {
		e.log.Warn("setupboard: cannot read the event log for card times", "error", err)
		return out
	}
	for _, ev := range envs { // ascending seq: later rows overwrite earlier
		var p CharacterStage
		if err := json.Unmarshal(ev.Data, &p); err != nil || p.RunID != runID {
			continue
		}
		out[p.CharacterID] = p.At
	}
	return out
}

// progress computes the setup.progress payload from the facts: the three
// stat tiles (lines read · characters · voiced) and the six-cell rail.
func (e *Emitter) progress(f *runFacts) Progress {
	stages := make([]Stage, 0, len(f.characters))
	voiced := 0
	briefed := 0
	for _, ch := range f.characters {
		st := stageFor(f, ch)
		stages = append(stages, st)
		if st.rank() >= StageVoicing.rank() {
			voiced++
		}
		if st.rank() >= StageBrief.rank() {
			briefed++
		}
	}
	p := Progress{
		RunID:      f.run.ID,
		LinesRead:  clampLines(f.totalLines),
		Characters: len(f.characters),
		Voiced:     voiced,
		Total:      len(f.characters),
		Elapsed:    elapsedSeconds(f.run.CreatedAt),
	}
	p.StageRail = e.rail(f, stages, briefed)
	return p
}

// rail is the six-cell strip (plan.md item 4). A cell is done when its
// durable signal holds, active when every earlier cell is done and it is not
// (the run's frontier), and pending otherwise. The values are the rail's
// number slots: the run's title, the line count, the counts the tiles show,
// and the profile status on the publish cell.
func (e *Emitter) rail(f *runFacts, stages []Stage, briefed int) []RailCell {
	n := len(f.characters)
	done := [6]bool{}
	// 01 Find game: the run was created with the game named (the plan freezes
	// the title and the catalogue half created the game row), so the cell is
	// done by construction; its value is the title the strip renders.
	done[0] = true
	// 02 Dialogue: a script entered the profile (an import tool succeeded).
	done[1] = e.importToolSucceeded(f)
	// 03 Characters: the roster is non-empty.
	done[2] = n > 0
	// 04 Briefs: every character has reached the brief column.
	done[3] = n > 0 && briefed == n
	// 05 Voices: every character has reached the voicing column.
	done[4] = n > 0
	for _, st := range stages {
		if st.rank() < StageVoicing.rank() {
			done[4] = false
			break
		}
	}
	// 06 Publish: the profile is published.
	status := ""
	if f.profile != nil {
		status = string(f.profile.Status)
	}
	done[5] = status == string(games.StatusPublished)

	vals := [6]string{
		f.plan.Title,
		fmt.Sprintf("%d lines", clampLines(f.totalLines)),
		fmt.Sprintf("%d characters", n),
		fmt.Sprintf("%d/%d briefs", briefed, n),
		fmt.Sprintf("%d/%d voiced", countStageAtLeast(stages, StageVoicing), n),
		orDefault(status, "draft"),
	}
	cells := make([]RailCell, 0, len(railOrder))
	for i, rc := range railOrder {
		state := CellPending
		switch {
		case done[i]:
			state = CellDone
		default:
			allEarlier := true
			for j := 0; j < i; j++ {
				if !done[j] {
					allEarlier = false
					break
				}
			}
			if allEarlier {
				state = CellActive
			}
		}
		cells = append(cells, RailCell{ID: rc.id, Label: rc.label, State: state, Value: vals[i]})
	}
	return cells
}

// importToolSucceeded reports whether a script-entering tool call succeeded
// in this run (the dialogue cell's durable signal).
func (e *Emitter) importToolSucceeded(f *runFacts) bool {
	return f.importSucceeded
}

func countStageAtLeast(stages []Stage, at Stage) int {
	n := 0
	for _, st := range stages {
		if st.rank() >= at.rank() {
			n++
		}
	}
	return n
}

func elapsedSeconds(created time.Time) int64 {
	if created.IsZero() {
		return 0
	}
	s := int64(time.Since(created).Seconds())
	if s < 0 {
		return 0
	}
	return s
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

// emit publishes one typed event. A hub fault (closed, store append
// failure) is logged, not returned: a stage event that cannot be published
// must not fail the tool call that is the real, durable transition — the
// durable row has already happened, and the board snapshot still derives the
// same stage from it (the events are the fast path, the derivation is the
// authority).
func (e *Emitter) emit(ctx context.Context, name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		e.log.Error("setupboard: cannot encode event payload", "name", name, "error", err)
		return
	}
	_, err = e.hub.Publish(ctx, events.Envelope{Name: name, Data: data})
	if err != nil && !isHubGone(err) {
		e.log.Warn("setupboard: event publish failed", "name", name, "error", err)
	}
}

// emitProgress publishes setup.progress for the run's current facts.
func (e *Emitter) emitProgress(ctx context.Context, f *runFacts) {
	if e.hub == nil {
		return
	}
	e.emit(ctx, EventProgress, e.progress(f))
}

// isHubGone reports the publish errors that mean "the stream is over" (hub
// closed, context done) rather than "the durable log faulted": the former
// are the ordinary end of a run's process, not a fault worth a warning line.
func isHubGone(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, events.ErrClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// discardHandler swallows the log lines an unwired logger would not have
// anywhere to go (the emitter degrades toward silence, not toward a panic on
// a missing logger).
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }
