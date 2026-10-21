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
	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	ses "github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/alsahw"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/speech/filesink"
	"github.com/dbirks/game-narrator/internal/speech/pipewire"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/telemetry"
	"github.com/dbirks/game-narrator/internal/voices"
)

// Audio sink kinds for a session's speech pipeline (docs/spec/07 "Audio
// scheduler": "audio sink abstraction also includes dummy/file/capture for
// CI and tests"). The values are the audio package's constants (ADR-0074
// keeps one spelling: the selection logic and the runtime validate the same
// set).
const (
	// SinkDummy plays nothing but paces and records: the test default.
	SinkDummy = audio.SinkDummy
	// SinkFile records, in real time, exactly the samples that would have
	// played, one WAV per utterance (docs/spec/11 evidence).
	SinkFile = audio.SinkFile
	// SinkPipeWire streams to the PipeWire/PulseAudio device of the
	// configured stable identity (docs/spec/07: "Use USB ALSA/PipeWire sink
	// by stable device identity").
	SinkPipeWire = audio.SinkPipeWire
	// SinkALSA writes the device node in-process at the profile's exact
	// proven hw_params (issue #5 / ADR-0111): the UNO Q narrator is a
	// system service without a user PipeWire session, and the hub HDMI
	// bridge (ANX7625) audibly plays only at the config the profile pins.
	SinkALSA = audio.SinkALSA
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

// Successor-prewarming defaults (FR-11, docs/spec/07 "Prewarm top weighted
// successor messages but cap by scenes/byte quota, graph entropy and
// resource budget").
const (
	// DefaultPrewarmTopK is how many of the committed line's weighted
	// successors are prewarmed per commit.
	DefaultPrewarmTopK = 3
	// DefaultPrewarmMaxEntropyBits is the Shannon entropy (bits) of the
	// successor weight distribution above which the batch is skipped: with
	// two near-equal successors the entropy is ~1.0 bit, so the default
	// prewarms a confident next line and a two-way fork, but not a flat
	// three-way choice (1.58 bits).
	DefaultPrewarmMaxEntropyBits = 1.5
	// DefaultPrewarmByteQuota bounds the bytes of prewarmed audio one
	// session may commit. 4 MiB is ~2 minutes of 16 kHz/16-bit mono speech:
	// the prewarm stays ahead of the player for the rest of the scene
	// without filling the box's storage budget.
	DefaultPrewarmByteQuota = 4 << 20
	// DefaultPrewarmJobDeadline bounds one prewarm's synthesis: a prewarm
	// that cannot finish while the player likely still has the committed
	// line on screen has lost the race, and the commit pays the round trip.
	DefaultPrewarmJobDeadline = 10 * time.Second
)

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
	// MasterVolume is the device-level master volume, 0..1 (1.0 = unity,
	// the default): the appliance's one output level, shared by every
	// session and set from the Settings page (ADR-0074). It multiplies the
	// session's own live volume at the sink, and the API updates it live
	// through Manager.SetMasterVolume.
	MasterVolume float64
	// SinkFactory is the test seam: when non-nil it supplies the sink
	// instead of Sink (tests need the programmable dummy sink's records).
	// Production wiring (cmd/game-narrator) leaves it nil.
	SinkFactory func() (speech.AudioSink, error)

	// Prewarm tunes the session's successor prewarming (FR-11, docs/spec/07
	// "Prewarm top weighted successor messages but cap by scenes/byte
	// quota, graph entropy and resource budget"); nil takes the documented
	// defaults. The feature is inert for a session whose dialogue graph has
	// no usable outgoing edge from a committed line — no graph evidence, no
	// prewarming (the "cap by scenes" in practice).
	Prewarm *PrewarmOptions
	// CatchUp tunes the session's catch-up narration and no-backlog speech
	// policy (issue #25, ADR-0122): the catch_up/latest_only policy, the
	// batch window and the per-utterance max age. nil takes the documented
	// defaults (catch_up, 600 ms, 3 s).
	CatchUp *CatchUpOptions
}

// CatchUpOptions is the session's catch-up narration tuning (issue #25,
// ADR-0122); it is the speech package's own options shape so the runtime
// and the scenario harness configure the policy from one vocabulary.
type CatchUpOptions = speech.CatchUpOptions

// PrewarmOptions tunes one session's successor prewarming. Zero fields take
// the documented defaults (DefaultPrewarm* above).
type PrewarmOptions struct {
	// TopK is how many of the committed line's weighted successors are
	// prewarmed per commit (0 selects DefaultPrewarmTopK).
	TopK int
	// MaxEntropyBits is the Shannon entropy (bits) of the successor weight
	// distribution above which the batch is skipped: flat successors are too
	// uncertain to spend the TTS budget on (0 selects
	// DefaultPrewarmMaxEntropyBits).
	MaxEntropyBits float64
	// ByteQuota bounds the bytes of prewarmed audio the session may commit
	// (0 selects DefaultPrewarmByteQuota).
	ByteQuota int64
	// JobDeadline bounds one prewarm's synthesis (0 selects
	// DefaultPrewarmJobDeadline).
	JobDeadline time.Duration
}

// prewarmConfig is the session's resolved prewarming policy.
type prewarmConfig struct {
	topK           int
	maxEntropyBits float64
	byteQuota      int64
	jobDeadline    time.Duration
}

func defaultPrewarmConfig(opts *PrewarmOptions) prewarmConfig {
	c := prewarmConfig{
		topK:           DefaultPrewarmTopK,
		maxEntropyBits: DefaultPrewarmMaxEntropyBits,
		byteQuota:      DefaultPrewarmByteQuota,
		jobDeadline:    DefaultPrewarmJobDeadline,
	}
	if opts == nil {
		return c
	}
	if opts.TopK > 0 {
		c.topK = opts.TopK
	}
	if opts.MaxEntropyBits > 0 {
		c.maxEntropyBits = opts.MaxEntropyBits
	}
	if opts.ByteQuota > 0 {
		c.byteQuota = opts.ByteQuota
	}
	if opts.JobDeadline > 0 {
		c.jobDeadline = opts.JobDeadline
	}
	return c
}

// PrewarmStats are one session's successor-prewarm decision counters
// (docs/spec/07, docs/spec/13 "counters a report can act on"): how much
// prewarming the session requested, how much a later commit actually
// consumed from the cache, how much finished (or was cut short) without
// ever being consumed, and the bytes the session spent on it.
type PrewarmStats struct {
	// Requested: prewarm jobs the session enqueued.
	Requested int64 `json:"requested"`
	// Hit: prewarmed keys a later committed line resolved from the cache.
	Hit int64 `json:"hit"`
	// Wasted: prewarmed keys that reached no hit before the session ended
	// (counted at the end), plus jobs cut short by the fence, a scene
	// change or the session's own end.
	Wasted int64 `json:"wasted"`
	// Bytes: the committed bytes of every prewarmed utterance.
	Bytes int64 `json:"bytes"`
	// Skipped: batches the entropy or byte-quota gate refused to prewarm.
	Skipped int64 `json:"skipped"`
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

// CatchUpStats are the session's catch-up policy decision counters
// (issue #25, ADR-0122): what the policy batched, merged and fenced — the
// "did it speak, and why not" reading for the catch-up decisions, in the
// same surface as the speech counters.
type CatchUpStats struct {
	// Batched: lines a new commit took into the batch (the rescue) or the
	// line itself joined the batch on a busy commit.
	Batched int64 `json:"batched"`
	// Dropped: lines the batch fenced as stale (the window's fence, the
	// max age, the pause and stop) — the no-backlog policy's teeth
	// (issue #25 item 1).
	Dropped int64 `json:"dropped"`
	// MergedRuns: merged utterances the batch spoke (one TTS request each,
	// the merge budget the policy is for, ADR-0122 decision 3).
	MergedRuns int64 `json:"merged_runs"`
}

// CatchUpInfo is the session's catch-up narration surface in the API
// snapshot: the policy word the session runs (the live settings value) and
// the policy's decision counters.
type CatchUpInfo struct {
	Policy string `json:"policy"`
	CatchUpStats
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
	Muted         bool    `json:"muted"`
	Volume        float64 `json:"volume"`
	SpeechDelayMS int     `json:"speech_delay_ms"`
	// MasterVolume is the appliance's device-level master volume (ADR-0074),
	// shared by every session of this process.
	MasterVolume float64        `json:"master_volume"`
	Speech       SpeechCounters `json:"speech"`
	// Underruns counts this run's started playbacks that ended in a sink
	// failure — the docs/spec/13 "playback underruns" counter for this
	// session.
	Underruns int64 `json:"underruns"`
	// CatchUp is the session's catch-up narration policy and its decision
	// counters (issue #25, ADR-0122): the policy word the session runs and
	// what the policy batched, merged and fenced.
	CatchUp CatchUpInfo `json:"catch_up"`
	// Prewarm is this session's successor-prewarm decision counters (FR-11).
	Prewarm PrewarmStats `json:"prewarm"`
	// OutputLatency is this run's output-latency table (sink accepted the
	// playback → the device buffer accepted the first samples); count 0 when
	// the sink does not report first-sample timestamps.
	OutputLatency pipeline.LatencySummary `json:"output_latency"`
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
	// runner is the session's pipeline run: the audio counters the wiring
	// observes (underruns, output latency) land in the run's own stats, so
	// the terminal event's payload and the bottleneck summary carry them
	// (ADR-0074, docs/spec/13).
	runner *pipeline.Runner

	// metrics is the process's telemetry surface (docs/spec/13 "Metrics",
	// internal/telemetry): the TTS latency and cache hit/miss, the playback
	// underrun and the prewarm hit book here, labelled by mLabels — the same
	// game/revision pair the pipeline's stages use, so a run reads as one
	// story across the surface. Taken from the runner (which owns the pair);
	// nil (the runner built no catalog, or the test wiring has no runner) is
	// the off shape, and every hook it feeds is nil-safe.
	metrics *telemetry.Product
	mLabels telemetry.Labels

	// speakerByMessage maps a committed message row id to its assigned
	// speaker character id (the curator's roster from the published
	// profile); empty means the line is the narrator's.
	speakerByMessage map[string]string

	// narratorFallback is the profile's narrator fallback (ADR-0101): a line
	// whose speaker has no approved voice is spoken by the narrator voice
	// when one is configured and only when this flag is on. The profile row
	// defaults it on, the runtime's historical behaviour, and a session reads
	// it once at start. A skip's reason says when the setting — not just the
	// absence of voices — is what kept the line silent.
	narratorFallback bool

	// sem bounds the concurrent TTS workers (explicit backpressure).
	sem chan struct{}

	// Successor prewarming (FR-11, docs/spec/07): after each speakable
	// commit the committed line's top weighted successors are synthesised,
	// at background priority, into the SAME versioned cache key a later
	// commit would use, so the later commit resolves from the cache
	// (from_cache:true) instead of paying a TTS round trip. The batch is
	// capped by the byte quota and the graph's entropy, runs at
	// PriorityBackground (committed speech admits ahead of it), and is
	// cancelled when the fence moves or the scene changes.
	idx          *matcher.Index    // nil = no dialogue graph, no prewarming
	prewarmTexts map[string]string // message id → canonical TTS text
	prewarmCfg   prewarmConfig
	// prewarmCancel cancels the in-flight batch; the next batch replaces it
	// (and cancels it) on a new speakable commit or a scene change. w.mu.
	prewarmCancel context.CancelFunc
	// prewarmKeys tracks the prewarmed keys not yet consumed by a commit:
	// a hit removes its key, and the remainder at session end is the
	// wasted count. w.mu.
	prewarmKeys map[string]struct{}
	// The prewarm counters of docs/spec/13 (FR-11): w.mu.
	prewarmRequested, prewarmHit, prewarmWasted, prewarmBytes, prewarmSkipped int64

	// endCtx is the workers' deadline scope: it is cancelled when the run
	// ends so a slow TTS call cannot outlive the session's finalisation.
	endCtx    context.Context
	endCancel context.CancelFunc

	// voiceLat is the session's commit→voice sampler (status.go). It owns
	// its own lock: the commit path books on the pipeline's writer and the
	// scheduler's publisher completes from the sink's goroutine, so it must
	// not ride w.mu (the scheduler events above take w.mu, and a path that
	// took both in the opposite order from another would deadlock).
	voiceLat *frameVoiceSampler

	mu sync.Mutex
	// utterances is the occurrence→correlation table (the whole session:
	// one row per committed occurrence, bounded by the clip's line count).
	utterances map[string]*utteranceInfo
	// byGeneration maps a generation to its occurrence for the scheduler's
	// cancel events, which carry no utterance id.
	byGeneration map[int64]string
	// genCancel holds the cancellable context of a synthesis still in flight
	// for a generation: a fence that moves while a line is being
	// synthesised cancels the read at the source (ADR-0057) and the client
	// discards whatever partial the stream had delivered.
	genCancel map[int64]*synthSlot
	// settled marks the generations that reached a terminal — ended, cancelled,
	// skipped, muted or failed — so the fence never cancels them (and re-emits a
	// terminal for them) a second time. Terminal results are idempotent
	// (issue #6; ADR-0092): "settled" and "a terminal was emitted for this
	// generation" are the same fact, and the fence's cancel list is built only
	// from generations that have not settled yet.
	settled map[int64]bool
	// masterLoad reads the manager's device-level master volume (ADR-0074);
	// the wrapper sink and the snapshot both read through it, so a
	// Settings-page change is visible without rebuilding the wiring.
	masterLoad func() float64
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

	// Catch-up narration and the no-backlog policy (issue #25, ADR-0122).
	// w.mu.
	// catchUp is the session's resolved policy (the defaults when the
	// options were nil).
	catchUp speech.CatchUpOptions
	// catchUpPending is the batch: the committed lines waiting to be merged,
	// in commit order. A line lives here between its commit and its flush;
	// the window-drop fences its old prefix at each newer line. The batcher
	// core's own line type, so the merge decision reads the same data the
	// runtime's bookkeeping writes.
	catchUpPending []speech.CatchUpLine
	// catchUpTimer is the flush timer's cancel (nil while no batch is
	// pending): the window elapsing since the batch's first line flushes,
	// the playback terminal flushes, and the box-change hook flushes.
	catchUpTimer func() bool
	// playingGen is the generation the session's active playback plays
	// (0 = nothing playing): the scheduler's started/ended events move it
	// (noteSchedulerEvent). Under catch_up the commit path leaves the
	// playing line to run out — that is the behaviour the issue asked for.
	playingGen int64
	// catchUpRuns is the in-flight merged runs of the catch-up batches,
	// by their bookkeeping generation (the batch's newest commit generation
	// plus the run's index — the fence approves each, the rescue finds each,
	// and the settle bookkeeping is per run). A run leaves the table when
	// it settles (its terminal event) or when a rescue takes it over.
	catchUpRuns map[int64]*utteranceInfo
	// lineClaim is the owner of a pending or covered line's terminal:
	// the run generation that speaks the line (the batch's flush claimed it
	// when it built the run), 0 while the line waits in the batch with no
	// run yet, and absent once the line's terminal is out. The one-terminal
	// rule for a line the batch owns (issue #25 + ADR-0092) is this map: a
	// run's terminal settles the line only under the claim it built, so a
	// late run that met its line re-batched by a newer commit settles
	// nothing, and the window's fence and the pause settle the line by
	// deleting the claim with the terminal.
	lineClaim map[string]int64
	// The policy's decision counters (docs/spec/13 "did it speak, and why
	// not"): w.mu.
	catchUpBatched, catchUpMerged, catchUpDropped int64
}

// utteranceInfo is one committed line's audio correlation record.
type utteranceInfo struct {
	occurrence string
	messageID  string
	regionID   string
	generation int64
	text       string
	traceID    string
	// character is the speaker the line resolves to: the assigned character,
	// or the narrator when unattributed. It rides on every speech event so a
	// skip (no approved voice) names whose voice was missing (issue #6).
	character string
	voiceID   string // the voice profile spoken ("" until resolved)
	// committedAt is the wall clock the line reached the speech side
	// (issue #25, ADR-0122): the catch-up window and the max age measure
	// from it.
	committedAt time.Time
	// rescued marks a line the catch-up batch took over from its standalone
	// utterance (ADR-0122 decision 2): the worker that synthesises it skips
	// its own terminal — the batch's terminal for the line (skipped,
	// catch_up_batched, or the merged run's outcome) is the one that counts.
	rescued bool
	// synthSlot is the utterance's own in-flight synthesis cancel: the
	// merged runs (several under one batch generation) keep it here rather
	// than in the per-generation genCancel table, and the rescue cancels it
	// at the source when a new commit batches the run over.
	synthSlot *synthSlot
	// submitted marks the utterance handed to the scheduler (the worker
	// sets it under w.mu at the submit): the rescue distinguishes a queued
	// item (the scheduler's cancel is its terminal) from an in-flight
	// synthesis (the rescue's own terminal).
	submitted bool
	// mergedLines is, for a merged run's utterance, the covered lines in
	// commit order (ADR-0122 decision 3): the run's terminal settles them,
	// and a rescue of the run re-batches them individually.
	mergedLines []speech.CatchUpLine
}

// synthSlot is one in-flight synthesis's cancellable scope, registered
// against the utterance's generation: the fence cancels it on a new commit
// and the worker deregisters it when the synthesis settles. A pointer, so
// the fence and the worker compare the registration by identity.
type synthSlot struct {
	cancel context.CancelFunc
}

// newSpeechWiring builds the per-session speech pipeline. It returns nil
// (with a warning already logged) when the sink cannot be built — the
// documented degradation: the session runs silent rather than failing the
// run because audio output is misconfigured.
func (m *Manager) newSpeechWiring(id string, speakerByMessage map[string]string, narratorFallback bool, runCtx context.Context, runner *pipeline.Runner, idx *matcher.Index, prewarmTexts map[string]string, voiceLat *frameVoiceSampler) *speechWiring {
	opts := m.opts.Speech
	var metrics *telemetry.Product
	var mLabels telemetry.Labels
	if runner != nil {
		metrics, mLabels = runner.Telemetry()
	}
	w := &speechWiring{
		id:               id,
		log:              m.log.Debug,
		hub:              m.opts.Hub,
		voices:           opts.Voices,
		assets:           opts.Assets,
		runner:           runner,
		metrics:          metrics,
		mLabels:          mLabels,
		voiceLat:         voiceLat,
		speakerByMessage: speakerByMessage,
		narratorFallback: narratorFallback,
		sem:              make(chan struct{}, speechWorkerSlots),
		utterances:       map[string]*utteranceInfo{},
		byGeneration:     map[int64]string{},
		genCancel:        map[int64]*synthSlot{},
		settled:          map[int64]bool{},
		catchUpRuns:      map[int64]*utteranceInfo{},
		lineClaim:        map[string]int64{},
		volume:           1.0,
	}
	// The catch-up policy's options (issue #25, ADR-0122): nil takes the
	// documented defaults; a construction that names a foreign policy word
	// is a wiring bug — the defaults run, and the log says why (the
	// settings route validates the word live, so this only trips here).
	var cuOpts speech.CatchUpOptions
	if opts.CatchUp != nil {
		cuOpts = *opts.CatchUp
	}
	if cu, err := cuOpts.NormalizeCatchUpOptions(); err == nil {
		w.catchUp = cu
	} else {
		m.log.Warn("catch-up options rejected; the session runs the documented defaults",
			"session_id", id, "error", err)
		if cu, err := (speech.CatchUpOptions{}).NormalizeCatchUpOptions(); err == nil {
			w.catchUp = cu
		}
	}
	w.initPrewarm(idx, prewarmTexts, opts.Prewarm)
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
			case *alsahw.Sink:
				kind = SinkALSA
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
	case opts.Sink == SinkALSA:
		// The alsahw sink addresses a profiled device by its stable hw:
		// identity and negotiates the profile's exact params. A device
		// without a proven profile is not addressable by it: inventing
		// params is how the receiver goes silent, so the documented
		// degradation (the pipewire sink's own precedent) is filesink
		// evidence with a warning, not a guessed negotiation.
		target := opts.PipeWireTarget
		prof, ok := audio.ProfileForDevice(target)
		if !ok {
			m.log.Warn("alsahw sink configured but the device has no proven hw_params profile; recording to the file sink as evidence instead",
				"session_id", id, "device", target)
			kind, sink, err = m.filesinkFor(id)
		} else {
			kind = SinkALSA
			params := alsahw.Params{
				Format:      prof.Format,
				Rate:        prof.Rate,
				Channels:    prof.Channels,
				PeriodSize:  uint32(prof.PeriodSize),
				PeriodCount: uint32(prof.PeriodCount),
			}
			sink, err = alsahw.New(prof.DeviceID(), params, time.Duration(audio.DefaultKeepAlive)*time.Millisecond)
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
	// The master volume (the appliance's one output level, ADR-0074) wraps
	// the device sink; the session's own live volume wraps it again. The two
	// multiply — both scale the samples exactly once, before the device —
	// and the master wrapper reads the live value at every playback, so a
	// Settings-page change applies from the next utterance.
	w.masterLoad = m.loadMasterVolume
	w.sink = newGainSink(newMasterSink(sink, m.loadMasterVolume))
	w.fence = &generationFence{}
	w.sched = speech.NewScheduler(w.sink, w.fence, &speechEventPublisher{hub: m.opts.Hub, w: w}, speech.Options{
		FencePeriod: speech.DefaultFencePeriod,
	})
	w.sched.Start(endCtx)
	return w
}

// initPrewarm resolves the session's prewarming policy and state. It is
// called by newSpeechWiring and by the white-box test harness, which builds
// the wiring without going through the manager.
func (w *speechWiring) initPrewarm(idx *matcher.Index, texts map[string]string, opts *PrewarmOptions) {
	w.idx = idx
	w.prewarmTexts = texts
	w.prewarmCfg = defaultPrewarmConfig(opts)
	w.prewarmKeys = map[string]struct{}{}
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
	// The catch-up batch the session's end takes down (issue #25, ADR-0122):
	// the pending lines are skipped with the reason. A run that reached the
	// scheduler settles here (the scheduler's loop is gone, so nothing else
	// would event it) — except the one playing, whose terminal the
	// scheduler's join already delivered. A run still in its TTS call
	// settles through its own worker: the end's cancellation wakes it (the
	// drain above) and the worker's own terminal is the one that counts —
	// the stop only clears the bookkeeping, so the two never double.
	var terminals []lineTerminal
	w.mu.Lock()
	if w.catchUp.Policy == speech.CatchUpPolicyCatchUp {
		if w.catchUpTimer != nil {
			t := w.catchUpTimer
			w.catchUpTimer = nil
			t()
		}
		pending := w.catchUpPending
		w.catchUpPending = nil
		for _, l := range pending {
			delete(w.lineClaim, l.Occurrence)
			terminals = append(terminals, lineTerminal{w.lineTerminalInfo(l), "skipped", "session_ended"})
		}
		for g, run := range w.catchUpRuns {
			delete(w.catchUpRuns, g)
			if g == w.playingGen || !run.submitted {
				continue // the join, or the worker, owns the terminal
			}
			w.settled[g] = true
			delete(w.byGeneration, g)
			terminals = append(terminals, lineTerminal{run, "failed", "session_ended"})
		}
	}
	w.mu.Unlock()
	w.fence.clearApproved()
	for _, t := range terminals {
		w.finishEnded(t.u, t.result, t.reason)
		if t.u.mergedLines != nil {
			for _, l := range t.u.mergedLines {
				w.finishLineTerminal(l, t.result, t.reason, t.u.generation)
			}
		}
	}
	// Prewarmed but not consumed by any commit before the session ended is
	// wasted budget (FR-11 stats): the count lands where the session's audio
	// work ends. Jobs still in flight count themselves when the end
	// cancellation reaches them, so nothing is double-booked.
	w.mu.Lock()
	w.prewarmWasted += int64(len(w.prewarmKeys))
	w.prewarmKeys = map[string]struct{}{}
	w.closed = true
	w.mu.Unlock()
}

// fenceAndCancel advances the audio fence to the committing generation and
// cancels every older one still queued or playing. It is called only on a
// SPEAKABLE commit (onCommit): a commit with no audio of its own must not
// fence, so a noise commit can never cancel the line the player is listening
// to (issue #6; ADR-0092). A newer committed speakable generation is exactly
// when stale audio must stop (docs/spec/01 invariant 2; docs/spec/12 "zero
// stale audio starts").
//
// The cancel list is built only from generations that have not settled yet:
// a generation that already ended or was cancelled is terminal, and cancelling
// it again would emit a second terminal for the same generation (issue #6).
func (w *speechWiring) fenceAndCancel(generation int64) {
	w.mu.Lock()
	// Under the catch-up policy the line that is playing keeps being heard:
	// a new speakable commit batches the new line instead of cutting the
	// playback (ADR-0122 decision 2). The playing generation is the only
	// one the fence spares — everything else queued or in its TTS round
	// trip was already rescued into the batch by onCommit (catch_up) or is
	// the latest_only cancellation this method exists to emit.
	skipPlaying := w.catchUp.Policy == speech.CatchUpPolicyCatchUp
	stale := make([]int64, 0, 4)
	var staleSynth []*synthSlot
	for g := range w.byGeneration {
		if g < generation && !w.settled[g] {
			if skipPlaying && g == w.playingGen {
				continue
			}
			stale = append(stale, g)
			// The fence is asking this one to die: settle it now so a later
			// commit cannot cancel it (and re-emit its terminal) again.
			w.settled[g] = true
			if s, ok := w.genCancel[g]; ok {
				staleSynth = append(staleSynth, s)
				delete(w.genCancel, g)
			}
		}
	}
	w.fence.set(generation)
	w.mu.Unlock()
	for _, s := range staleSynth {
		// The in-flight synthesis of a stale generation stops now: the
		// stream's partial is discarded, the utterance ends failed, and the
		// phase fence keeps it from starting later.
		s.cancel()
	}
	for _, g := range stale {
		w.sched.Cancel(g, "new_commit")
	}
}

// settle marks a generation terminal so the fence will never cancel — and
// re-emit a terminal for — it again. Every path that terminates a generation
// funnels through here: the fence (fenceAndCancel), the scheduler's own
// terminal events (noteSchedulerEvent) and the wiring's own terminals
// (finishEnded). It prunes the generation from the fence's cancel table
// (byGeneration); the correlation tables are kept, because the terminal's own
// event still needs the occurrence's ids when it is correlated.
func (w *speechWiring) settle(g int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.settled[g] = true
	delete(w.byGeneration, g)
}

// pause fences every utterance the session has queued or playing
// (docs/spec/08 "…/pause": "pause stops new speech and fences pending
// utterances"). It is the speech half of the pause control: the reducer
// half (the same control message, the same single writer) stopped the
// commits and cancelled the in-flight recognitions, and here the audio
// stops. Every generation is cancelled — the *current* one included, which
// is the difference from fenceAndCancel: a pause pressed mid-line ends that
// line, and the scheduler's cancellation table keeps a late Submit of any of
// these generations from starting after the pause.
//
// The fence is deliberately *not* advanced: a pause is not a textbox
// advance, and the commit path's fence is what the resume's next commit
// fences against. Advancing it here would look like a box advance the player
// never made.
//
// Settle is still honoured (issue #6): a generation that already ended is not
// re-cancelled, so a pause cannot emit a second terminal for it. The
// generations the pause does cancel are settled here, so a later commit
// cannot cancel them again.
// lineTerminal is one terminal the pause/stop path claims under its lock
// and emits after the lock is down (the wiring's publish-unlocked rule).
type lineTerminal struct {
	u              *utteranceInfo
	result, reason string
}

func (w *speechWiring) pause() {
	w.mu.Lock()
	gens := make([]int64, 0, len(w.byGeneration))
	for g := range w.byGeneration {
		if w.settled[g] {
			continue // terminal: a second cancel would be a second result
		}
		gens = append(gens, g)
		w.settled[g] = true
	}
	var synths []*synthSlot
	for g, s := range w.genCancel {
		if w.settled[g] {
			continue
		}
		synths = append(synths, s)
		delete(w.genCancel, g)
	}
	var terminals []lineTerminal
	// The catch-up batch the pause takes down (issue #25, ADR-0122): the
	// pending lines are skipped with the reason, the in-flight runs are
	// cancelled at the source (their synthesis's cancellation wakes the
	// workers, which end the utterance failed) and their generations are
	// cancelled (the scheduler drops the queued items through its own
	// terminal), and the approved set empties — a batch the session is not
	// speaking keeps nothing speakable. The playing run stops through the
	// fence, like the lines.
	if w.catchUp.Policy == speech.CatchUpPolicyCatchUp {
		if w.catchUpTimer != nil {
			t := w.catchUpTimer
			w.catchUpTimer = nil
			t()
		}
		pending := w.catchUpPending
		w.catchUpPending = nil
		for _, l := range pending {
			delete(w.lineClaim, l.Occurrence)
			terminals = append(terminals, lineTerminal{w.lineTerminalInfo(l), "skipped", "paused"})
		}
		for g, run := range w.catchUpRuns {
			if g == w.playingGen {
				continue
			}
			delete(w.catchUpRuns, g)
			w.settled[g] = true
			delete(w.byGeneration, g)
			if run.synthSlot != nil {
				// The worker wakes on the cancellation and sees the flag: it
				// emits nothing — the pause owns the run's terminal (and its
				// lines'), so the worker's own would be a second one.
				run.rescued = true
				synths = append(synths, run.synthSlot)
				run.synthSlot = nil
			}
			if run.submitted {
				gens = append(gens, g)
			} else {
				terminals = append(terminals, lineTerminal{run, "failed", "paused"})
			}
		}
	}
	w.mu.Unlock()
	w.fence.clearApproved()
	for _, s := range synths {
		// The in-flight synthesis of a paused generation stops at the source:
		// the stream's partial is discarded, the utterance ends failed, and
		// the scheduler's table keeps it from starting later.
		s.cancel()
	}
	for _, g := range gens {
		w.sched.Cancel(g, "paused")
	}
	for _, t := range terminals {
		w.finishEnded(t.u, t.result, t.reason)
		if t.u.mergedLines != nil {
			for _, l := range t.u.mergedLines {
				w.finishLineTerminal(l, t.result, t.reason, t.u.generation)
			}
		}
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
	text := strings.TrimSpace(d.Text)

	// The commit payload is the eligibility answer: an abstained box
	// (message_id "unknown"), a non-narratable line, or a decision that
	// refused speech is not a line the narrator speaks. It gets no speech
	// events at all — and it must NOT fence: a commit with no audio of its
	// own must not cancel the line the player is listening to (issue #6;
	// ADR-0092). Only a speakable commit advances the fence, because only a
	// speakable commit is a line whose audio supersedes the previous one.
	if !d.Eligible || d.MessageID == "" || d.MessageID == "unknown" || text == "" {
		return
	}

	u := &utteranceInfo{
		occurrence:  d.Occurrence,
		messageID:   d.MessageID,
		regionID:    d.RegionID,
		generation:  d.Generation,
		text:        text,
		traceID:     e.TraceID,
		character:   w.speakerFor(d.MessageID),
		committedAt: time.Now().UTC(),
	}
	w.register(u)
	// The status bar's commit half (status.go): this wall clock is the
	// sample's start, and the occurrence id is what speech.started carries
	// back to complete it. Booked before the line is routed so a batched or
	// backpressure-skipped line still measures what it skipped from.
	if w.voiceLat != nil {
		w.voiceLat.noteCommit(u.occurrence, u.committedAt)
	}
	// A speakable commit is the advance that makes the previous line's audio
	// stale: the fence moves to this generation and the superseded
	// generation(s) stop (docs/spec/01 invariant 2). Under the catch-up
	// policy the commit first routes the new line and rescues the older
	// lines the batch takes over (ADR-0122), and the fence then cancels only
	// what the policy did not batch: under latest_only the older generations
	// (the historical shape), under catch_up nothing (the rescued generations
	// are settled and the playing one is spared).
	batched := w.catchUpBatchCommit(u)
	w.fenceAndCancel(d.Generation)
	if batched {
		// The batched line's commit is still a request (the counter and the
		// event say the line was asked for), but the line's one terminal is
		// the batch's outcome, not this commit's: the merged run that speaks
		// it ends it (the run's result), and the window's fence, the pause
		// or the max-age drop skips it (their reasons). The generation is
		// settled with the batch's claim, so the fence never cancels — and
		// re-emits a terminal for — it again.
		w.mu.Lock()
		w.requested++
		w.current = &UtteranceState{Phase: "requested", Generation: u.generation, UtteranceID: u.occurrence, At: u.committedAt}
		w.mu.Unlock()
		w.emitSpeech(events.SpeechRequested, u, map[string]any{
			"dialogue_occurrence_id": u.occurrence,
			"message_id":             u.messageID,
			"generation":             u.generation,
			"region_id":              u.regionID,
			"text":                   u.text,
			"character_id":           u.character,
			"trace_id":               u.traceID,
		})
		// No terminal here: the batch owns the line's one terminal (its
		// outcome — the merged run's, the window's fence, the pause — emits
		// it under the claim the batch holds, ADR-0092's one-terminal rule
		// carried onto the batch's lines by lineClaim).
		w.prewarm(u.messageID)
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
		"character_id":           u.character,
		"trace_id":               u.traceID,
	})
	w.wg.Add(1)
	go w.speak(u)
	// Successor prewarming (FR-11): the just-committed line's weighted
	// successors are synthesised in the background into the same cache, so
	// the later commit of one of them resolves from the cache. The fence
	// just moved, so this batch's "likely next" supersedes the previous
	// batch's — the previous batch is cancelled inside the call. (The
	// backpressure skip above returns earlier: the session's TTS slots are
	// all busy, and a prewarm would only queue behind them.)
	w.prewarm(u.messageID)
}

// catchUpBatchCommit is the catch-up policy's commit-side decision
// (ADR-0122 decision 2), taken under w.mu: it reports whether u goes into
// the batch (true) instead of the immediate path (false), and does the
// bookkeeping of the decision — the rescue of the older generations the
// batch takes over, the window-drop of the pending batch's old prefix, the
// line's append and the flush timer's arm.
//
// false — the immediate path: the policy is latest_only, or speech is not
// busy (nothing playing, no batch pending, no immediate utterance in its
// TTS round trip: the fast path keeps the first line's commit→voice
// latency). No rescue, no batch, no timer.
//
// true — the line joins the batch. The older unsettled generations are
// rescued first, in the order the player left them: the one that is
// PLAYING is spared (its lines are being heard; that is the behaviour the
// issue asked for), and every other one — in its TTS round trip or queued
// for the scheduler — is taken over: settled here (the batch owns the
// lines' fate from here, so the fence's cancel list is built around the
// rescue, not under it), its in-flight synthesis cancelled at the source
// (the worker, flagged rescued, emits no terminal of its own) and the line
// appended to the pending batch under its own committed clock. A pending
// line the new line is more than the batch window after is fenced on the
// spot (terminal skipped, catch_up_stale) — the issue's "Fencing (ADR 0092)
// still applies to anything older than the batch window", made local.
// catchUpBatchCommit is the catch-up policy's commit-side decision
// (ADR-0122 decision 2), taken under w.mu: it reports whether u goes into
// the batch (true) instead of the immediate path (false), and does the
// bookkeeping of the decision — the rescue of the older generations the
// batch takes over, the window-drop of the pending batch's old prefix, the
// line's append and the flush timer's arm.
//
// false — the immediate path: the policy is latest_only, or speech is not
// busy (nothing playing, no batch pending, no immediate utterance in its
// TTS round trip: the fast path keeps the first line's commit→voice
// latency). No rescue, no batch, no timer.
//
// true — the line joins the batch. The older unsettled generations are
// rescued first, in the order the player left them: the one that is
// PLAYING is spared (its lines are being heard; that is the behaviour the
// issue asked for), and every other one — in its TTS round trip or queued
// for the scheduler — is taken over: settled here (the batch owns the
// lines' fate from here, so the fence's cancel list is built around the
// rescue, not under it), its in-flight synthesis cancelled at the source
// (the worker, flagged rescued, emits no terminal of its own) and the line
// appended to the pending batch under its own committed clock. A pending
// line the new line is more than the batch window after is fenced on the
// spot (terminal skipped, catch_up_stale) — the issue's "Fencing (ADR
// 0092) still applies to anything older than the batch window", made
// local.
// catchUpBatchCommit is the catch-up policy's commit-side decision
// (ADR-0122 decision 2), taken under w.mu: it reports whether u goes into
// the batch (true) instead of the immediate path (false), and does the
// bookkeeping of the decision — the rescue of the older generations the
// batch takes over, the window-drop of the pending batch's old prefix, the
// line's append and the flush timer's arm.
//
// false — the immediate path: the policy is latest_only, or speech is not
// busy (nothing playing, no batch pending, no immediate utterance in its
// TTS round trip: the fast path keeps the first line's commit→voice
// latency). No rescue, no batch, no timer.
//
// true — the line joins the batch. The older unsettled generations are
// rescued first, in the order the player left them: the one that is
// PLAYING is spared (its lines are being heard; that is the behaviour the
// issue asked for), and every other one — in its TTS round trip, queued
// for the scheduler, or a merged run in flight — is taken over: settled
// here (the batch owns the lines' fate from here, so the fence's cancel
// list is built around the rescue, not under it), its in-flight synthesis
// cancelled at the source (the worker, flagged rescued, emits no terminal
// of its own) and the covered lines appended to the pending batch under
// their own committed clocks. A pending line the new line is more than the
// batch window after is fenced on the spot (terminal skipped, catch_up
// stale) — the issue's "Fencing (ADR 0092) still applies to anything older
// than the batch window", made local.
//
// The terminal accounting (ADR-0092, one terminal per generation): the
// rescue emits none — a rescued line's terminal is the batch's outcome
// (the merged run's settled line event, the window-drop, or the pause and
// stop), so the line's generation reaches exactly one terminal. What the
// rescue does emit is only the merged run's own run-level terminal when it
// takes a merged run over (its requested already went out at the flush).
func (w *speechWiring) catchUpBatchCommit(u *utteranceInfo) bool {
	// The terminals the batch claims are emitted only after the commit lock
	// is down: the hub's publish never takes w.mu, and the wiring's rule
	// (publish unlocked) is what keeps a synchronous subscriber from
	// re-entering the wiring on a held lock.
	type terminal struct {
		u              *utteranceInfo
		result, reason string
	}
	w.mu.Lock()
	if w.catchUp.Policy != speech.CatchUpPolicyCatchUp {
		w.mu.Unlock()
		return false
	}
	busy := w.playingGen != 0 || len(w.catchUpPending) > 0 || len(w.genCancel) > 0
	if !busy {
		w.mu.Unlock()
		return false
	}
	// The playing line is the session's active playback, and the commit must
	// not cut it (issue #25's point: a tap that lands while the player is
	// listening keeps the line playing to the end, the new line batches). The
	// scheduler's own fence check would drop the playback the moment the
	// fence's latest moves past its generation, so the approval is what the
	// "leave it to run out" is implemented with: the playing generation stays
	// current for the rest of its playback, and the playback's terminal is
	// where the approval leaves (noteSchedulerEvent unapproves it).
	if w.playingGen != 0 {
		w.fence.approve(w.playingGen, speech.DefaultCatchUpApprovedCap)
	}
	// The rescue: every older generation that is not the playing one is
	// taken over by the batch. Settling here (not in fenceAndCancel) is what
	// keeps the fence's cancel list from double-cancelling the rescue — the
	// rescued generations are terminal for the fence's purposes the moment
	// the batch claims them. The one-terminal-per-generation rule (ADR-0092)
	// is enforced by the settled map every terminal path consults: a
	// submitted utterance dies through the scheduler's cancel (which emits
	// its terminal), an in-flight one through the rescue (which emits it
	// while the flagged worker stays silent), and a covered line's terminal
	// is the batch's outcome unless an earlier source already settled it.
	var terminals []terminal
	var staleSynth []*synthSlot
	var staleCancel []int64
	rescue := func(g int64) {
		if g >= u.generation || w.settled[g] || g == w.playingGen {
			return
		}
		// A merged run (its table) wins the generation when both tables
		// name it: the run's utterance is the audio's owner, and a commit
		// that collided on the generation number registered its line
		// alongside it, not over it.
		line := w.catchUpRuns[g]
		if line == nil {
			if occ, ok := w.byGeneration[g]; ok {
				line = w.utterances[occ]
			}
		}
		if line == nil {
			return
		}
		line.rescued = true // the worker skips its own terminal and its submit
		for _, l := range coveredLines(line) {
			if !catchUpLinePending(w.catchUpPending, l.Occurrence) {
				w.catchUpPending = append(w.catchUpPending, l)
				// The line's terminal is claimed by the batch again: no run
				// owns it while it waits, and the run it last spoke with (its
				// late terminal, if any) settles nothing under the new claim.
				w.lineClaim[l.Occurrence] = 0
			}
		}
		if s, ok := w.genCancel[g]; ok {
			staleSynth = append(staleSynth, s)
			delete(w.genCancel, g)
		}
		if line.synthSlot != nil {
			staleSynth = append(staleSynth, line.synthSlot)
			line.synthSlot = nil
		}
		if !line.submitted {
			// In its TTS round trip (or before it): the rescue owns the
			// terminal. A merged run's requested already went out at the
			// flush, so the run's own skipped terminal names the rescue; a
			// standalone line's terminal stays the batch's outcome.
			if line.mergedLines != nil {
				terminals = append(terminals, terminal{line, "skipped", "catch_up_batched"})
			}
		} else {
			// Queued for, or accepted by, the scheduler: the scheduler's
			// cancel emits its terminal (cancelled: catch_up_batched) — the
			// rescue must not emit one for the same generation.
			staleCancel = append(staleCancel, g)
		}
		w.settled[g] = true
		delete(w.byGeneration, g)
		delete(w.catchUpRuns, g)
		w.catchUpBatched++
	}
	for g := range w.byGeneration {
		rescue(g)
	}
	for g := range w.catchUpRuns {
		rescue(g)
	}
	// The window: the new line fences the pending prefix it is more than the
	// window after (oldest first — the prefix property, the batcher's own
	// rule), and the fenced lines' terminal names the fence.
	if n := speech.BatchWindowDrop(w.catchUpPending, u.committedAt, w.catchUp.BatchWindow); n > 0 {
		for i := 0; i < n; i++ {
			w.catchUpDropped++
			line := w.lineTerminalInfo(w.catchUpPending[i])
			delete(w.lineClaim, w.catchUpPending[i].Occurrence)
			terminals = append(terminals, terminal{line, "skipped", "catch_up_stale"})
		}
		w.catchUpPending = w.catchUpPending[n:]
	}
	w.catchUpPending = append(w.catchUpPending, catchUpLineOf(u))
	// The batch owns the line's terminal from here: no run claims it yet
	// (claim 0), and the window's fence, the pause and the flush's drops
	// all settle it under that claim.
	w.lineClaim[u.occurrence] = 0
	w.settled[u.generation] = true // the batch owns the line from here
	delete(w.byGeneration, u.generation)
	if w.catchUpTimer == nil {
		w.armCatchUpTimerLocked(w.catchUp.BatchWindow)
	}
	w.mu.Unlock()
	for _, s := range staleSynth {
		// The in-flight synthesis of a rescued utterance stops at the source:
		// the partial is discarded (the worker, flagged, emits nothing) and
		// the line's content speaks in the merged utterance.
		s.cancel()
	}
	for _, g := range staleCancel {
		// The scheduler emits the cancelled terminal for the generation it
		// dropped (the reason travels, ADR-0092's vocabulary).
		w.sched.Cancel(g, "catch_up_batched")
	}
	for _, t := range terminals {
		w.finishEnded(t.u, t.result, t.reason)
	}
	return true
}

// catchUpLineOf is one committed line's batcher form (the lock is held or
// the fields are immutable: the utteranceInfo's fields are written once at
// the commit, before the wiring shares the pointer).
func (w *speechWiring) catchUpLineOf(u *utteranceInfo) speech.CatchUpLine {
	return speech.CatchUpLine{
		Generation:  u.generation,
		Occurrence:  u.occurrence,
		MessageID:   u.messageID,
		RegionID:    u.regionID,
		Text:        u.text,
		Speaker:     u.character,
		CommittedAt: u.committedAt,
	}
}

// coveredLines is one utterance's lines the batch takes over: the utterance
// itself when it is a standalone line, its covered lines when it is a merged
// run (the run's lines go back into the batch individually, under their own
// occurrences and committed clocks).
func coveredLines(u *utteranceInfo) []speech.CatchUpLine {
	if u.mergedLines != nil {
		return u.mergedLines
	}
	return []speech.CatchUpLine{speech.CatchUpLine{
		Generation:  u.generation,
		Occurrence:  u.occurrence,
		MessageID:   u.messageID,
		RegionID:    u.regionID,
		Text:        u.text,
		Speaker:     u.character,
		CommittedAt: u.committedAt,
	}}
}

// catchUpLinePending reports whether the line (by occurrence) is already in
// the batch: a rescue that meets the same line twice must not double it.
func catchUpLinePending(pending []speech.CatchUpLine, occurrence string) bool {
	for _, l := range pending {
		if l.Occurrence == occurrence {
			return true
		}
	}
	return false
}

// lineTerminalInfo is the correlation record a pending line's terminal
// carries: its own committed utterance when the table still holds it, a
// record built from the line otherwise (a table entry evicted by the
// correlation cap — the ids the event needs all live on the line).
func (w *speechWiring) lineTerminalInfo(l speech.CatchUpLine) *utteranceInfo {
	if u := w.utterances[l.Occurrence]; u != nil {
		return u
	}
	return &utteranceInfo{
		occurrence:  l.Occurrence,
		messageID:   l.MessageID,
		regionID:    l.RegionID,
		generation:  l.Generation,
		text:        l.Text,
		character:   l.Speaker,
		committedAt: l.CommittedAt,
	}
}

// lookupUtterance is the correlation table's read (the lock round trip a
// caller without the lock takes; nil when the table evicted the entry).
func (w *speechWiring) lookupUtterance(occurrence string) *utteranceInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.utterances[occurrence]
}

// catchUpLineOf is one committed line's batcher form: the fields the
// batcher decides on and the terminals carry, read from the line's
// correlation record (written once at the commit, before the wiring shares
// the pointer — the read needs no lock beyond the caller's).
func catchUpLineOf(u *utteranceInfo) speech.CatchUpLine {
	return speech.CatchUpLine{
		Generation:  u.generation,
		Occurrence:  u.occurrence,
		MessageID:   u.messageID,
		RegionID:    u.regionID,
		Text:        u.text,
		Speaker:     u.character,
		CommittedAt: u.committedAt,
	}
}

// armCatchUpTimerLocked arms the batch's flush timer (the lock is held):
// the window elapsing since the batch's first line flushes. The timer fires
// on the runtime's own goroutine (time.AfterFunc), so the session's event
// loop is never blocked waiting for narration; the timer's Stop is stored
// as catchUpTimer so a later flush or the session's end can disarm it.
func (w *speechWiring) armCatchUpTimerLocked(window time.Duration) {
	if window <= 0 {
		window = speech.DefaultCatchUpBatchWindow
	}
	timer := time.AfterFunc(window, func() { w.flushCatchUp("catch_up_window") })
	w.catchUpTimer = timer.Stop
}

// armCatchUpTimerLocked arms the batch's flush timer (the lock is held):
// the window elapsing since the batch's first line flushes. The timer fires
// on the runtime's own goroutine (time.AfterFunc), so the session's event
// loop is never blocked waiting for narration; the timer's Stop is stored
// as catchUpTimer so a later flush or the session's end can disarm it.

// settleLocked is settle with the lock already held (the batch's commit
// path settles under the same lock it rescues under).
func (w *speechWiring) settleLocked(g int64) {
	w.settled[g] = true
	delete(w.byGeneration, g)
}

// finishEndedLocked is finishEnded with the lock already held: the batch's
// commit path terminalises the rescued and fenced lines under the same lock
// it claims them, so a concurrent flush cannot read a line between its
// claim and its terminal. (The emit is outside the lock, as everywhere else
// in the wiring: the hub's publish never takes w.mu.)
func (w *speechWiring) finishEndedLocked(u *utteranceInfo, result, reason string) {
	if w.closed {
		return
	}
	w.localEnded++
	w.current = &UtteranceState{Phase: result, Result: result, Generation: u.generation, UtteranceID: u.occurrence, At: time.Now().UTC()}
	w.settleLocked(u.generation)
	data := map[string]any{
		"dialogue_occurrence_id": u.occurrence,
		"message_id":             u.messageID,
		"generation":             u.generation,
		"region_id":              u.regionID,
		"result":                 result,
		"character_id":           u.character,
		"trace_id":               u.traceID,
	}
	if reason != "" {
		data["reason"] = reason
	}
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	ctx := w.endCtx
	if w.endCtx.Err() != nil {
		ctx = context.Background()
	}
	if _, err := w.hub.Publish(ctx, events.Envelope{
		SchemaVersion: 1,
		SessionID:     w.id,
		TraceID:       u.traceID,
		Name:          events.SpeechEnded,
		Data:          b,
	}); err != nil {
		w.log("speech event publish failed", "session_id", w.id, "event", "speech.ended", "error", err)
	}
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
		// Never a substitute (docs/spec/07). The reason names the speaker
		// whose voice is missing, in the event (character_id + reason) and the
		// log, so an all-skip session is diagnosable (issue #6).
		w.log("speech: voice skip — no approved voice",
			"session_id", w.id, "character_id", u.character, "message_id", u.messageID,
			"generation", u.generation, "reason", why)
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
	defer cancel()
	// The synthesis context is the utterance's own cancellable scope, on top
	// of the deadline: a fence that moves while this line is still being
	// synthesised cancels it at the source (the stream's partial is
	// discarded, the utterance ends failed) instead of after it has started
	// (ADR-0057; docs/spec/12 "TTS partly streams before cancellation").
	synthCtx, synthCancel := context.WithCancel(pctx)
	slot := &synthSlot{cancel: synthCancel}
	w.mu.Lock()
	w.genCancel[u.generation] = slot
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.genCancel[u.generation] == slot {
			delete(w.genCancel, u.generation)
		}
		w.mu.Unlock()
		slot.cancel()
	}()
	ttsStart := time.Now()
	// WithKeyInput carries the message id into the cache: the key's hash cannot
	// be inverted, so the index row the cache writes persists the id it got
	// from here (ADR-0107: a profile purge addresses "the cache rows of this
	// profile's messages" through that column).
	res, err := w.voices.Speech.GetOrGenerate(speech.WithKeyInput(synthCtx, u.messageID), key, speech.PriorityCurrent, func(ctx context.Context) (tts.Audio, error) {
		return rv.generate(ctx, u.text)
	})
	if err != nil && w.lineRescued(u) {
		// The catch-up batch took the line over while its synthesis ran
		// (ADR-0122 decision 2): the rescue cancelled the synthesis, the
		// worker emits nothing (the batch's terminal for the line is the one
		// that counts), and it does not retry — regenerating a key the batch
		// will merge would only spend the TTS budget.
		return
	}
	if err != nil && ctx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		// The shared result may be a CANCELLED background prewarm of this very
		// key: the fence moved while its synthesis ran, and the cache finished
		// the shared call with the prewarm's context error for every caller
		// waiting on it. The committed line is still current, so it retries ONCE
		// as the fresh leader under its own context (bounded: one retry, and
		// singleflight keeps at most one new generation for the key). Any other
		// provider failure keeps its own error and does not retry.
		pctx2, cancel2 := context.WithTimeout(ctx, speechTTSTimeout)
		defer cancel2()
		synthCtx2, synthCancel2 := context.WithCancel(pctx2)
		defer synthCancel2()
		slot := &synthSlot{cancel: synthCancel2}
		w.mu.Lock()
		w.genCancel[u.generation] = slot
		w.mu.Unlock()
		defer func() {
			w.mu.Lock()
			if w.genCancel[u.generation] == slot {
				delete(w.genCancel, u.generation)
			}
			w.mu.Unlock()
			slot.cancel()
		}()
		r, err2 := w.voices.Speech.GetOrGenerate(speech.WithKeyInput(synthCtx2, u.messageID), key, speech.PriorityCurrent, func(ctx context.Context) (tts.Audio, error) {
			return rv.generate(ctx, u.text)
		})
		if err2 == nil {
			res, err = r, nil
		}
	}
	// The TTS stage of docs/spec/13 "Metrics": request-to-resolved, cache
	// outcome and all, in the same domain the voice wiring's own status
	// sampler uses (wall clock). A failed or ended synthesis is not a sample
	// of the stage's latency — its event says so — and books nothing.
	if err == nil {
		w.metrics.TTSResolved(w.mLabels, res.FromCache, time.Since(ttsStart))
	}
	if err != nil {
		if w.lineRescued(u) {
			// The catch-up batch took the line over mid-synthesis (ADR-0122
			// decision 2): the synthesis's cancellation error is the rescue's
			//, the worker emits nothing (the batch's terminal for the line is
			// the one that counts), and no retry regenerates the key.
			return
		}
		if ctx.Err() != nil {
			// The session ended while this line was still synthesising: it
			// was requested, so it ends with the reason, not in silence.
			w.finishEnded(u, "failed", "session_ended")
			return
		}
		w.finishEnded(u, "failed", providerErrorKind(err))
		return
	}
	if !res.FromCache {
		// QA on insert (FR-10, ADR 0061): the freshly committed line is read
		// back; an over-bound transcript regenerates once and the verdict
		// (with WER + transcript) persists on the entry. Cache hits keep
		// their stored verdict. QA never blocks playback: when it cannot run
		// the entry stays "not run" and the line plays.
		res, _ = w.voices.QA.QAInsert(synthCtx, key, u.text, res, func(ctx context.Context) (tts.Audio, error) {
			return rv.generate(ctx, u.text)
		})
	}
	if res.FromCache {
		w.mu.Lock()
		w.cacheHits++
		w.mu.Unlock()
		// FR-11's prewarm hit: the line this commit resolved from the cache
		// was one THIS session prewarmed, so the commit paid no TTS round
		// trip. (A cache hit on a key this session never prewarmed — filled
		// by an earlier session — is a plain cache hit, not a prewarm hit.)
		if w.prewarmKeyConsumed(key) {
			w.metrics.PrewarmHit(w.mLabels)
		}
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
		"character_id":           u.character,
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
	// The per-voice gain (docs/spec/07 "per-voice gain", ADR-0074): applied to
	// the samples at playback, after the cache key is computed — a gain change
	// re-renders nothing, it scales what the cache already holds, and a line
	// served from the cache plays at the voice's current gain.
	wav := res.WAV
	if g := rv.profile.Gain; g != 0 && g != 1 {
		pcm, derr := speech.DecodeWAV(res.WAV)
		if derr != nil {
			w.finishEnded(u, "failed", "voice_gain:undecodable_wav")
			return
		}
		if g < 0 {
			g = 0
		}
		if g > audio.GainMax {
			g = audio.GainMax
		}
		wav = speech.EncodeWAV(scalePCM(pcm.Data, g, pcm.BitsPerSample),
			pcm.SampleRate, pcm.Channels, pcm.BitsPerSample)
	}
	if w.lineRescued(u) {
		// The catch-up batch took the line over after its synthesis
		// (ADR-0122 decision 2): the line's content speaks in the merged
		// utterance, so the standalone submit never happens and the worker
		// emits no terminal of its own.
		return
	}
	w.sched.Submit(speech.UtteranceReady{
		SessionID:        w.id,
		Generation:       u.generation,
		UtteranceID:      u.occurrence,
		Priority:         speech.PriorityCurrent,
		WAV:              wav,
		ExpectedDuration: res.Duration,
	})
	w.mu.Lock()
	u.submitted = true
	w.mu.Unlock()
}

// ── catch-up narration (issue #25, ADR-0122) ------------------------------

// setCatchUp live-switches the catch-up policy (issue #25, ADR-0122): the
// settings control (the session.settings_changed event) moves it mid-session.
// The switch decides what happens with lines committed *next* — the one
// transition with in-flight state is catch_up → latest_only while a batch is
// pending: those lines are committed (their settlements claimed into the
// batch), so the batch is flushed now and its merged runs speak back-to-back
// instead of being silently dropped — every committed line still gets spoken
// and settled, and the subsequent commits run the immediate shape. A switch
// the other way, or to the same word, is a flag flip alone; an empty word
// (an event without the field) is a no-op.
func (w *speechWiring) setCatchUp(policy string) {
	if policy == "" {
		return
	}
	w.mu.Lock()
	was := w.catchUp.Policy
	w.catchUp.Policy = policy
	pending := len(w.catchUpPending)
	w.mu.Unlock()
	if was != policy && policy == speech.CatchUpPolicyLatestOnly && pending > 0 {
		w.flushCatchUp("policy_switch")
	}
}

// lineRescued reports whether the catch-up batch took the utterance over
// (the flag the rescue sets under w.mu; the worker reads it under the lock
// so the handoff is data-race free).
func (w *speechWiring) lineRescued(u *utteranceInfo) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return u.rescued
}

// NotifyBoxChange implements speech.BoxChangeNotifier: the box-change
// priority (ADR-0122 decision 7). The capture/pipeline side calls it the
// moment a dialogue box appears or changes — ahead of the OCR read and the
// commit that follows — and a pending catch-up batch flushes on the spot so
// the merged playback starts on the new box instead of at the window's end.
// Cheap and non-blocking: at most one lock round trip and one goroutine
// spawn, and a no-op when the policy is latest_only or the batch is empty.
func (w *speechWiring) NotifyBoxChange() {
	w.mu.Lock()
	if w.closed || w.catchUp.Policy != speech.CatchUpPolicyCatchUp || len(w.catchUpPending) == 0 {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.flushCatchUp("box_change")
}

// flushCatchUp takes the pending batch (under the lock) and runs the flush
// on its own goroutine: the merge, the per-run TTS and the submits never
// touch the session's event loop (docs/spec/07 "audio sink must never block
// session event loop"). The entry points are the window's elapse (the arm
// timer), a playback terminal (noteSchedulerEvent: the issue's "when
// playback ends") and the box change (NotifyBoxChange). Taking the batch
// and disarming the timer under the lock is what makes two entry points
// meet on an empty batch instead of double-flushing one.
func (w *speechWiring) flushCatchUp(trigger string) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	if w.catchUpTimer != nil {
		t := w.catchUpTimer
		w.catchUpTimer = nil
		t()
	}
	batch := w.catchUpPending
	w.catchUpPending = nil
	w.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	w.log("catch-up batch flush", "session_id", w.id, "trigger", trigger, "lines", len(batch))
	go w.runCatchUpBatch(batch, trigger)
}

// runCatchUpBatch is the flush: the batcher's own decisions (split, max-age
// drop) on the shared core, then one TTS request per speaker run, in
// commit order (ADR-0122 decision 3). The runs share the batch's newest
// committed generation as their bookkeeping base — run i books generation
// base+i — and the fence approves every one of them (decision 4) BEFORE the
// first submit, so a commit that lands mid-flush cannot fence a run that
// the fence already let speak: it batches its own line instead.
func (w *speechWiring) runCatchUpBatch(batch []speech.CatchUpLine, trigger string) {
	if len(batch) == 0 {
		return
	}
	// The session ended while the batch was in flight (the playback terminal
	// or the window met the shutdown): no run is synthesised — the session
	// is not speaking — and every line the batch holds ends with the reason
	// (docs/spec/13 "did it speak, and why not" still holds on the way out;
	// the lines' claims are the flush's own, so nothing else settles them).
	if w.endCtx.Err() != nil {
		for _, l := range batch {
			w.finishLineTerminal(l, "skipped", "session_ended", 0)
		}
		return
	}
	now := time.Now().UTC()
	var batchBase int64
	for _, l := range batch {
		if l.Generation > batchBase {
			batchBase = l.Generation
		}
	}
	runs := speech.SplitRuns(batch)

	type mergedRun struct {
		u   *utteranceInfo
		run speech.CatchUpRun
		gen int64
	}
	var merged []mergedRun
	// The per-utterance max age (issue #25 item 1): a run whose audio would
	// not be ready until after the newest line it covers committed plus the
	// max age is dropped before it starts (the ready instant is the flush
	// instant — the TTS latency lands on top of it, which is why the bound
	// is a bound and not a target). This is the dismiss-without-successor
	// backstop ADR-0092 deferred: the box is gone, no commit moved the
	// fence, and only the age bound keeps the late audio off the sink.
	for i, run := range runs {
		if now.Sub(run.LatestCommittedAt()) > w.catchUp.MaxUtteranceAge {
			// The drop's terminal goes out under the claim the batch holds
			// (claim 0, still held: finishLineTerminal emits and releases it
			// — deleting it first would leave the line without its terminal,
			// the one-terminal rule the claim exists to enforce).
			w.mu.Lock()
			w.catchUpDropped += int64(len(run.Lines))
			w.mu.Unlock()
			for _, l := range run.Lines {
				w.finishLineTerminal(l, "skipped", "catch_up_stale", 0)
			}
			continue
		}
		first := run.Lines[0]
		var traceID string
		if lu := w.lookupUtterance(first.Occurrence); lu != nil {
			traceID = lu.traceID
		}
		u := &utteranceInfo{
			occurrence:  games.NewID(),
			messageID:   speech.MergedOriginalID(run.MessageIDs()),
			regionID:    first.RegionID,
			generation:  batchBase + int64(i),
			text:        run.Text,
			traceID:     traceID,
			character:   run.Speaker,
			committedAt: now,
			mergedLines: append([]speech.CatchUpLine(nil), run.Lines...),
		}
		merged = append(merged, mergedRun{u: u, run: run, gen: u.generation})
	}
	if len(merged) == 0 {
		return
	}
	// Register the merged utterances (the correlation tables the speech
	// events join on) and the bookkeeping generations, and approve the
	// fence — all before the first submit.
	w.mu.Lock()
	for _, m := range merged {
		if _, ok := w.utterances[m.u.occurrence]; !ok {
			w.utterances[m.u.occurrence] = m.u
		}
		if _, taken := w.catchUpRuns[m.gen]; !taken {
			w.byGeneration[m.gen] = m.u.occurrence
			w.catchUpRuns[m.gen] = m.u
		}
		// The run claims the terminal of every line it speaks: a run's
		// terminal settles a line only under the claim this flush wrote, so
		// a late run whose lines a newer commit re-batched settles nothing.
		for _, l := range m.run.Lines {
			w.lineClaim[l.Occurrence] = m.gen
		}
		w.catchUpMerged++
	}
	w.mu.Unlock()
	for _, m := range merged {
		w.fence.approve(m.gen, speech.DefaultCatchUpApprovedCap)
	}
	for _, m := range merged {
		w.emitSpeech(events.SpeechRequested, m.u, map[string]any{
			"dialogue_occurrence_id": m.u.occurrence,
			"message_id":             m.u.messageID,
			"message_ids":            m.run.MessageIDs(),
			"generation":             m.gen,
			"region_id":              m.u.regionID,
			"text":                   m.u.text,
			"character_id":           m.u.character,
			"trace_id":               m.u.traceID,
			"merged":                 len(m.run.Lines) > 1,
			"trigger":                trigger,
		})
		w.wg.Add(1)
		go w.speakMerged(m.u, m.run)
	}
}

// speakMerged is the TTS worker of one merged run: the same stages as speak
// (voice → key → synthesise (cached) → delay → mute → the scheduler's
// fence-checked start), with three catch-up differences. The voice is
// resolved from the run's speaker, not from the message id (the merged id
// is not in the speaker table). The cache key's original-message form is
// the versioned merged id (issue #25 item 3: the merged utterance's cache
// keys include the merged message ids). And the in-flight synthesis's cancel
// lives on the utterance (synthCancel), not in the per-generation table —
// several runs share the batch's generation base, and the rescue cancels
// the flagged worker at the source instead of a table entry.
func (w *speechWiring) speakMerged(u *utteranceInfo, run speech.CatchUpRun) {
	defer w.wg.Done()
	ctx := w.endCtx
	if ctx.Err() != nil {
		// The session ended between the flush and the worker's start: the
		// run never speaks, and its terminal (with the covered lines') lands
		// here rather than in silence — the stop's own sweep skips a run
		// that has not reached the scheduler, because the worker owns the
		// terminal of a run still in its TTS call.
		w.finishRun(u, "session_ended")
		return
	}
	rv, why := w.resolveVoiceForCharacter(ctx, u.character)
	if rv.profile.ID == "" {
		w.log("speech: catch-up run voice skip — no approved voice",
			"session_id", w.id, "character_id", u.character,
			"generation", u.generation, "reason", why)
		w.finishRun(u, why)
		return
	}
	w.mu.Lock()
	u.voiceID = rv.profile.ID
	w.mu.Unlock()

	// The merged id form is the key's original-message input (ADR-0122
	// decision 6): the versioned merged message ids ride in the key, and
	// the cache row the generation writes persists them as the row's
	// provenance (the profile purge addresses merged rows through it, via
	// the store's whole-segment match).
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
		w.finishRun(u, fmt.Sprintf("cache_key:%v", err))
		return
	}

	pctx, cancel := context.WithTimeout(ctx, speechTTSTimeout)
	defer cancel()
	synthCtx, synthCancel := context.WithCancel(pctx)
	slot := &synthSlot{cancel: synthCancel}
	w.mu.Lock()
	u.synthSlot = slot
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if u.synthSlot == slot {
			u.synthSlot = nil
		}
		w.mu.Unlock()
		synthCancel()
	}()
	ttsStart := time.Now()
	res, err := w.voices.Speech.GetOrGenerate(speech.WithKeyInput(synthCtx, u.messageID), key, speech.PriorityCurrent, func(ctx context.Context) (tts.Audio, error) {
		return rv.generate(ctx, u.text)
	})
	if err != nil {
		if w.lineRescued(u) {
			// The batch took the run over (ADR-0122 decision 2): the rescue
			// owns the run's terminal, and the worker emits nothing.
			return
		}
		if ctx.Err() != nil {
			w.finishRun(u, "session_ended")
			return
		}
		w.finishRun(u, providerErrorKind(err))
		return
	}
	w.metrics.TTSResolved(w.mLabels, res.FromCache, time.Since(ttsStart))
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
		"message_ids":            run.MessageIDs(),
		"generation":             u.generation,
		"region_id":              u.regionID,
		"cache_key":              string(key),
		"voice_id":               rv.profile.ID,
		"voice_revision":         rv.profile.Revision,
		"from_cache":             res.FromCache,
		"character_id":           u.character,
		"trace_id":               u.traceID,
	})

	if d := w.delay(); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			w.finishRun(u, "session_ended")
			return
		}
	}
	if w.isMuted() {
		w.finishRun(u, "muted")
		return
	}
	wav := res.WAV
	if g := rv.profile.Gain; g != 0 && g != 1 {
		pcm, derr := speech.DecodeWAV(res.WAV)
		if derr != nil {
			w.finishRun(u, "voice_gain:undecodable_wav")
			return
		}
		if g < 0 {
			g = 0
		}
		if g > audio.GainMax {
			g = audio.GainMax
		}
		wav = speech.EncodeWAV(scalePCM(pcm.Data, g, pcm.BitsPerSample),
			pcm.SampleRate, pcm.Channels, pcm.BitsPerSample)
	}
	if w.lineRescued(u) {
		// The batch took the run over after its synthesis: the covered lines
		// speak in the new batch's merged utterances, so this submit never
		// happens and the worker emits no terminal of its own.
		return
	}
	w.sched.Submit(speech.UtteranceReady{
		SessionID:        w.id,
		Generation:       u.generation,
		UtteranceID:      u.occurrence,
		Priority:         speech.PriorityCurrent,
		WAV:              wav,
		ExpectedDuration: res.Duration,
	})
	w.mu.Lock()
	u.submitted = true
	w.mu.Unlock()
}

// finishRun is the merged run's own terminal (the run-level skip or
// failure before the scheduler owns the playback): the run's terminal
// settles the run's generation and the covered lines' — the lines' own
// terminals name the run's outcome (issue #25: every message spoken
// exactly once, and every skipped one named).
func (w *speechWiring) finishRun(u *utteranceInfo, reason string) {
	result := "failed"
	if reason == "muted" {
		result = "muted"
	}
	w.finishEnded(u, result, reason)
	for _, l := range u.mergedLines {
		w.finishLineTerminal(l, result, reason, u.generation)
	}
}

// settleRunLines settles the lines a merged run's terminal covered (issue
// #25: every message spoken exactly once — the line's terminal names the
// run's outcome under the claim the run built, and nothing else settles it).
// The run's generation leaves the in-flight table and the fence's approved
// set with the terminal: it is done, and the approval it kept is no longer
// needed. A generation no run owns (a standalone line's) is a no-op.
func (w *speechWiring) settleRunLines(gen int64, result, eventReason string) {
	w.mu.Lock()
	run, ok := w.catchUpRuns[gen]
	if !ok {
		w.mu.Unlock()
		return
	}
	delete(w.catchUpRuns, gen)
	lines := append([]speech.CatchUpLine(nil), run.mergedLines...)
	w.mu.Unlock()
	w.fence.unapprove(gen)
	reason := eventReason
	switch {
	case result == "ended":
		reason = "catch_up_merged"
	case reason == "":
		reason = "catch_up_run_failed"
	}
	for _, l := range lines {
		w.finishLineTerminal(l, result, reason, gen)
	}
}

// finishLineTerminal settles one covered line's terminal from the outcome
// of the utterance that spoke — or did not speak — it. claimGen is the run
// generation the line's terminal is claimed under (0 for the flush's own
// drops, which claim nothing): a line whose claim is absent or names another
// run keeps its own terminal — the drop or the pause that fenced it, or the
// run it now speaks with — and a line that already settled through an
// earlier source settles nothing twice. The merged events still name the
// line's message, so the coverage reads once, not twice.
func (w *speechWiring) finishLineTerminal(l speech.CatchUpLine, result, reason string, claimGen int64) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	claim, ok := w.lineClaim[l.Occurrence]
	if !ok || claim != claimGen {
		w.mu.Unlock()
		return
	}
	delete(w.lineClaim, l.Occurrence)
	w.mu.Unlock()
	w.finishEnded(w.lineTerminalInfo(l), result, reason)
}

// resolveVoiceForCharacter is resolveVoice's body for a known speaker
// (the catch-up runs resolve their voice from the run's speaker: the merged
// id is not in the speaker table, and the speaker is the decision's input,
// not a lookup).
func (w *speechWiring) resolveVoiceForCharacter(ctx context.Context, character string) (resolvedVoice, string) {
	if rv, why := w.approvedVoice(ctx, character); rv.profile.ID != "" || why != "" {
		return rv, why
	}
	if character == narratorCharacter {
		// The narrator's own line has no fallback question to ask: the
		// speaker and the fallback are the same voice.
		return resolvedVoice{}, "no_approved_voice"
	}
	if w.narratorFallback {
		if rv, why := w.approvedVoice(ctx, narratorCharacter); rv.profile.ID != "" || why != "" {
			return rv, why
		}
		return resolvedVoice{}, "no_approved_voice"
	}
	nr, nwhy := w.approvedVoice(ctx, narratorCharacter)
	switch {
	case nr.profile.ID != "":
		// The narrator could have spoken; the profile opted out. The skip is
		// the setting's, not the voice roster's.
		return resolvedVoice{}, "narrator_fallback_disabled"
	case nwhy != "":
		// The narrator's approved reference is broken: a specific fault is
		// reported on its own terms, not folded into the setting's reason.
		return resolvedVoice{}, nwhy
	default:
		// Even the narrator has no approved voice: the roster is what kept
		// the line silent, on or off the setting.
		return resolvedVoice{}, "no_approved_voice"
	}
}

// resolveVoice finds the speaker character's approved voice and its TTS
// identity: the registered name when the voice is ready on this instance,
// else the approved reference carried inline (docs/spec/06 fallback).
//
// The speaker is the line's assigned character; a line with no assignment
// is the narrator's. A character without an approved voice falls back to
// the narrator voice when one is configured and the profile's narrator
// fallback is on (ADR-0101) — never to a different character's voice, never
// to a builtin. A specific fault on the primary voice (its approved
// reference is missing) is reported, not papered over by the fallback:
// substituting a voice for a broken voice would hide the fault. The skip
// reasons split what kept the line silent: narrator_fallback_disabled is a
// line the setting held back while a narrator voice could speak it, and
// no_approved_voice is a line even the narrator could not speak.
func (w *speechWiring) resolveVoice(ctx context.Context, messageID string) (resolvedVoice, string) {
	return w.resolveVoiceForCharacter(ctx, w.speakerFor(messageID))
}

// ── successor prewarming (FR-11) -----------------------------------------

// prewarm enqueues TTS for the committed line's top weighted successors
// (FR-11, docs/spec/07 "Prewarm top weighted successor messages but cap by
// scenes/byte quota, graph entropy and resource budget"). It runs on the
// session event loop's commit path, so it does exactly one bounded graph
// lookup, the entropy and byte-quota gates, and a bounded number of worker
// spawns — the synthesis itself is the workers' job, at background
// priority.
//
// Cancellation: a new speakable commit moves the fence and redefines
// "likely next", so the previous batch is cancelled at the source (its
// partial stream is discarded and no late audio plays); a scene change does
// the same through the commit watcher. A batch whose successors are too
// uncertain — the weight distribution's entropy above the cap — is skipped:
// prewarming a coin flip would spend the TTS budget on lines that probably
// will not be committed.
func (w *speechWiring) prewarm(messageID string) {
	w.mu.Lock()
	if w.idx == nil || w.closed {
		w.mu.Unlock()
		return
	}
	// The fence just moved to this commit's generation: the previous batch's
	// successors are no longer the likely next, so their in-flight synthesis
	// stops at the source. A scene change does the same through the commit
	// watcher (cancelPrewarmBatch).
	w.cancelPrewarmLocked()

	succs := w.idx.Successors(messageID, w.prewarmCfg.topK)
	if len(succs) == 0 {
		w.mu.Unlock()
		return
	}
	// The entropy gate (docs/spec/07 "graph entropy"): the Shannon entropy of
	// the successor weight distribution, in bits. A confident single
	// successor reads 0; a flat two-way fork ~1; the cap refuses the rest —
	// prewarming a coin flip would spend the TTS budget on lines that
	// probably will not be committed.
	if prewarmEntropy(succs) > w.prewarmCfg.maxEntropyBits {
		w.prewarmSkipped++
		w.mu.Unlock()
		return
	}
	// The byte-quota gate (docs/spec/07 "byte quota"): a commit whose batch
	// would exceed the session's prewarm budget prewarms nothing — the lines
	// then pay the round trip on commit, which is the documented degradation,
	// not a failure.
	if w.prewarmBytes >= w.prewarmCfg.byteQuota {
		w.prewarmSkipped++
		w.mu.Unlock()
		return
	}
	batch, cancel := context.WithCancel(w.endCtx)
	w.prewarmCancel = cancel
	w.mu.Unlock()
	for _, s := range succs {
		go w.prewarmWorker(batch, s.MessageID)
	}
}

// cancelPrewarmBatch cancels the in-flight prewarm batch (fence move, scene
// change). Jobs in flight stop at the source through the batch context; jobs
// that have already committed their audio stay in the cache — a hit later is
// still a hit, and only a key that reaches no commit counts as wasted.
func (w *speechWiring) cancelPrewarmBatch() {
	w.mu.Lock()
	w.cancelPrewarmLocked()
	w.mu.Unlock()
}

// cancelPrewarmLocked cancels and forgets the in-flight batch. Caller holds
// w.mu.
func (w *speechWiring) cancelPrewarmLocked() {
	if w.prewarmCancel != nil {
		w.prewarmCancel()
		w.prewarmCancel = nil
	}
}

// prewarmWorker synthesises one successor's line into the session's shared
// TTS cache, at background priority, under the same key identity a later
// commit of the line would use (same speaker resolution, same canonical
// text, same voice and backend identity). No playback, no speech events, no
// QA: prewarming fills the cache, the commit speaks.
//
// Accounting: the key is booked when the worker starts its generation, and
// prewarmRequested counts that moment — a job the batch cancelled before the
// generation ran booked no budget and counts nothing. On completion the
// committed bytes are booked against the quota and the key stays
// not-yet-consumed until a commit of the line resolves it (a hit) or the
// session ends (wasted). A cancellation that lands after the generation
// finished still commits the audio (it is valid, versioned cache content) —
// a commit that never comes is what makes it wasted.
func (w *speechWiring) prewarmWorker(batch context.Context, messageID string) {
	text := w.prewarmTexts[messageID]
	if text == "" {
		return // no canonical text in the profile: nothing to synthesise
	}
	rv, _ := w.resolveVoice(batch, messageID)
	if rv.profile.ID == "" {
		return // no approved voice for the speaker (and no narrator voice):
		// the same silence-with-reason the commit path takes; the line's own
		// commit reports it when it comes.
	}
	key, err := speech.CacheKeyOf(speech.KeyInput{
		UtteranceText:            text,
		OriginalMessageID:        messageID,
		SpeechRulesRevision:      1,
		Language:                 rv.profile.Language,
		VoiceRevisionFingerprint: rv.profile.Fingerprint,
		TTSModelID:               rv.profile.ModelID,
		BackendConfigDigest:      w.voices.BackendDigest,
		Format:                   "wav/pcm16",
	})
	if err != nil {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	if _, booked := w.prewarmKeys[string(key)]; booked {
		// A still-running or finished prewarm owns this key (an earlier batch
		// of this session, or a converging graph reached it first): one
		// synthesis per key, so this worker yields without double-spending.
		w.mu.Unlock()
		return
	}
	w.prewarmKeys[string(key)] = struct{}{}
	w.prewarmRequested++
	w.mu.Unlock()

	if batch.Err() != nil {
		w.prewarmKeyWasted(key)
		return
	}
	pctx, cancel := context.WithTimeout(batch, w.prewarmCfg.jobDeadline)
	defer cancel()
	res, err := w.voices.Speech.GetOrGenerate(speech.WithKeyInput(pctx, messageID), key, speech.PriorityBackground, func(ctx context.Context) (tts.Audio, error) {
		return rv.generate(ctx, text)
	})
	if err != nil {
		if batch.Err() == nil && !w.closedLoad() {
			// A provider failure the batch did not cause (or the job deadline
			// expired while the batch lived): no bytes were committed, so the key
			// goes back to "unbooked" and a later batch may request it again.
			w.prewarmKeyDrop(key)
			w.log("speech: prewarm generation failed",
				"session_id", w.id, "message_id", messageID, "reason", providerErrorKind(err))
			return
		}
		// Cancelled (fence move, scene change, session end): the job is cut
		// short and its budget is gone.
		w.prewarmKeyWasted(key)
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return // the session ended after the generation: the bytes are cached,
		// but this session's stats do not book them
	}
	w.prewarmBytes += int64(len(res.WAV))
	w.mu.Unlock()
}

// prewarmKeyConsumed records that a committed line resolved one of this
// session's prewarmed keys from the cache (FR-11's "hit"). Returns true when
// the key was this session's prewarm.
func (w *speechWiring) prewarmKeyConsumed(key speech.CacheKey) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.prewarmKeys[string(key)]; !ok {
		return false
	}
	delete(w.prewarmKeys, string(key))
	w.prewarmHit++
	return true
}

// prewarmKeyWasted records one prewarmed key that will not be consumed: the
// job was cut short (fence, scene, session end). The key leaves the
// not-yet-consumed set so the session end does not count it a second time.
func (w *speechWiring) prewarmKeyWasted(key speech.CacheKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.prewarmKeys[string(key)]; ok {
		delete(w.prewarmKeys, string(key))
		w.prewarmWasted++
	}
}

// prewarmKeyDrop releases a booking whose generation failed for a provider
// reason: no bytes were spent, so the session may still prewarm the key
// later (a later batch re-requests it).
func (w *speechWiring) prewarmKeyDrop(key speech.CacheKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.prewarmKeys, string(key))
}

func (w *speechWiring) closedLoad() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// prewarmEntropy is the Shannon entropy, in bits, of the successor weight
// distribution (weights normalised to sum 1). A confident single successor
// reads 0; a flat two-way fork reads 1; the gate refuses anything above the
// configured cap.
func prewarmEntropy(succs []matcher.Successor) float64 {
	total := 0.0
	for _, s := range succs {
		total += s.Weight
	}
	if total <= 0 {
		return 0
	}
	h := 0.0
	for _, s := range succs {
		p := s.Weight / total
		if p > 0 {
			h -= p * math.Log2(p)
		}
	}
	return h
}

// speakerFor is the line's speaker character: the curator's assigned
// character for the message, or the narrator when the line is unattributed.
func (w *speechWiring) speakerFor(messageID string) string {
	if c := w.speakerByMessage[messageID]; c != "" {
		return c
	}
	return narratorCharacter
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
// negotiated identity (registered name, or inline reference). A client that
// carries the streaming seam (ADR-0057) synthesises the registered-voice
// path over the streaming wire — the ctx passed in is the utterance's
// cancellable context, so a fence that moves mid-synthesis stops the read
// and the partial is discarded; a client without it (the test fakes) stays
// on the atomic wire. The reference path stays atomic: the inline
// reference's identity is the GenerateWithReference contract.
type streamingSynthesis interface {
	GenerateStream(ctx context.Context, req tts.GenerateRequest) (tts.Audio, error)
}

func (rv *resolvedVoice) generate(ctx context.Context, text string) (tts.Audio, error) {
	req := tts.GenerateRequest{Text: text, Voice: rv.name, Language: rv.profile.Language}
	if rv.ready {
		if sg, ok := rv.client.(streamingSynthesis); ok {
			return sg.GenerateStream(ctx, req)
		}
		return rv.client.Generate(ctx, req)
	}
	return rv.client.GenerateWithReference(ctx, req, rv.refWAV, rv.refText)
}

// finishEnded emits one terminal speech.ended the wiring owns (skip, mute,
// worker failure) and records the session's audio position. The generation is
// settled: it is terminal, so the fence will not cancel — and re-emit a
// terminal for — it again (issue #6).
func (w *speechWiring) finishEnded(u *utteranceInfo, result, reason string) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.localEnded++
	w.current = &UtteranceState{Phase: result, Result: result, Generation: u.generation, UtteranceID: u.occurrence, At: time.Now().UTC()}
	w.mu.Unlock()
	w.settle(u.generation)
	data := map[string]any{
		"dialogue_occurrence_id": u.occurrence,
		"message_id":             u.messageID,
		"generation":             u.generation,
		"region_id":              u.regionID,
		"result":                 result,
		"character_id":           u.character,
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
		MasterVolume:  w.masterVolume(),
		CatchUp: CatchUpInfo{
			Policy: w.catchUp.Policy,
			CatchUpStats: CatchUpStats{
				Batched:    w.catchUpBatched,
				Dropped:    w.catchUpDropped,
				MergedRuns: w.catchUpMerged,
			},
		},
		Speech: SpeechCounters{
			Requested: w.requested,
			Ready:     w.ready,
			Ended:     w.localEnded,
			CacheHits: w.cacheHits,
		},
		Prewarm: PrewarmStats{
			Requested: w.prewarmRequested,
			Hit:       w.prewarmHit,
			Wasted:    w.prewarmWasted,
			Bytes:     w.prewarmBytes,
			Skipped:   w.prewarmSkipped,
		},
		Current: w.current,
	}
	w.mu.Unlock()
	st := w.sched.Stats()
	info.Speech.Started = st.Started
	info.Speech.Cancelled = st.Cancelled
	info.Speech.Ended += st.Ended + st.Failed
	// The audio counters live on the run's own stats (the wiring books them
	// as the scheduler's events arrive), so the snapshot and the terminal
	// event read the same numbers (ADR-0074).
	if w.runner != nil {
		if rs := w.runner.Stats(); rs.AudioUnderruns > 0 || rs.AudioOutputLatency.Count > 0 {
			info.Underruns = int64(rs.AudioUnderruns)
			info.OutputLatency = rs.AudioOutputLatency
		}
	}
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

// masterVolume reports the appliance's device-level master volume (ADR-0074)
// as this session sees it: the manager's live value, clamped to 0..1 (a
// loader that misbehaves is a wiring bug, and clamping keeps the samples in
// range rather than amplifying them).
func (w *speechWiring) masterVolume() float64 {
	if w.masterLoad == nil {
		return 1.0
	}
	v := w.masterLoad()
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
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
//
// The approved set (issue #25, ADR-0122 decision 4): a catch-up batch's runs
// are submitted under the batch's generation — the newest committed line's
// generation at the flush. A commit that lands while the batch's runs are
// still speaking (they cover the window the player just read) must not fence
// them: the commit batched the new line instead of superseding them, so the
// batch's generation stays approved until its runs settle. The set is
// bounded (DefaultCatchUpApprovedCap): a live batch holds a handful of
// generations at most, and the cap evicts the oldest on overflow.
type generationFence struct {
	mu     sync.Mutex
	latest int64
	// approved is the catch-up set of approved batch generations, in
	// approval order (the cap evicts from the front).
	approved []int64
}

func (f *generationFence) set(g int64) {
	f.mu.Lock()
	f.latest = g
	f.mu.Unlock()
}

// approve adds one catch-up batch generation to the approved set (the cap
// bounds it, evicting the oldest on overflow). A generation already approved
// is not doubled.
func (f *generationFence) approve(g int64, cap int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.approved {
		if a == g {
			return
		}
	}
	f.approved = append(f.approved, g)
	if cap > 0 && len(f.approved) > cap {
		f.approved = f.approved[1:]
	}
}

// unapprove removes one approved generation (its runs settled, or the
// session's pause/stop took the batch down).
func (f *generationFence) unapprove(g int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.approved[:0]
	for _, a := range f.approved {
		if a != g {
			out = append(out, a)
		}
	}
	f.approved = out
}

// clearApproved empties the approved set (pause and stop: the batch the
// session is not speaking must not keep its generations speakable).
func (f *generationFence) clearApproved() {
	f.mu.Lock()
	f.approved = nil
	f.mu.Unlock()
}

// IsCurrent implements speech.Fence.
func (f *generationFence) IsCurrent(g int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.latest == g {
		return true
	}
	for _, a := range f.approved {
		if a == g {
			return true
		}
	}
	return false
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
	switch e.Name {
	case events.MessageCommitted:
		p.w.onCommit(e)
	case events.SessionSceneChanged:
		// A scene change invalidates the in-flight prewarm batch (docs/spec/07
		// "...capped by scenes"): the committed line's successors were computed
		// for the scene the player just left, so their audio is now at best
		// useless. The fence itself does not move — a scene change is not a
		// textbox advance — so only the background prewarming dies here.
		p.w.cancelPrewarmBatch()
	case events.SessionPaused:
		// The pause control's speech half (docs/spec/08 "…/pause"): the event
		// arrives on the same chain, in the order the single writer applied
		// it — after the reducer stopped committing and cancelled the
		// in-flight recognitions — so by the time the audio fences, the text
		// side is already fenced. The event is the handoff, not a second
		// control: a pause that reached the writer is the pause the audio
		// takes.
		p.w.pause()
	case events.SessionSettings:
		// The settings control's audio half (issue #25, ADR-0122): the catch-up
		// narration policy moves live mid-session on the same control that moves
		// the mode and the narrated classes. The payload carries the applied word
		// (the session package's vocabulary; the runtime cross-checks it — the
		// two spellings are the same two words); a payload without one leaves the
		// policy as it is.
		var payload struct {
			CatchUp string `json:"catch_up"`
		}
		_ = json.Unmarshal(e.Data, &payload)
		if payload.CatchUp != "" {
			p.w.setCatchUp(payload.CatchUp)
		}
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
	reason, _ := data["reason"].(string)
	genI := int64(gen)
	// The status bar's voice half (status.go): the scheduler has accepted
	// the playback on the sink, so the line's commit→voice sample completes
	// now. Only a start measures — a cancel or an end is the line's
	// outcome, not its arrival.
	if e.Name == events.SpeechStarted && w.voiceLat != nil {
		w.voiceLat.noteVoiceStart(occ, time.Now().UTC())
	}
	// The frame→voice half of docs/spec/13 "Metrics" ("first audible audio"): the
	// pipeline's reduce loop booked this line's frame-entry anchor when it
	// committed; the audio has started, so the interval completes now. A
	// line without an anchor (telemetry unwired, or a table reset) books
	// nothing — the same honest shape the status sampler takes.
	if e.Name == events.SpeechStarted && w.runner != nil {
		w.runner.ObserveFrameToVoice(occ)
	}
	w.mu.Lock()
	if genI > 0 {
		// The session's active playback (issue #25, ADR-0122 decision 2):
		// the commit path leaves the playing generation to run out, and the
		// playback's terminal is the batch's flush trigger ("when playback
		// ends") — so the start and the terminal move the marker, and a
		// terminal of another generation leaves it (the playing line keeps
		// playing under the batched newer one).
		switch e.Name {
		case events.SpeechStarted:
			w.playingGen = genI
		case events.SpeechEnded, events.SpeechCancelled:
			if w.playingGen == genI {
				w.playingGen = 0
				// The approval the commit path kept the playing generation
				// current with is done with: the playback is over, and a
				// generation the fence no longer approves is one the next
				// commit's cancel list may take (as it may, the line has run
				// out — its terminal is out or is being settled below).
				w.fence.unapprove(genI)
			}
		}
	}
	w.current = &UtteranceState{
		Phase:       phase,
		Result:      result,
		Generation:  genI,
		UtteranceID: occ,
		At:          time.Now().UTC(),
	}
	flushNow := e.Name != events.SpeechStarted && len(w.catchUpPending) > 0 && w.catchUp.Policy == speech.CatchUpPolicyCatchUp
	w.mu.Unlock()
	// A scheduler terminal (ended or cancelled) is the generation's final
	// device-level result: settle it so the fence never cancels — and
	// re-emits a terminal for — it again (issue #6; terminal results are
	// idempotent). A start is not a terminal: a started generation is still
	// cancellable by a newer speakable commit (the stale guarantee). A
	// merged run's terminal also settles the lines it spoke — under the
	// claim the run built, so a late run settles nothing else's lines.
	if genI > 0 && e.Name != events.SpeechStarted {
		w.settleRunLines(genI, result, reason)
		w.settle(genI)
		if flushNow {
			// The issue's second flush trigger: the playback just ended and
			// the batch holds lines committed in the meantime — they speak
			// now, back-to-back, instead of at the window's end.
			w.flushCatchUp("playback_ended")
		}
	}
	if e.Name != events.SpeechEnded {
		return
	}
	// The audio counters of docs/spec/13 (ADR-0074): an ended event that
	// names a started playback (started_at present) and a failed result is a
	// playback underrun — samples lost mid-utterance. A failed start (no
	// started_at) is not one. Output latency lands with the same event when
	// the sink stamped its first sample.
	if started, ok := data["started_at"].(string); ok && started != "" && result == "failed" {
		w.noteAudioUnderrun()
	}
	if lat, ok := data["output_latency_ms"].(float64); ok && lat > 0 {
		w.noteAudioOutputLatency(time.Duration(lat * float64(time.Millisecond)))
	}
}

// noteAudioUnderrun books one underrun on the run's own stats (nil-safe: a
// wiring built without a runner — no pipeline in the test — skips the
// booking rather than panicking). It is also one sample of the playback
// stage on the telemetry surface (docs/spec/13 "Metrics") — the underrun
// counter the spec names, booked under the run's labels; a wiring with no
// catalog books only the stats.
func (w *speechWiring) noteAudioUnderrun() {
	if w.runner != nil {
		w.runner.ObserveAudioUnderrun()
	}
	w.metrics.PlaybackUnderrun(w.mLabels)
}

// noteAudioOutputLatency books one output-latency sample (nil-safe).
func (w *speechWiring) noteAudioOutputLatency(d time.Duration) {
	if w.runner != nil {
		w.runner.ObserveAudioOutputLatency(d)
	}
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

// masterSink is the device-level master-volume wrapper (ADR-0074): it scales
// the utterance samples by the appliance's one output level, read live from
// the loader at every playback, so a Settings-page change applies from the
// next utterance without rebuilding the session's sink. It sits outside the
// session's gainSink, and the two multiply — the session's live volume is
// the operator's per-session control, the master volume is the appliance's
// calibration of the physical output.
type masterSink struct {
	inner speech.AudioSink
	// load returns the current master volume (0..1).
	load func() float64
}

func newMasterSink(inner speech.AudioSink, load func() float64) *masterSink {
	if load == nil {
		load = func() float64 { return 1.0 }
	}
	return &masterSink{inner: inner, load: load}
}

// Play implements speech.AudioSink.
func (s *masterSink) Play(ctx context.Context, req speech.PlayRequest) (speech.PlayHandle, error) {
	g := s.load()
	if g < 0 {
		g = 0
	}
	if g > 1 {
		g = 1
	}
	if g != 0 && g != 1 {
		pcm, err := speech.DecodeWAV(req.WAV)
		if err != nil {
			return nil, fmt.Errorf("runtime: scaling master volume: %w", err)
		}
		req.WAV = speech.EncodeWAV(scalePCM(pcm.Data, g, pcm.BitsPerSample),
			pcm.SampleRate, pcm.Channels, pcm.BitsPerSample)
	}
	return s.inner.Play(ctx, req)
}

// Close implements speech.AudioSink.
func (s *masterSink) Close() error { return s.inner.Close() }

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
