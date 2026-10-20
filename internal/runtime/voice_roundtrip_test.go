package runtime

// The end-to-end half of issue #11 (ADR-0101): a profile built on the PC —
// the speaker-linked roster (placeholder ww:spk:1 linked to the target), an
// approved character voice and an approved narrator voice — is exported,
// imported into a fresh store with its reference blobs, and the fresh
// appliance's speech wiring is then driven on its commit path: the committed
// speaker-linked line speaks with the imported character voice (the stable id
// and the original fingerprint), the unattributed line with the imported
// narrator voice, and the attributed-but-voiceless line ends
// skipped/narrator_fallback_disabled only when the dial turned the fallback
// off and skipped/no_approved_voice when the narrator has no voice at all.
//
// Everything is real except the OCR engine and the TTS provider: the store,
// the asset store, the manifest encode/validate, the import, the speaker-link
// resolver and the wiring are the production code, and the TTS fake records
// the identity of every voice it was asked to speak, so "which voice spoke
// the line" is an assertion rather than a faith statement. No game text and
// no voice samples appear (AGENTS.md rule 5).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/profileio"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// ── the scripted TTS seam -----------------------------------------------------

// roundTripTTS is the scripted Base checkpoint: it records the voice identity
// of every request (the assertion) and answers with a deterministic tone —
// a real, validated WAV.
type roundTripTTS struct {
	mu    sync.Mutex
	refs  []string
	toneS float64
}

func (f *roundTripTTS) Generate(_ context.Context, req tts.GenerateRequest) (tts.Audio, error) {
	f.record(req.Voice)
	return f.tone()
}

func (f *roundTripTTS) GenerateWithReference(_ context.Context, req tts.GenerateRequest, _ []byte, _ string) (tts.Audio, error) {
	f.record(req.Voice + ":ref")
	return f.tone()
}

func (f *roundTripTTS) record(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, v)
}

func (f *roundTripTTS) voices() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.refs))
	copy(out, f.refs)
	return out
}

func (f *roundTripTTS) tone() (tts.Audio, error) {
	wav := speech.MakeToneWAV(440, f.toneS, 16000)
	return tts.Audio{WAV: wav, SampleRate: 16000, Channels: 1, BitsPerSample: 16,
		Duration: time.Duration(f.toneS * float64(time.Second)), Model: "fake-base"}, nil
}

// ── the PC side -----------------------------------------------------------------

// pcSide is the machine the curator built the profile on: a store and an
// asset store, the exported manifest bytes, and the two approved voices'
// stable identities.
type pcSide struct {
	t               *testing.T
	ctx             context.Context
	store           *store.Store
	assets          *assets.Local
	manifest        []byte
	targetVoiceID   string
	narratorVoiceID string
	targetRef       string
	narratorRef     string
}

// newPcSide builds the profile the board story needs — a roster with the
// speaker link folded in (placeholder ww:spk:1 retired, its name in the
// target's aliases), one committed line per speaker shape (the linked line,
// the unattributed line, the voiceless character's line), and the two
// approved voices — publishes it, and exports the manifest.
func newPcSide(t *testing.T) *pcSide {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open pc store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("pc migrate: %v", err)
	}
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("pc assets: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC().Format(time.RFC3339)

	g := games.Game{
		ID: games.NewID(), Slug: "voice-rt", Title: "Voice Round Trip",
		Platform: "gamecube", UserTags: []byte(`["synthetic"]`), CreatedAt: now,
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("game fixture invalid: %v", err)
	}
	if err := st.CreateGame(ctx, g); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	ed := games.Edition{
		ID: games.NewID(), GameID: g.ID, Region: "US", Language: "en",
		Revision: "rev1", Locale: "en_US",
	}
	if err := ed.Validate(); err != nil {
		t.Fatalf("edition fixture invalid: %v", err)
	}
	if err := st.CreateEdition(ctx, ed); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	p, err := st.CreateDraftProfile(ctx, ed.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}

	scriptSHA := sha256.Sum256([]byte("synthetic script body"))
	source := games.Source{
		ID: games.NewID(), ProfileID: p.ID, SourceType: games.SourceUserFile,
		URI: "file:///dev/null/voice-rt-script.csv", RetrievedAt: now,
		ContentSHA256: hex.EncodeToString(scriptSHA[:]), MimeType: "text/csv",
		Rights: games.RightsUserSupplied, Trust: 1.0, Parser: "generic",
	}
	if err := source.Validate(); err != nil {
		t.Fatalf("source fixture invalid: %v", err)
	}
	if err := st.PutSources(ctx, p.ID, []games.Source{source}); err != nil {
		t.Fatalf("PutSources: %v", err)
	}

	// The roster the speaker link (ADR-0070) leaves behind: the placeholder
	// retired, its name folded into the target's aliases, the lines re-pointed.
	target := games.Character{
		ID: games.NewID(), ProfileID: p.ID, Name: "Captain Vayne",
		Aliases: json.RawMessage(`["Vayne","ww:spk:1"]`), Role: "captain",
		Status:             games.CharacterConfirmed,
		VerifiedSourceRefs: json.RawMessage(`["` + source.ID + `"]`),
		CreatedAt:          now,
	}
	placeholder := games.Character{
		ID: games.NewID(), ProfileID: p.ID, Name: "ww:spk:1",
		Aliases: json.RawMessage(`[]`), Role: "crew",
		Status: games.CharacterRetired, CreatedAt: now,
	}
	barnacle := games.Character{
		ID: games.NewID(), ProfileID: p.ID, Name: "Barnacle",
		Aliases: json.RawMessage(`[]`), Role: "crew",
		Status: games.CharacterConfirmed, CreatedAt: now,
	}
	for _, ch := range []games.Character{target, placeholder, barnacle} {
		if err := ch.Validate(); err != nil {
			t.Fatalf("character fixture invalid: %v", err)
		}
	}
	if err := st.PutCharacters(ctx, p.ID, []games.Character{target, placeholder, barnacle}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}

	scene := games.Scene{
		ID: games.NewID(), ProfileID: p.ID, Name: "The Bridge",
		Aliases: json.RawMessage(`[]`), CreatedAt: now,
	}
	if err := st.PutScenes(ctx, p.ID, []games.Scene{scene}); err != nil {
		t.Fatalf("PutScenes: %v", err)
	}
	region := games.Region{
		ID: games.NewID(), ProfileID: p.ID, SceneID: scene.ID, Label: "textbox",
		RegionType: games.RegionTextbox, DetectMethod: games.DetectManual,
		RectX: 0.07, RectY: 0.72, RectW: 0.86, RectH: 0.23,
		OCRSettings: json.RawMessage(`{"engine":"tesseract","psm":6,"language":"eng"}`),
		Revision:    1, UpdatedAt: now,
	}
	if err := region.Validate(); err != nil {
		t.Fatalf("region fixture invalid: %v", err)
	}
	if err := st.PutRegions(ctx, p.ID, []games.Region{region}); err != nil {
		t.Fatalf("PutRegions: %v", err)
	}

	line := func(occ string, speaker string, attributed bool) games.Message {
		text := "The " + occ + " line is a synthetic sentence."
		m := games.Message{
			ID: games.NewID(), ProfileID: p.ID, SourceID: source.ID,
			SourceMessageID: occ, OriginalText: text,
			NormalizedText: strings.ToLower(text), SpeechText: text,
			Language: "en", TextKind: games.TextDialogue, SceneID: scene.ID,
			AttributionStatus: games.AttributionUnknown,
			Revision:          1, CreatedAt: now, UpdatedAt: now,
		}
		if attributed {
			m.SpeakerID = speaker
			m.AttributionStatus = games.AttributionConfirmed
			m.AttributionConfidence = 0.9
			m.AttributionEvidence = json.RawMessage(`[{"method":"manual","note":"round-trip fixture"}]`)
		}
		return m
	}
	msgs := []games.Message{
		line("linked", placeholder.ID, true),
		line("unattributed", "", false),
		line("voiceless", barnacle.ID, true),
	}
	if err := st.PutMessages(ctx, p.ID, msgs, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}

	targetVoiceID, targetRef := seedRoundTripVoice(t, st, a, target.ID, 220)
	narratorVoiceID, narratorRef := seedRoundTripVoice(t, st, a, narratorCharacter, 330)

	_, v, err := st.GetProfile(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile before review: %v", err)
	}
	if err := st.SetProfileStatus(ctx, p.ID, games.StatusReview, v); err != nil {
		t.Fatalf("SetProfileStatus(review): %v", err)
	}
	_, v, err = st.GetProfile(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile before publish: %v", err)
	}
	published, err := st.Publish(ctx, p.ID, v, "curator@local")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	m, err := profileio.Export(ctx, st, published.ID, profileio.ExportOptions{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	file, err := profileio.Encode(m)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := profileio.ValidateDocument(file); err != nil {
		t.Fatalf("the PC's export does not satisfy its own schema: %v", err)
	}
	return &pcSide{
		t: t, ctx: ctx, store: st, assets: a, manifest: file,
		targetVoiceID: targetVoiceID, narratorVoiceID: narratorVoiceID,
		targetRef: targetRef, narratorRef: narratorRef,
	}
}

// seedRoundTripVoice gives one character (or the narrator constant) a single
// approved voice with a committed reference and returns its id and the
// reference blob's content address. The tone differs per call so the blobs
// differ: "this blob is absent" is then a per-voice fact, not a per-store one.
func seedRoundTripVoice(t *testing.T, st *store.Store, a *assets.Local, character string, freq float64) (string, string) {
	t.Helper()
	ctx := context.Background()
	ref := speech.MakeToneWAV(freq, 1.0, 16000)
	sum := sha256.Sum256(ref)
	refSHA := hex.EncodeToString(sum[:])
	id, _, err := a.PutStaged(ctx, bytes.NewReader(ref), refSHA)
	if err != nil {
		t.Fatalf("staging reference for %s: %v", character, err)
	}
	if _, err := a.Commit(ctx, id); err != nil {
		t.Fatalf("committing reference for %s: %v", character, err)
	}
	now := time.Now().UTC()
	profID := games.NewID()
	if err := st.CreateVoiceRevision(ctx, store.VoiceProfile{
		ID:                profID,
		Character:         character,
		Revision:          1,
		State:             store.VoiceStateApproved,
		DesignInstruction: "a calm synthetic voice",
		ReferenceAssetID:  refSHA,
		RefTranscript:     voices.SyntheticAuditionLines[0],
		ModelID:           "fake-base",
		Language:          "en",
		ConsentProvenance: "original_synthetic_design (automated test)",
		ApprovedBy:        "test",
		Fingerprint:       voices.Fingerprint(refSHA, voices.SyntheticAuditionLines[0], "fake-base"),
		Gain:              1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("creating approved voice for %s: %v", character, err)
	}
	return profID, refSHA
}

// ── the fresh appliance ---------------------------------------------------------

// freshSide is the board: a new store and asset store, the imported manifest,
// and a speech wiring built against the imported rows exactly the way a
// session builds it (the link-resolved speaker map, the profile row's dial).
type freshSide struct {
	t           *testing.T
	ctx         context.Context
	st          *store.Store
	a           *assets.Local
	hub         *events.Hub
	mem         *events.MemStore
	tts         *roundTripTTS
	w           *speechWiring
	sink        *dummy.Sink
	profile     games.Profile
	content     store.ProfileContent
	report      profileio.ImportManifestReport
	speakers    map[string]string
	generations map[string]int64
}

// newFreshSide imports pc's manifest into a fresh store. blobRefs are the
// reference blobs committed to the fresh asset store first — the pack case is
// both, the pointer-only case is the list minus the blob it withholds.
func newFreshSide(t *testing.T, pc *pcSide, blobRefs []string, narratorFallback bool) *freshSide {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("fresh migrate: %v", err)
	}
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("fresh assets: %v", err)
	}
	for _, ref := range blobRefs {
		// The blob the PC committed under this content address, byte for byte.
		b, err := readAsset(ctx, pc.assets, ref)
		if err != nil {
			t.Fatalf("reading reference %s from the PC store: %v", ref, err)
		}
		id, _, err := a.PutStaged(ctx, bytes.NewReader(b), ref)
		if err != nil {
			t.Fatalf("staging reference %s on the board: %v", ref, err)
		}
		if _, err := a.Commit(ctx, id); err != nil {
			t.Fatalf("committing reference %s on the board: %v", ref, err)
		}
	}

	reg := adapter.NewRegistry()
	if err := generic.Register(reg); err != nil {
		t.Fatalf("generic.Register: %v", err)
	}
	report, err := profileio.ImportManifest(ctx, profileio.Deps{Store: st, Assets: a, Adapters: reg},
		bytes.NewReader(pc.manifest), profileio.ImportManifestOptions{})
	if err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}

	content, err := st.GetProfileContent(ctx, report.ProfileID)
	if err != nil {
		t.Fatalf("GetProfileContent(fresh): %v", err)
	}
	profile := content.Profile

	// The speaker map the session builds (session.go): every attributed line
	// through the curator's link, unattributed lines left to the narrator.
	resolve := speakerLinkResolver(content.Characters)
	speakers := make(map[string]string, len(content.Messages))
	generations := map[string]int64{}
	g := int64(0)
	for _, msg := range content.Messages {
		g++
		generations[msg.ID] = g
		if msg.SpeakerID != "" {
			speakers[msg.ID] = resolve(msg.SpeakerID)
		}
	}

	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	tt := &roundTripTTS{toneS: 0.05}
	svc := &voices.Service{
		Store: st, Assets: a, Base: tt,
		Speech:     speech.New(a, speech.NewMemIndex(), 4),
		InstanceID: "primary", BaseModelID: "fake-base", BackendDigest: "test-backend-v1",
	}

	sink := dummy.New(1, 5)
	endCtx, endCancel := context.WithCancel(ctx)
	w := &speechWiring{
		id:               "roundtrip",
		log:              func(string, ...any) {},
		hub:              hub,
		voices:           svc,
		assets:           a,
		speakerByMessage: speakers,
		narratorFallback: narratorFallback,
		sem:              make(chan struct{}, speechWorkerSlots),
		utterances:       map[string]*utteranceInfo{},
		byGeneration:     map[int64]string{},
		genCancel:        map[int64]*synthSlot{},
		settled:          map[int64]bool{},
		volume:           1.0,
		endCtx:           endCtx,
		endCancel:        endCancel,
		sink:             newGainSink(sink),
		fence:            &generationFence{},
	}
	w.sched = speech.NewScheduler(w.sink, w.fence, &speechEventPublisher{hub: hub, w: w}, speech.Options{
		FencePeriod: 5 * time.Millisecond,
	})
	w.sched.Start(endCtx)

	f := &freshSide{
		t: t, ctx: ctx, st: st, a: a, hub: hub, mem: mem, tts: tt,
		w: w, sink: sink, profile: profile, content: content, report: report,
		speakers: speakers, generations: generations,
	}
	t.Cleanup(func() {
		endCancel()
		_ = w.sched.Stop()
		_ = st.Close()
		_ = hub.Close()
	})
	return f
}

// byText finds the fresh message row by its source-side identity (row ids are
// re-minted on import; the source message id is what survives the trip).
func (f *freshSide) byText(t *testing.T, occ string) games.Message {
	t.Helper()
	for _, m := range f.content.Messages {
		if m.SourceMessageID == occ {
			return m
		}
	}
	t.Fatalf("the fresh revision carries no line with source message id %q", occ)
	return games.Message{}
}

// characterByName reads a character row off the fresh roster.
func (f *freshSide) characterByName(t *testing.T, name string) games.Character {
	t.Helper()
	for _, c := range f.content.Characters {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the fresh roster has no character named %q", name)
	return games.Character{}
}

// commit drives the wiring's commit path with one committed, speakable line.
func (f *freshSide) commit(t *testing.T, occ string, generation int64) {
	t.Helper()
	msg := f.byText(t, occ)
	data, _ := json.Marshal(map[string]any{
		"dialogue_occurrence_id": "occ-" + occ,
		"message_id":             msg.ID,
		"region_id":              "",
		"generation":             generation,
		"text":                   msg.OriginalText,
		"speech_eligible":        true,
	})
	f.w.onCommit(events.Envelope{
		SchemaVersion: 1,
		SessionID:     f.w.id,
		TraceID:       "trace-" + occ,
		Name:          events.MessageCommitted,
		Data:          data,
	})
}

// eventsOf reads the session's event log, newest last.
func (f *freshSide) eventsOf() []events.Envelope {
	evs, err := f.mem.After(context.Background(), 0, events.Filter{SessionID: f.w.id}, 4096)
	if err != nil {
		f.t.Fatalf("read events: %v", err)
	}
	return evs
}

// waitTerminal polls until the generation's terminal speech.ended is in the log.
func (f *freshSide) waitTerminal(t *testing.T, desc string, generation int64) []events.Envelope {
	t.Helper()
	by := time.Now().Add(10 * time.Second)
	for {
		evs := f.eventsOf()
		for _, ev := range evs {
			if ev.Name == events.SpeechEnded && genOfEv(t, ev) == generation {
				return evs
			}
		}
		if time.Now().After(by) {
			for _, ev := range evs {
				f.t.Logf("   %d %s %s", ev.Seq, ev.Name, string(ev.Data))
			}
			f.t.Fatalf("%s: generation %d reached no terminal within 10s", desc, generation)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// terminalOf decodes one generation's speech.ended payload.
func (f *freshSide) terminalOf(t *testing.T, evs []events.Envelope, generation int64) map[string]any {
	t.Helper()
	for _, ev := range evs {
		if ev.Name != events.SpeechEnded || genOfEv(t, ev) != generation {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			f.t.Fatalf("decode speech.ended: %v", err)
		}
		return p
	}
	f.t.Fatalf("no speech.ended for generation %d", generation)
	return nil
}

// voiceOf reads the voice profile id the wiring resolved for one occurrence,
// the wiring's own correlation record (w.mu taken and released).
func (f *freshSide) voiceOf(t *testing.T, occ string) string {
	t.Helper()
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	for _, u := range f.w.utterances {
		if u.occurrence == "occ-"+occ {
			return u.voiceID
		}
	}
	f.t.Fatalf("the wiring has no utterance record for %q", occ)
	return ""
}

// characterOf reads the speaker the wiring resolved for one occurrence: the
// link-resolved character, or the narrator for an unattributed line.
func (f *freshSide) characterOf(t *testing.T, occ string) string {
	t.Helper()
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	for _, u := range f.w.utterances {
		if u.occurrence == "occ-"+occ {
			return u.character
		}
	}
	f.t.Fatalf("the wiring has no utterance record for %q", occ)
	return ""
}

// ── the test --------------------------------------------------------------------

// TestVoiceRoundTripNarration is the board story: export on the PC, import on
// the board, and the board narrates what the PC's committed lines say.
func TestVoiceRoundTripNarration(t *testing.T) {
	pc := newPcSide(t)

	// ── the full trip: both reference blobs present ─────────────────────────
	f := newFreshSide(t, pc, []string{pc.targetRef, pc.narratorRef}, true)
	if f.report.VoicesWritten != 2 {
		t.Fatalf("voices written = %d, want 2 (the target character and the narrator)", f.report.VoicesWritten)
	}
	if len(f.report.VoiceRefsMissing) != 0 {
		t.Fatalf("voice_refs_missing = %v, want none (both blobs made the pack)", f.report.VoiceRefsMissing)
	}

	// The import applied the export's narrator fallback to the fresh draft.
	if !f.profile.NarratorFallback {
		t.Fatalf("the imported draft's narrator fallback = off, want the export's on")
	}

	// The speaker link resolved: the committed line attributed to the
	// placeholder speaks for the target character — the fresh roster's row.
	target := f.characterByName(t, "Captain Vayne")
	linked := f.byText(t, "linked")
	if got := f.speakers[linked.ID]; got != target.ID {
		t.Fatalf("the linked line resolves to speaker %q, want the target character %q (the speaker link the curator made)", got, target.ID)
	}

	// The linked line speaks with the imported target voice: the stable id
	// that never left the PC, not a re-minted one.
	linkedGen := f.generations[linked.ID]
	f.commit(t, "linked", linkedGen)
	evs := f.waitTerminal(t, "the speaker-linked line", linkedGen)
	if v := f.voiceOf(t, "linked"); v != pc.targetVoiceID {
		t.Fatalf("the linked line resolved to voice %s, want the imported target voice %s (the stable id)", v, pc.targetVoiceID)
	}
	tv, err := f.st.GetVoiceRevision(f.ctx, pc.targetVoiceID)
	if err != nil {
		t.Fatalf("the imported target voice is not in the fresh store: %v", err)
	}
	if tv.Character != target.ID {
		t.Fatalf("the imported target voice points at character %s, want the fresh target row %s", tv.Character, target.ID)
	}
	term := f.terminalOf(t, evs, linkedGen)
	if term["result"] != "ended" || term["reason"] != nil {
		t.Fatalf("the linked line ended result=%v reason=%v, want ended with no reason", term["result"], term["reason"])
	}
	if term["voice_id"] != pc.targetVoiceID {
		t.Fatalf("the linked line's terminal names voice %v, want the imported target voice %s (the correlation on the event)", term["voice_id"], pc.targetVoiceID)
	}
	if c := f.characterOf(t, "linked"); c != target.ID {
		t.Fatalf("the linked line resolved to character %s, want the target %s", c, target.ID)
	}
	if f.sink.Starts() != 1 {
		t.Fatalf("the linked line started %d playback(s), want 1", f.sink.Starts())
	}

	// The unattributed line speaks with the imported narrator voice.
	narr := f.byText(t, "unattributed")
	narrGen := f.generations[narr.ID]
	f.commit(t, "unattributed", narrGen)
	evs = f.waitTerminal(t, "the unattributed line", narrGen)
	if v := f.voiceOf(t, "unattributed"); v != pc.narratorVoiceID {
		t.Fatalf("the unattributed line resolved to voice %s, want the imported narrator voice %s", v, pc.narratorVoiceID)
	}
	term = f.terminalOf(t, evs, narrGen)
	if term["result"] != "ended" || term["voice_id"] != pc.narratorVoiceID {
		t.Fatalf("the unattributed line ended result=%v voice_id=%v, want ended under the imported narrator voice", term["result"], term["voice_id"])
	}
	if c := f.characterOf(t, "unattributed"); c != narratorCharacter {
		t.Fatalf("the unattributed line resolved to character %s, want the narrator constant", c)
	}

	// The voiceless character's line, fallback on: the narrator speaks it,
	// and the utterance still names the character whose voice was missing.
	voiceless := f.byText(t, "voiceless")
	voicelessGen := f.generations[voiceless.ID]
	f.commit(t, "voiceless", voicelessGen)
	evs = f.waitTerminal(t, "the voiceless character's line", voicelessGen)
	if v := f.voiceOf(t, "voiceless"); v != pc.narratorVoiceID {
		t.Fatalf("the voiceless line resolved to voice %s, want the narrator voice %s (the fallback)", v, pc.narratorVoiceID)
	}
	term = f.terminalOf(t, evs, voicelessGen)
	if term["result"] != "ended" {
		t.Fatalf("the voiceless line ended result=%v, want ended (the narrator speaks it)", term["result"])
	}
	barnacle := f.characterByName(t, "Barnacle")
	if c := f.characterOf(t, "voiceless"); c != barnacle.ID {
		t.Fatalf("the voiceless line names character %s, want Barnacle %s (the narrator spoke it, the line is still his)", c, barnacle.ID)
	}

	// The TTS requests carried the imported identities: the registry name the
	// fresh appliance derives from the imported row (stable voice id, fresh
	// character, the reference bytes it committed) — the inline-reference
	// path the board takes before its Reconcile pass registers the voice.
	refBytes, err := readAsset(f.ctx, f.a, tv.ReferenceAssetID)
	if err != nil {
		t.Fatalf("reading the imported target reference: %v", err)
	}
	wantName, err := voices.RegistryName(pc.targetVoiceID, target.ID, tv.Revision, refBytes)
	if err != nil {
		t.Fatalf("deriving the registry name: %v", err)
	}
	if joined := strings.Join(f.tts.voices(), ","); !strings.Contains(joined, wantName) {
		t.Errorf("the TTS requests %v carry no request under the imported identity %s", f.tts.voices(), wantName)
	}

	// ── the dial off: the setting, not the roster, keeps the line silent ───
	fOff := newFreshSide(t, pc, []string{pc.targetRef, pc.narratorRef}, false)
	off := fOff.byText(t, "voiceless")
	offGen := fOff.generations[off.ID]
	fOff.commit(t, "voiceless", offGen)
	evs = fOff.waitTerminal(t, "the voiceless line with the dial off", offGen)
	term = fOff.terminalOf(t, evs, offGen)
	if term["result"] != "skipped" || term["reason"] != "narrator_fallback_disabled" {
		t.Fatalf("dial off: the line ended result=%v reason=%v, want skipped/narrator_fallback_disabled", term["result"], term["reason"])
	}
	if fOff.sink.Starts() != 0 {
		t.Fatalf("dial off: %d playback(s) started, want none", fOff.sink.Starts())
	}

	// ── the pointer-only trip: the narrator's blob never arrived ────────────
	// The target's blob made the pack; the narrator's did not. The narrator
	// voice is reported missing, not written — and the voiceless line is the
	// roster's skip, not the setting's.
	fPtr := newFreshSide(t, pc, []string{pc.targetRef}, true)
	if fPtr.report.VoicesWritten != 1 {
		t.Fatalf("pointer-only: voices written = %d, want 1 (the target; the narrator's blob is absent)", fPtr.report.VoicesWritten)
	}
	if len(fPtr.report.VoiceRefsMissing) != 1 || fPtr.report.VoiceRefsMissing[0] != pc.narratorRef {
		t.Fatalf("pointer-only: voice_refs_missing = %v, want the narrator's reference named (%s)", fPtr.report.VoiceRefsMissing, pc.narratorRef)
	}
	ptr := fPtr.byText(t, "voiceless")
	ptrGen := fPtr.generations[ptr.ID]
	fPtr.commit(t, "voiceless", ptrGen)
	evs = fPtr.waitTerminal(t, "the voiceless line on the pointer-only board", ptrGen)
	term = fPtr.terminalOf(t, evs, ptrGen)
	if term["result"] != "skipped" || term["reason"] != "no_approved_voice" {
		t.Fatalf("pointer-only: the line ended result=%v reason=%v, want skipped/no_approved_voice (even the narrator has no voice)", term["result"], term["reason"])
	}
	if _, err := fPtr.st.GetVoiceRevision(f.ctx, pc.narratorVoiceID); err == nil {
		t.Fatalf("the narrator voice row was written although its reference blob is absent on this appliance")
	}
}

// readAsset reads one committed asset's bytes back out of the store.
func readAsset(ctx context.Context, a *assets.Local, id string) ([]byte, error) {
	r, _, err := a.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
