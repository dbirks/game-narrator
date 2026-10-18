package runtime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	ses "github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/speech/filesink"
	"github.com/dbirks/game-narrator/internal/speech/pipewire"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// Audio sink kinds for a session's speech pipeline (docs/spec/07 "Audio
// scheduler": "audio sink abstraction also includes dummy/file/capture for
// CI and tests").
const (
	// SinkDummy plays nothing but paces and records: the test default.
	SinkDummy = "dummy"
	// SinkFile records, in real time, exactly the samples that would have
	// played, one WAV per utterance (docs/spec/11 evidence).
	SinkFile = "filesink"
	// SinkPipeWire streams to the PipeWire/PulseAudio device of the
	// configured stable identity (docs/spec/07: "Use USB ALSA/PipeWire sink
	// by stable device identity").
	SinkPipeWire = "pipewire"
)

// narratorCharacter is the conventional character id of the fallback voice
// (internal/voices: "A voice profile is one per character (or the narrator)"):
// a line whose speaker is unattributed, or whose character has no approved
// voice, is spoken by the narrator voice when one is configured.
const narratorCharacter = "narrator"

// speechTTSTimeout is one utterance's synthesis deadline: the voice_clone
// role's documented timeout floor (internal/providers roleSpecs).
const speechTTSTimeout = 120 * time.Second

// speechWorkerSlots bounds the concurrent TTS generations per session; the
// shared cache's limiter bounds the provider concurrency process-wide.
const speechWorkerSlots = 4

// speechStopDrainBound is stop()'s bound for the in-flight speak workers (a
// line still in the TTS call or in the operator's delay) to reach their
// session-end terminal events before the session's own go out. It is the
// same order as the scheduler's own stop bound; a provider wedged past it
// degrades to the late event rather than holding the run's finalisation.
const speechStopDrainBound = 2 * time.Second

// speechDelayMaxMS is the operator's maximum speech delay (docs/spec/07
// "optional delay calibration"); the API clamps to this range.
const speechDelayMaxMS = 2000

// byGenerationCap bounds the generation→occurrence table the scheduler's
// cancel events (which carry no utterance id) are correlated through.
const byGenerationCap = 64

// SpeechOptions wires the per-session speech pipeline into the Manager
// (docs/spec/07). A nil *SpeechOptions on Options.Speech means the voice
// workflow is not configured in this process: sessions run silent and the
// audio API answers 503, the documented degraded mode — never a substitute
// voice.
type SpeechOptions struct {
	// Voices is the live voice service: the approved revisions, the
	// per-instance registrations and the Base (clone) TTS client the
	// utterances are synthesised with.
	Voices *voices.Service
	// Assets loads the approved reference clips for the inline-reference
	// fallback (docs/spec/06) when a voice is not registered on this
	// instance.
	Assets assets.Store
	// Sink selects the audio sink: "dummy", "filesink" (the default) or
	// "pipewire".
	Sink string
	// FileSinkDir is the filesink root; each session writes under
	// <dir>/<session id>. Empty with Sink=filesink is a construction error.
	FileSinkDir string
	// PipeWireTarget is the stable device identity the pipewire sink
	// streams to; "default" when empty.
	PipeWireTarget string
	// SinkFactory is the test seam: when non-nil it supplies the sink
	// instead of Sink (tests need the programmable dummy sink's records).
	// Production wiring (cmd/game-narrator) leaves it nil.
	SinkFactory func() (speech.AudioSink, error)
}

// AudioSettings is the operator's live audio control set
// (POST /api/v1/sessions/{id}/audio): each field is applied when present,
// the others keep their current value.
type AudioSettings struct {
	// Muted skips playback: the pipeline still resolves the voice and
	// synthesises (the cache is filled) but the utterance ends as
	// result:"muted" instead of starting.
	Muted *bool `json:"muted,omitempty"`
	// Volume scales the samples in the sink path, 0..1 (1.0 = unity).
	Volume *float64 `json:"volume,omitempty"`
	// SpeechDelayMS waits this long before starting playback (0..2000).
	// The wait is fenced: a commit during the delay still cancels the
	// waiting utterance at the scheduler's fence.
	SpeechDelayMS *int `json:"speech_delay_ms,omitempty"`
}

// SpeechCounters are one session's speech decision counters, the shape
// docs/spec/13 exposes to a report: the counts that make "did it speak,
// and why not" answerable.
type SpeechCounters struct {
	// Requested: commits the speech pipeline was asked to speak.
	Requested int64 `json:"requested"`
	// Ready: utterances for which audio was resolved (cache or TTS).
	Ready int64 `json:"ready"`
	// Started: playbacks the scheduler started on a sink.
	Started int64 `json:"started"`
	// Cancelled: cancellations announced (new commit, stale generation,
	// session end).
	Cancelled int64 `json:"cancelled"`
	// Ended: utterances that reached a terminal state — natural end,
	// failure, skip or mute — whether or not they ever started.
	Ended int64 `json:"ended"`
	// CacheHits: resolutions served from the TTS cache (evidence the cache
	// worked, docs/spec/11 "avoiding false positives").
	CacheHits int64 `json:"cache_hits"`
}

// UtteranceState is the session's current audio position: the last speech
// lifecycle step any utterance of this session went through.
type UtteranceState struct {
	// Phase: requested, ready, playing, ended, cancelled, skipped, muted
	// or failed.
	Phase string `json:"phase"`
	// Result is the payload result of the terminal step when the phase is
	// terminal ("ended", "muted", "skipped", "failed").
	Result     string `json:"result,omitempty"`
	Generation int64  `json:"generation"`
	// UtteranceID is the dialogue occurrence the audio belongs to.
	UtteranceID string    `json:"utterance_id,omitempty"`
	At          time.Time `json:"at"`
}

// AudioInfo is the session's audio half in GET /api/v1/sessions/{id}: the
// sink in use, the live controls, the counters and the current utterance.
type AudioInfo struct {
	Sink string `json:"sink"`
	// Muted, Volume and SpeechDelayMS are the live control values.
	Muted         bool           `json:"muted"`
	Volume        float64        `json:"volume"`
	SpeechDelayMS int            `json:"speech_delay_ms"`
	Speech        SpeechCounters `json:"speech"`
	// Current is nil until the first speech event.
	Current *UtteranceState `json:"current_utterance,omitempty"`
}

// speechWiring is one session's voice pipeline (docs/spec/07): a committed
// line resolves the speaker character's approved voice, the line is
// synthesised through the shared TTS cache, and the resulting WAV plays on
// the generation-fenced scheduler onto the configured sink.
//
// The session reducer inside the pipeline owns the text lifecycle; this
// wiring owns the audio lifecycle — speech.requested and speech.ready are
// emitted here, speech.started / ended / cancelled by the scheduler, which
// is the single owner of device-level playback events (ADR-0023). Every
// speech event carries the session id, the committing generation and the
// committing frame's trace id, so the developer UI follows frame → commit →
// audio.
type speechWiring struct {
	id     string // the session id
	log    func(msg string, kv ...any)
	hub    *events.Hub
	voices *voices.Service
	assets assets.Store

	sinkKind string
	sink     *gainSink
	fence    *generationFence
	sched    *speech.Scheduler

	// speakerByMessage maps a committed message row id to its assigned
	// speaker character id (the curator's roster from the published
	// profile); empty means the line is the narrator's.
	speakerByMessage map[string]string

	// sem bounds the concurrent TTS workers (explicit backpressure).
	sem chan struct{}

	// endCtx is the workers' deadline scope: it is cancelled when the run
	// ends so a slow TTS call cannot outlive the session's finalisation.
	endCtx    context.Context
	endCancel context.CancelFunc

	mu sync.Mutex
	// utterances is the occurrence→correlation table (the whole session:
	// one row per committed occurrence, bounded by the clip's line count).
	utterances map[string]*utteranceInfo
	// byGeneration maps a generation to its occurrence for the scheduler's
	// cancel events, which carry no utterance id.
	byGeneration map[int64]string
	// local counts the events this wiring emitted itself: the terminal
	// events (skips, mutes, worker failures) and the cache hits. The
	// scheduler counts its own starts/cancels/ends.
	requested, ready, localEnded, cacheHits int64
	// current is the session's audio position for the API snapshot.
	current *UtteranceState
	// Live controls (SetAudio applies them mid-session).
	muted   bool
	volume  float64
	delayMS int
	// wg tracks the speak workers in flight; stop() drains them (bounded)
	// so every requested utterance reaches its terminal event before the
	// session's terminal events.
	wg sync.WaitGroup
	// closed marks a stopped wiring so a worker that finalises past the
	// drain bound emits nothing rather than after the session is done.
	closed bool
}

// utteranceInfo is one committed line's audio correlation record.
type utteranceInfo struct {
	occurrence string
	messageID  string
	regionID   string
	generation int64
	text       string
	traceID    string
	voiceID    string // the voice profile spoken ("" until resolved)
}

// newSpeechWiring builds the per-session speech pipeline. It returns nil
// (with a warning already logged) when the sink cannot be built — the
// documented degradation: the session runs silent rather than failing the
// run because audio output is misconfigured.
func (m *Manager) newSpeechWiring(id string, speakerByMessage map[string]string, runCtx context.Context) *speechWiring {
	opts := m.opts.Speech
	w := &speechWiring{
		id:               id,
		log:              m.log.Debug,
		hub:              m.opts.Hub,
		voices:           opts.Voices,
		assets:           opts.Assets,
		speakerByMessage: speakerByMessage,
		sem:              make(chan struct{}, speechWorkerSlots),
		utterances:       map[string]*utteranceInfo{},
		byGeneration:     map[int64]string{},
		volume:           1.0,
	}
	endCtx, endCancel := context.WithCancel(runCtx)
	w.endCtx, w.endCancel = endCtx, endCancel

	var (
		sink speech.AudioSink
		kind string
		err  error
	)
	switch {
	case opts.SinkFactory != nil:
		sink, err = opts.SinkFactory()
		// The factory is the test seam; the snapshot still names the sink in
		// the documented vocabulary, so a supplied dummy sink is "dummy",
		// not "factory" — the kind is what plays the audio, not who built it.
		if err == nil {
			switch sink.(type) {
			case *dummy.Sink:
				kind = SinkDummy
			case *filesink.Sink:
				kind = SinkFile
			case *pipewire.Sink:
				kind = SinkPipeWire
			default:
				kind = "external"
			}
		}
	case opts.Sink == SinkDummy:
		kind = SinkDummy
		sink = dummy.New(0, 0)
	case opts.Sink == SinkPipeWire:
		if !pipewirePresent() {
			m.log.Warn("pipewire sink configured but pw-play/paplay not found; recording to the file sink as evidence instead",
				"session_id", id)
			kind, sink, err = m.filesinkFor(id)
		} else {
			kind = SinkPipeWire
			target := opts.PipeWireTarget
			if target == "" {
				target = "default"
			}
			sink, err = pipewire.New(target)
		}
	default:
		kind, sink, err = m.filesinkFor(id)
	}
	if err != nil {
		endCancel()
		m.log.Error("speech sink unavailable; the session runs silent",
			"session_id", id, "sink", opts.Sink, "error", err)
		return nil
	}
	w.sinkKind = kind
	w.sink = newGainSink(sink)
	w.fence = &generationFence{}
	w.sched = speech.NewScheduler(w.sink, w.fence, &speechEventPublisher{hub: m.opts.Hub, w: w}, speech.Options{
		FencePeriod: speech.DefaultFencePeriod,
	})
	w.sched.Start(endCtx)
	return w
}

// filesinkFor builds this session's file sink under <root>/<session id>.
func (m *Manager) filesinkFor(id string) (string, speech.AudioSink, error) {
	dir := m.opts.Speech.FileSinkDir
	if dir == "" {
		return "", nil, errors.New("runtime: filesink requires FileSinkDir")
	}
	sink, err := filesink.New(filepath.Join(dir, id))
	return SinkFile, sink, err
}

// pipewirePresent is the doctor-style preflight for the pipewire sink: the
// player binary the sink spawns must exist on the box (docs/spec/07 "by
// stable device identity" is the target; the binary is the presence check).
func pipewirePresent() bool {
	for _, bin := range []string{"pw-play", "paplay"} {
		if _, err := exec.LookPath(bin); err == nil {
			return true
		}
	}
	return false
}

// stop ends the session's audio: pending synthesis is cancelled, the
// in-flight speak workers reach their session-end terminal events (bounded),
// active playback stops with the scheduler's fade (bounded), and the sink
// releases its resources. It runs before the session's terminal events, so
// the run's final state is the session's final state.
func (w *speechWiring) stop() {
	w.endCancel()
	// Drain the workers first: a line in the TTS call or in the operator's
	// delay wakes on endCancel and emits its terminal event here, while the
	// hub is still open and before session.stopped goes out. The bound keeps
	// a wedged provider from holding the run's finalisation open; one past
	// it degrades to the closed-backstop silence (logged, not flaky).
	drained := make(chan struct{})
	go func() { w.wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(speechStopDrainBound):
		w.log("speech stop: a worker is still finalising past the drain bound; its terminal event may be dropped",
			"session_id", w.id)
	}
	_ = w.sched.Stop()
	_ = w.sink.Close()
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

// fenceAndCancel advances the audio fence to the committing generation and
// cancels every older one still queued or playing. A newer committed
// generation (or the textbox advance that produced it) is exactly when
// stale audio must stop (docs/spec/01 invariant 2; docs/spec/12 "zero
// stale audio starts").
func (w *speechWiring) fenceAndCancel(generation int64) {
	w.mu.Lock()
	stale := make([]int64, 0, 4)
	for g := range w.byGeneration {
		if g < generation {
			stale = append(stale, g)
		}
	}
	w.fence.set(generation)
	w.mu.Unlock()
	for _, g := range stale {
		w.sched.Cancel(g, "new_commit")
	}
}

// onCommit is the commit watcher's handoff: it runs on the pipeline's
// reducer goroutine and must stay fast — the fence, the cancel and the event
// are the synchronous half; the TTS work is the worker's.
func (w *speechWiring) onCommit(e events.Envelope) {
	var d struct {
		Occurrence string `json:"dialogue_occurrence_id"`
		MessageID  string `json:"message_id"`
		RegionID   string `json:"region_id"`
		Generation int64  `json:"generation"`
		Text       string `json:"text"`
		Eligible   bool   `json:"speech_eligible"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil || d.Occurrence == "" || d.Generation <= 0 {
		return
	}

	// The fence advances on EVERY commit — an ineligible box (UI text, an
	// abstained line) still means the player moved on, and the previous
	// line's audio stops with it.
	w.fenceAndCancel(d.Generation)

	u := &utteranceInfo{
		occurrence: d.Occurrence,
		messageID:  d.MessageID,
		regionID:   d.RegionID,
		generation: d.Generation,
		text:       strings.TrimSpace(d.Text),
		traceID:    e.TraceID,
	}
	w.register(u)

	// The commit payload is the eligibility answer: an abstained box
	// (message_id "unknown") or a decision that refused speech is not a
	// line the narrator speaks, and it gets no speech events at all.
	if !d.Eligible || d.MessageID == "" || d.MessageID == "unknown" || u.text == "" {
		return
	}
	select {
	case w.sem <- struct{}{}:
	default:
		// Explicit backpressure: the slots are all in slow TTS calls, and
		// the session event loop is not going to wait for audio.
		w.finishEnded(u, "skipped", "speech_backpressure")
		return
	}
	w.mu.Lock()
	w.requested++
	w.current = &UtteranceState{Phase: "requested", Generation: u.generation, UtteranceID: u.occurrence, At: time.Now().UTC()}
	w.mu.Unlock()
	w.emitSpeech(events.SpeechRequested, u, map[string]any{
		"dialogue_occurrence_id": u.occurrence,
		"message_id":             u.messageID,
		"generation":             u.generation,
		"region_id":              u.regionID,
		"text":                   u.text,
		"trace_id":               u.traceID,
	})
	w.wg.Add(1)
	go w.speak(u)
}

// register keeps the correlation tables bounded.
func (w *speechWiring) register(u *utteranceInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.utterances[u.occurrence]; !ok {
		w.utterances[u.occurrence] = u
	}
	w.byGeneration[u.generation] = u.occurrence
	for len(w.byGeneration) > byGenerationCap {
		var oldest int64 = -1
		for g := range w.byGeneration {
			if oldest == -1 || g < oldest {
				oldest = g
			}
		}
		if oldest == -1 {
			break
		}
		delete(w.byGeneration, oldest)
	}
}

// speak is the TTS worker: voice → cache key → synthesise (cached) →
// delay (fenced) → mute check → the scheduler's fence-checked start.
// Every requested utterance reaches a terminal event on this path —
// including when the session ends mid-flight: the run's terminal events
// end the session, not this line (docs/spec/13 "did it speak, and why not").
func (w *speechWiring) speak(u *utteranceInfo) {
	defer func() { <-w.sem }()
	defer w.wg.Done()
	ctx := w.endCtx
	if ctx.Err() != nil {
		return
	}

	rv, why := w.resolveVoice(ctx, u.messageID)
	if rv.profile.ID == "" {
		// Silence with the reason: no approved voice for the speaker and no
		// configured narrator voice — or the approved reference is missing.
		// Never a substitute (docs/spec/07).
		w.finishEnded(u, "skipped", why)
		return
	}
	w.mu.Lock()
	u.voiceID = rv.profile.ID
	w.mu.Unlock()

	key, err := speech.CacheKeyOf(speech.KeyInput{
		UtteranceText:            u.text,
		OriginalMessageID:        u.messageID,
		SpeechRulesRevision:      1,
		Language:                 rv.profile.Language,
		VoiceRevisionFingerprint: rv.profile.Fingerprint,
		TTSModelID:               rv.profile.ModelID,
		BackendConfigDigest:      w.voices.BackendDigest,
		Format:                   "wav/pcm16",
	})
	if err != nil {
		w.finishEnded(u, "failed", fmt.Sprintf("cache_key:%v", err))
		return
	}

	pctx, cancel := context.WithTimeout(ctx, speechTTSTimeout)
	res, err := w.voices.Speech.GetOrGenerate(pctx, key, speech.PriorityCurrent, func(ctx context.Context) (tts.Audio, error) {
		return rv.generate(ctx, u.text)
	})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			// The session ended while this line was still synthesising: it
			// was requested, so it ends with the reason, not in silence.
			w.finishEnded(u, "failed", "session_ended")
			return
		}
		w.finishEnded(u, "failed", providerErrorKind(err))
		return
	}
	if res.FromCache {
		w.mu.Lock()
		w.cacheHits++
		w.mu.Unlock()
	}

	w.mu.Lock()
	w.ready++
	w.current = &UtteranceState{Phase: "ready", Generation: u.generation, UtteranceID: u.occurrence, At: time.Now().UTC()}
	w.mu.Unlock()
	w.emitSpeech(events.SpeechReady, u, map[string]any{
		"dialogue_occurrence_id": u.occurrence,
		"message_id":             u.messageID,
		"generation":             u.generation,
		"region_id":              u.regionID,
		"cache_key":              string(key),
		"voice_id":               rv.profile.ID,
		"voice_revision":         rv.profile.Revision,
		"from_cache":             res.FromCache,
		"trace_id":               u.traceID,
	})

	// The operator's delay: a fence-checked wait before start (docs/spec/07
	// "optional delay calibration"). A commit during the wait moves the
	// fence, and the scheduler's before-start check drops the utterance;
	// a session end during the wait ends the utterance with its reason.
	if d := w.delay(); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			w.finishEnded(u, "failed", "session_ended")
			return
		}
	}
	if w.isMuted() {
		// Mute: the audio was resolved (the cache is filled), the playback
		// is skipped, and the event carries the reason (requirement: mute
		// still emits, with result "muted").
		w.finishEnded(u, "muted", "muted")
		return
	}
	w.sched.Submit(speech.UtteranceReady{
		SessionID:        w.id,
		Generation:       u.generation,
		UtteranceID:      u.occurrence,
		Priority:         speech.PriorityCurrent,
		WAV:              res.WAV,
		ExpectedDuration: res.Duration,
	})
}

// resolveVoice finds the speaker character's approved voice and its TTS
// identity: the registered name when the voice is ready on this instance,
// else the approved reference carried inline (docs/spec/06 fallback).
//
// The speaker is the line's assigned character; a line with no assignment
// is the narrator's. A character without an approved voice falls back to
// the narrator voice when one is configured — never to a different
// character's voice, never to a builtin. A specific fault on the primary
// voice (its approved reference is missing) is reported, not papered over
// by the fallback: substituting a voice for a broken voice would hide the
// fault.
func (w *speechWiring) resolveVoice(ctx context.Context, messageID string) (resolvedVoice, string) {
	character := w.speakerByMessage[messageID]
	if character == "" {
		character = narratorCharacter
	}
	if rv, why := w.approvedVoice(ctx, character); rv.profile.ID != "" || why != "" {
		return rv, why
	}
	if character != narratorCharacter {
		return w.approvedVoice(ctx, narratorCharacter)
	}
	return resolvedVoice{}, "no_approved_voice"
}

// approvedVoice resolves one character's current approved voice: the
// highest approved revision carrying a fingerprint and a reference
// (internal/voices' approval invariants). The second return is "" only
// when the character has no approved voice at all; any non-empty value is
// a specific fault (the approved reference is unreadable, the name could
// not be derived).
func (w *speechWiring) approvedVoice(ctx context.Context, character string) (resolvedVoice, string) {
	profiles, err := w.voices.Store.ListVoiceProfiles(ctx, character)
	if err != nil {
		// The store is the appliance's own database: a read failure is a
		// fault to surface, not a silent "no voice".
		return resolvedVoice{}, fmt.Sprintf("voice_store:%v", err)
	}
	for _, p := range profiles { // ordered by revision, descending
		if p.State != store.VoiceStateApproved || p.Fingerprint == "" || p.ReferenceAssetID == "" {
			continue
		}
		reg, regErr := w.voices.Store.GetVoiceRegistration(ctx, p.ID, w.voices.InstanceID)
		if regErr == nil && reg.State == store.VoiceRegReady && reg.RegisteredName != "" {
			// Ready on this instance: the Base checkpoint speaks the
			// registered name.
			return resolvedVoice{
				profile: p,
				name:    reg.RegisteredName,
				ready:   true,
				client:  w.voices.Base,
			}, ""
		}
		// Not ready (or no row): the call carries the approved reference
		// inline, speaking the revision's registry name.
		ref, err := w.loadReference(ctx, p.ReferenceAssetID)
		if err != nil {
			return resolvedVoice{}, "voice_reference_missing"
		}
		name := ""
		if regErr == nil {
			name = reg.RegisteredName
		}
		if name == "" {
			name, err = voices.RegistryName(p.ID, p.Character, p.Revision, ref)
			if err != nil {
				return resolvedVoice{}, "voice_name_derivation_failed"
			}
		}
		return resolvedVoice{
			profile: p,
			name:    name,
			ready:   false,
			refWAV:  ref,
			refText: p.RefTranscript,
			client:  w.voices.Base,
		}, ""
	}
	return resolvedVoice{}, ""
}

// loadReference reads one approved reference clip from the asset store.
func (w *speechWiring) loadReference(ctx context.Context, assetID string) ([]byte, error) {
	r, _, err := w.assets.Get(ctx, assetID)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	return out, err
}

// resolvedVoice is one speaker's resolved TTS identity.
type resolvedVoice struct {
	profile store.VoiceProfile
	// name is the voice the Base checkpoint speaks: the registered name
	// when ready, else the derived registry name of the reference.
	name    string
	ready   bool
	refWAV  []byte // the approved reference, when not ready
	refText string
	// client is the Base checkpoint's synthesis seam (the real TTS client
	// in production, a scripted fake in tests).
	client voices.BaseClient
}

// generate is the cache's GenerationFunc: the provider call with its
// negotiated identity (registered name, or inline reference).
func (rv *resolvedVoice) generate(ctx context.Context, text string) (tts.Audio, error) {
	req := tts.GenerateRequest{Text: text, Voice: rv.name, Language: rv.profile.Language}
	if rv.ready {
		return rv.client.Generate(ctx, req)
	}
	return rv.client.GenerateWithReference(ctx, req, rv.refWAV, rv.refText)
}

// finishEnded emits one terminal speech.ended the wiring owns (skip, mute,
// worker failure) and records the session's audio position.
func (w *speechWiring) finishEnded(u *utteranceInfo, result, reason string) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.localEnded++
	w.current = &UtteranceState{Phase: result, Result: result, Generation: u.generation, UtteranceID: u.occurrence, At: time.Now().UTC()}
	w.mu.Unlock()
	data := map[string]any{
		"dialogue_occurrence_id": u.occurrence,
		"message_id":             u.messageID,
		"generation":             u.generation,
		"region_id":              u.regionID,
		"result":                 result,
		"trace_id":               u.traceID,
	}
	if reason != "" {
		data["reason"] = reason
	}
	w.emitSpeech(events.SpeechEnded, u, data)
}

// emitSpeech publishes one speech event this wiring owns, on the session's
// scope with the committing frame's trace id.
func (w *speechWiring) emitSpeech(name string, u *utteranceInfo, data map[string]any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	ctx := w.endCtx
	if w.endCtx.Err() != nil {
		ctx = context.Background() // a worker finalising on the way out still reports
	}
	_, err = w.hub.Publish(ctx, events.Envelope{
		SchemaVersion: 1,
		SessionID:     w.id,
		TraceID:       u.traceID,
		Name:          name,
		Data:          b,
	})
	if err != nil {
		w.log("speech event publish failed", "session_id", w.id, "event", name, "error", err)
	}
}

// audioInfo is the session's audio half for the API snapshot.
func (w *speechWiring) audioInfo() *AudioInfo {
	w.mu.Lock()
	info := &AudioInfo{
		Sink:          w.sinkKind,
		Muted:         w.muted,
		Volume:        w.volume,
		SpeechDelayMS: w.delayMS,
		Speech: SpeechCounters{
			Requested: w.requested,
			Ready:     w.ready,
			Ended:     w.localEnded,
			CacheHits: w.cacheHits,
		},
		Current: w.current,
	}
	w.mu.Unlock()
	st := w.sched.Stats()
	info.Speech.Started = st.Started
	info.Speech.Cancelled = st.Cancelled
	info.Speech.Ended += st.Ended + st.Failed
	return info
}

// setAudio applies one live control change (Manager.SetAudio validates).
func (w *speechWiring) setAudio(s AudioSettings) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s.Muted != nil {
		w.muted = *s.Muted
	}
	if s.Volume != nil {
		w.volume = *s.Volume
		w.sink.setGain(*s.Volume)
	}
	if s.SpeechDelayMS != nil {
		w.delayMS = *s.SpeechDelayMS
	}
}

func (w *speechWiring) isMuted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.muted
}

func (w *speechWiring) delay() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Duration(w.delayMS) * time.Millisecond
}

// providerErrorKind reduces one provider error to its class for the event
// payload: the class travels, the message (which may quote the endpoint)
// stays in the log (AGENTS.md rule 5).
func providerErrorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, assets.ErrNotFound):
		return "reference_missing"
	default:
		return "provider_error"
	}
}

// ── fence ------------------------------------------------------------------

// generationFence is the runtime's Fence (docs/spec/07 "only event reducer
// can approve playback token"): it approves exactly the generation the
// session has currently committed. Equality, not "≤" — a clip synthesised
// for generation 11 that arrives after generation 12 is stale and never
// plays (docs/spec/01 invariant 2).
type generationFence struct {
	mu     sync.Mutex
	latest int64
}

func (f *generationFence) set(g int64) {
	f.mu.Lock()
	f.latest = g
	f.mu.Unlock()
}

// IsCurrent implements speech.Fence.
func (f *generationFence) IsCurrent(g int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest == g
}

// ── publishers -------------------------------------------------------------

// commitWatcher sits between the pipeline's session reducer and the rest of
// the publisher chain: it forwards every event unchanged, and on a commit it
// hands the speech wiring the commit's correlation data. The handoff is
// synchronous and fast (fence + cancel + one event); the TTS work runs on
// the wiring's own workers, so the audio pipeline can never block the
// session event loop (docs/spec/07).
type commitWatcher struct {
	w     *speechWiring
	inner ses.Publisher
}

// Publish implements ses.Publisher.
func (p *commitWatcher) Publish(ctx context.Context, e events.Envelope) (events.Envelope, error) {
	if e.Name == events.MessageCommitted {
		p.w.onCommit(e)
	}
	return p.inner.Publish(ctx, e)
}

// speechEventPublisher decorates the hub for the scheduler's events
// (speech.started / ended / cancelled): it fills the session id the
// scheduler cannot know for queued drops, and joins the committing frame's
// trace and correlation ids onto every speech event so the developer UI can
// follow frame → commit → audio (docs/spec/09 "Link everything using
// trace_id").
type speechEventPublisher struct {
	hub *events.Hub
	w   *speechWiring
}

// Publish implements speech.Publisher.
func (p *speechEventPublisher) Publish(ctx context.Context, e events.Envelope) (events.Envelope, error) {
	switch e.Name {
	case events.SpeechStarted, events.SpeechEnded, events.SpeechCancelled:
		if e.SessionID == "" {
			e.SessionID = p.w.id
		}
		e = p.w.correlateSpeech(e)
		p.w.noteSchedulerEvent(e)
	default:
	}
	return p.hub.Publish(ctx, e)
}

// correlateSpeech joins the correlation ids onto one scheduler event: the
// envelope's trace_id from the committing frame, and the message /
// occurrence / region / voice ids the scheduler's minimal payload omits.
// A payload it cannot read is passed through untouched: an event must
// never fail to publish because its correlation is unavailable.
func (w *speechWiring) correlateSpeech(e events.Envelope) events.Envelope {
	var data map[string]any
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return e
	}
	occ, _ := data["utterance_id"].(string)
	var gen int64
	switch v := data["generation"].(type) {
	case float64:
		gen = int64(v)
	case int64:
		gen = v
	}
	var u *utteranceInfo
	if occ != "" {
		w.mu.Lock()
		u = w.utterances[occ]
		w.mu.Unlock()
	} else {
		w.mu.Lock()
		if id, ok := w.byGeneration[gen]; ok {
			u = w.utterances[id]
		}
		w.mu.Unlock()
	}
	if u == nil {
		return e
	}
	e.TraceID = u.traceID
	data["dialogue_occurrence_id"] = u.occurrence
	data["message_id"] = u.messageID
	data["region_id"] = u.regionID
	data["trace_id"] = u.traceID
	if u.voiceID != "" {
		data["voice_id"] = u.voiceID
	}
	if b, err := json.Marshal(data); err == nil {
		e.Data = b
	}
	return e
}

// noteSchedulerEvent records the session's audio position from one
// scheduler event (the wiring's own events update it inline).
func (w *speechWiring) noteSchedulerEvent(e events.Envelope) {
	var data map[string]any
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return
	}
	phase, result := "", ""
	switch e.Name {
	case events.SpeechStarted:
		phase = "playing"
	case events.SpeechCancelled:
		phase, result = "cancelled", "cancelled"
	case events.SpeechEnded:
		phase = "ended"
		result, _ = data["result"].(string)
	default:
		return
	}
	occ, _ := data["utterance_id"].(string)
	gen, _ := data["generation"].(float64)
	w.mu.Lock()
	w.current = &UtteranceState{
		Phase:       phase,
		Result:      result,
		Generation:  int64(gen),
		UtteranceID: occ,
		At:          time.Now().UTC(),
	}
	w.mu.Unlock()
}

// ── volume sink --------------------------------------------------------------

// gainSink is the volume wrapper of the sink path: it scales the utterance
// samples by the live gain before they reach the device sink. The scheduler
// hands every sink a unity Gain (its requestOf), so the scaling happens
// exactly once, here; the wrapped request always carries 1.0 so a sink that
// honours Gain on top cannot double-apply it.
type gainSink struct {
	inner speech.AudioSink
	// gain is the live volume as float64 bits in a Uint64: the stdlib
	// atomic of this toolchain has no Float64 type.
	gain atomic.Uint64
}

func newGainSink(inner speech.AudioSink) *gainSink {
	g := &gainSink{inner: inner}
	g.setGain(1.0)
	return g
}

func (s *gainSink) setGain(v float64) { s.gain.Store(math.Float64bits(v)) }

func (s *gainSink) loadGain() float64 { return math.Float64frombits(s.gain.Load()) }

// Play implements speech.AudioSink.
func (s *gainSink) Play(ctx context.Context, req speech.PlayRequest) (speech.PlayHandle, error) {
	g := s.loadGain()
	if g < 0 {
		g = 0
	}
	if g > 1 {
		g = 1
	}
	if g != 0 && g != 1 {
		pcm, err := speech.DecodeWAV(req.WAV)
		if err != nil {
			return nil, fmt.Errorf("runtime: scaling volume: %w", err)
		}
		req.WAV = speech.EncodeWAV(scalePCM(pcm.Data, g, pcm.BitsPerSample),
			pcm.SampleRate, pcm.Channels, pcm.BitsPerSample)
	}
	req.Gain = 1.0
	return s.inner.Play(ctx, req)
}

// Close implements speech.AudioSink.
func (s *gainSink) Close() error { return s.inner.Close() }

// scalePCM multiplies interleaved little-endian PCM by g, clamping to the
// sample range. Only 16- and 32-bit samples exist (the contract
// tts.ValidateAudio enforces on every utterance).
func scalePCM(data []byte, g float64, bits int) []byte {
	out := make([]byte, len(data))
	switch bits {
	case 16:
		for i := 0; i+1 < len(data); i += 2 {
			v := int32(int16(binary.LittleEndian.Uint16(data[i:])))
			v = int32(math.Round(float64(v) * g))
			if v < math.MinInt16 {
				v = math.MinInt16
			}
			if v > math.MaxInt16 {
				v = math.MaxInt16
			}
			binary.LittleEndian.PutUint16(out[i:], uint16(int16(v)))
		}
	case 32:
		for i := 0; i+3 < len(data); i += 4 {
			v := int32(binary.LittleEndian.Uint32(data[i:]))
			f := math.Round(float64(v) * g)
			if f < math.MinInt32 {
				f = math.MinInt32
			}
			if f > math.MaxInt32 {
				f = math.MaxInt32
			}
			binary.LittleEndian.PutUint32(out[i:], uint32(int32(f)))
		}
	}
	return out
}
