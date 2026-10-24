package runtime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
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

// SpeakerConfidenceFloor is the adjudicator's speaker-answer confidence the
// runtime treats as naming a speaker (ADR-0156): the chosen speaker option's
// probability must reach this before the answer is allowed to pick a line's
// voice. It is the same half-confidence the safe_to_speak gate already applies
// to the model's utterance permission, so the two "sure" judgements read at
// one threshold.
const SpeakerConfidenceFloor = 0.5

// kindSpeakerKeyPrefix is the opaque roster alias prefix the adjudicator's
// apparent-kind answers resolve through (ADR-0158): a kind the adjudicator
// reads off the frame ("adult_male") becomes the opaque roster key
// "kind:adult_male", which a character row's aliases may carry — the generic
// voice for an unnamed speaker of that apparent kind. The prefix is the same
// one the Wind Waker's opaque speaker keys use shape-wise ("ww:spk:<n>", ADR-0070)
// but names a different axis: identity vs apparent kind.
const kindSpeakerKeyPrefix = "kind:"

// speechReasonUnmatched is the speech.requested reason of the unmatched
// read-through line (ADR-0136): the trace's answer to "why did a line the
// corpus cannot name get audio?".
const speechReasonUnmatched = "unmatched_read_through"

// unmatchedMinConfidence is the OCR confidence floor the unmatched
// read-through requires (ADR-0136): the box is already SETTLED (the
// commitment policy only commits a READY box), so this is the *text* half of
// "confident and stable" — the tracker's mean read confidence, in the
// [0,1] scale the tracker's MinConfidence default (0.55, recognition's
// documented default) is measured on. A matched line speaks on the
// commitment, never on this number; the read-through speaks OCR text with
// no script to vouch for it, so the floor is the tracker's own readiness
// floor raised — a wobbly read the matcher already refused is not a line to
// improvise aloud. Measured against the live capture's clean top-box reads
// (the ADR-0137 fixture, where the full line reads at ~0.9+ confidence),
// 0.75 is a read that is plain as day, not a guess.
const unmatchedMinConfidence = 0.75

// unmatchedMaxRunes bounds the TTS text length of a read-through line
// (docs/spec/14 "Bound … TTS text length"): the text is untrusted game
// content (trust zone C), and a hostile or noisy read must not become a
// long synthesis. A dialogue box shows at most a few lines; beyond the
// bound the text is cut at a word boundary, which is the degradation —
// the line still speaks, at the bound.
const unmatchedMaxRunes = 400

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
	DefaultPrewarmTopK = 5
	// DefaultPrewarmMaxEntropyBits is the Shannon entropy (bits) of the
	// successor weight distribution above which the batch is skipped: with
	// two near-equal successors the entropy is ~1.0 bit, so the default
	// prewarms a confident next line and forks up to a flat four-way
	// choice (2.0 bits), but not a flat five-way coin flip (~2.32 bits).
	DefaultPrewarmMaxEntropyBits = 2.0
	// DefaultPrewarmByteQuota bounds the bytes of prewarmed audio one
	// session may commit. 8 MiB is ~4 minutes of 16 kHz/16-bit mono speech:
	// the prewarm stays ahead of the player for the rest of the scene
	// without filling the box's storage budget.
	DefaultPrewarmByteQuota = 8 << 20
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
	// policy (issue #25, ADR-0122, amended by ADR-0175 amendment 1): the
	// catch_up/latest_only policy, the batch window and the backlog cap.
	// nil takes the documented
	// defaults (catch_up, 900 ms window, the 45 s backlog cap).
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
// (issue #25, ADR-0122; the backlog cap is the ADR-0175 amendment): what the
// policy batched, spoke and dropped — the "did it speak, and why not"
// reading for the catch-up decisions, in the same surface as the speech
// counters.
type CatchUpStats struct {
	// Batched: lines a new commit took into the batch (the rescue) or the
	// line itself joined the batch on a busy commit.
	Batched int64 `json:"batched"`
	// Dropped: lines the batch dropped (the pause and the stop, the
	// session's own decisions).
	Dropped int64 `json:"dropped"`
	// MergedRuns: catch-up utterances the batch spoke (one per line since the
	// ADR-0175 amendment: each page's own cached clip, back-to-back).
	MergedRuns int64 `json:"merged_runs"`
	// BacklogCapped: the oldest pages the backlog cap dropped (reason
	// catch_up_backlog_cap) — the safety bound that keeps the narrator from
	// lagging unboundedly behind a sustained mash (ADR-0175 amendment).
	BacklogCapped int64 `json:"backlog_capped"`
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
	// speakerSlotByMessage maps a committed message id onto whether its
	// speaker slot is shared (ADR-0156): the slot takes more than one speaker,
	// so the curator's slot→character link is a claim about a slot, not a
	// per-occurrence identity, and a shared slot's line must not take its
	// voice from the link alone. An unset message reads as not shared, which
	// is the pre-ADR-0156 shape (a slot that maps to a single character keeps
	// its assignment).
	speakerSlotByMessage map[string]bool
	// speakerAliasByID maps a character row id to its display name so
	// approvedVoice resolves voices keyed by character name (Cast UI /
	// auto-cast, ADR-0109/ADR-0135) as well as character ID (ADR-0101).
	speakerAliasByID map[string]string
	// kindSpeakerByKey maps an opaque apparent-kind key (the adjudicator's
	// kind answer carried through the roster's "kind:<kind>" prefix, ADR-0158)
	// onto the character whose aliases include it — the generic voice for an
	// unnamed speaker of that apparent kind. Empty when the profile carries no
	// such generic kind voices. A nil map answers no kind, which is the
	// narrator fallback.
	kindSpeakerByKey map[string]string

	// narratorFallback is the profile's narrator fallback (ADR-0101): a line
	// whose speaker has no approved voice is spoken by the narrator voice
	// when one is configured and only when this flag is on. The profile row
	// defaults it on, the runtime's historical behaviour, and a session reads
	// it once at start. A skip's reason says when the setting — not just the
	// absence of voices — is what kept the line silent.
	narratorFallback bool

	// The ADR-0167 speaker-at-commit ask: the decision wiring this session's
	// committed shared-slot lines ask (the ADR-0160 redesign's successor — the
	// ask fired at commit time instead of at the first read of a typing box),
	// plus the bounded wait the utterance holds while the answer is in flight
	// (the ADR-0167 budget, the ADR-0160 proposal's 300 ms). The runtime's
	// speech side owns the wait; the decision path's ask goroutine feeds it.
	// Nil-safe without a decision path wired.
	decision *decisionWiring
	// askMu guards askPending / askBudget / askTimes: the registry the routing
	// workers read the bounded wait from, the budget the wait is bounded by,
	// and the ask timestamps the per-minute cap counts.
	askMu      sync.Mutex
	askPending map[string]*askWait
	askBudget  time.Duration
	askTimes   []time.Time
	// speakerAskBudget overrides DefaultSpeakerAskBudget when positive (tests
	// under the race detector widen it so a fake gateway's answer lands).
	speakerAskBudget time.Duration

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
	// Display pages (ADR-0155): a committed message's pages in ordinal order,
	// page texts only (the display line break folded to the speech column's
	// space, so a page's text is the text that box would speak). Populated
	// from the published content's page rows when the profile carries them;
	// when it carries none, the fallback split of the canonical text fills the
	// map (the profiles imported before paging was derived). An absent entry
	// means the message has no pages to speak (a single-box message, or one the
	// split found empty).
	prewarmPages map[string][]string
	// recentRouted is the cross-region duplicate guard (#41): line identity
	// (message id, or "rt:"+the cleaned read-through text) → the region and
	// wall clock it was last routed from. A profile that declares the box at
	// more than one on-screen position can see the same box from two
	// overlapping regions, and both commit the same line ~100-200 ms apart;
	// routing both spoke the line twice (merged into one catch-up run) and
	// pushed the next line behind it. w.mu.
	recentRouted map[string]routedMark
	// lastRoutedKey is the identity key (pageKey, "rt:"+text) of the most
	// recent routing, "" before any routing. The cross-region duplicate
	// guard (ADR-0186) only suppresses a same-page commit from another region
	// while no different line routed in between — the box is still the same
	// one; once a different line spoke, the other region's same page is a
	// repeat line, not a double-read. w.mu.
	lastRoutedKey string
	// recentSpoken is a short ring of the matched lines routed to speech
	// (text + when), the reference the read-through echo check compares a
	// garbled re-read against. Guarded by mu.
	recentSpoken []spokenMark
	// voiceLow caches, per assigned character, whether its approved voice is
	// a low (adult male) voice — measured once from the reference WAV
	// (warmVoiceClasses). An absent entry is unknown. Guarded by mu.
	voiceLow   map[string]bool
	prewarmCfg prewarmConfig
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
	// the backlog cap drops its oldest prefix at each newer line. The batcher
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
	// runGenReserve is the next bookkeeping generation the catch-up runs may
	// take (the ADR-0175 amendment's per-page runs are many, and booking the
	// batch's newest commit generation plus the run's index collided with a
	// commit that landed mid-flush: run and commit shared one generation, and
	// the run's line terminal was lost on the collision). A flush books the
	// runs from this pool and advances it, and every batched commit bumps it
	// past its own generation, so a run generation never is a commit
	// generation and no two flushes book the same run generation.
	runGenReserve int64
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
	// catchUpBacklogDropped is the backlog cap's counter (ADR-0175
	// amendment): the oldest pending pages dropped when the batch's
	// estimated audio ran past the cap (reason catch_up_backlog_cap).
	catchUpBacklogDropped int64

	// unmatchedReadThrough is the session's unmatched read-through policy
	// (ADR-0136): when the box commits as a line the corpus cannot name (the
	// matcher abstained and nothing adjudicated into a candidate), the
	// speaker speaks the OCR text (cleaned and bounded) instead of staying
	// silent. Default ON (a construction with no settings call is the
	// documented default); the settings route moves it mid-session.
	// w.mu.
	unmatchedReadThrough bool

	// The session's learned player name (ADR-0165, #48): the name a "{player}"
	// slot speaks under — trusted once two agreeing OCR reads of the slot
	// have voted for one candidate, and "" while the session has not earned
	// one, in which case the slot's line speaks the dropped placeholder form.
	// w.mu.
	playerName string
	// The player-name slot's per-session votes (ADR-0165): candidate name →
	// agreeing-read count, while the session has not yet trusted a name.
	// w.mu.
	playerVotes map[string]int
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
	// voiceRoute is how this utterance's voice was resolved (ADR-0158): the
	// adjudicator's confident speaker ("speaker"), the confident apparent-kind
	// routed through the roster's generic kind character ("speaker_kind"), the
	// neutral narrator fallback ("narrator"), or the roster's own assignment
	// ("assigned"). It rides the speech.ready the worker emits, so the dev UI
	// and the A/V harness can tell a kind-routed line from a narrator line.
	voiceRoute string
	voiceID    string // the voice profile spoken ("" until resolved)
	// speakerAskKey is the ADR-0167 speaker-at-commit ask's occurrence key —
	// the box (region, generation) plus the committed message — when a shared-
	// slot commit owed a speaker-at-commit ask for this utterance, "" when it
	// did not (no decision path wired, not a shared slot, already attributed,
	// over the ask cap). Set by onCommit before routing; the routing worker
	// reads it to wait for the answer before resolving the voice.
	speakerAskKey string
	// committedAt is the wall clock the line reached the speech side
	// (issue #25, ADR-0122): the flush timer's window and the backlog cap's
	// ordering measure from it.
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
	// reason names the speech side's verdict for the line (ADR-0136): "" is
	// the matched line the commitment policy committed, and
	// speechReasonUnmatched is the unmatched read-through — the line the
	// matcher could not name, spoken from the OCR text under the session's
	// policy. It rides on the line's speech.requested so the trace shows why
	// a line without a message id was spoken.
	reason string
	// confidence is the line's OCR confidence for the read-through's
	// record (0 for a matched line: its evidence is the commitment, not the
	// OCR's number).
	confidence float64
}

// synthSlot is one in-flight synthesis's cancellable scope, registered
// against the utterance's generation: the fence cancels it on a new commit
// and the worker deregisters it when the synthesis settles. A pointer, so
// the fence and the worker compare the registration by identity.
type synthSlot struct {
	cancel context.CancelFunc
}

// DefaultSpeakerAskBudget bounds how long a committed shared-slot line may
// hold its voice resolution for the ADR-0167 speaker-at-commit answer before
// the commit proceeds on the local evidence alone and the line speaks the
// assigned voice (the ADR-0160 proposal's 300 ms). A session may tighten or
// widen it through the ask's own options; zero takes this default.
const DefaultSpeakerAskBudget = 300 * time.Millisecond

// speakerAskWait is the ADR-0167 budget this wiring bounds a commit-time ask by.
func (w *speechWiring) speakerAskWait() time.Duration {
	if w.speakerAskBudget > 0 {
		return w.speakerAskBudget
	}
	return DefaultSpeakerAskBudget
}

// maxSpeakerAsksPerMinute caps the speaker-at-commit asks per session per
// minute: the global ask rate the ADR-0167 design bounds (the issue's "cap it
// at a few requests per minute"), so a session that keeps committing shared-
// slot lines cannot spend the decision budget's worth of GPU round trips a
// minute.
const maxSpeakerAsksPerMinute = 20

// askKey is the ADR-0167 speaker-at-commit occurrence key — the box (region,
// generation) plus the committed message the occurrence's line speaks — the
// runtime's ask registry and the routing worker hold the bounded wait under.
func askKey(region string, gen int64, messageID string) string {
	return region + "\x00" + strconv.FormatInt(gen, 10) + "\x00" + messageID
}

// askWait is one committed occurrence's in-flight speaker-at-commit ask
// (ADR-0167): the bounded wait for the adjudicator's speaker answer while the
// commit-time ask is in flight, closed when the answer lands or the ask gives
// up — so a wait is bounded and never blocks forever.
type askWait struct {
	done chan struct{}
	// attribution holds the answer that closed the wait, written under the
	// registry's lock before done is closed (the mutex orders the write before
	// the close, the same way it orders every other registry step).
	speakerID       string
	speakerKnown    bool
	speakerConf     float64
	speakerKind     string
	speakerKindConf float64
}

// attributionAsk is the speaker-at-commit answer's attribution half as the
// speech side reads it back: the speaker the adjudicator named and the
// apparent kind it read, with the confidences the runtime's gates read
// (ADR-0156, ADR-0158).
type attributionAsk struct {
	speakerID       string
	speakerKnown    bool
	speakerConf     float64
	speakerKind     string
	speakerKindConf float64
}

// beginAsk registers one committed occurrence's speaker-at-commit ask wait
// (ADR-0167) and returns its handle; the runtime's speech side owns the wait,
// and the decision path's ask goroutine feeds it. The wait is bounded by the
// budget the caller passes (the ADR-0167 budget), and a nil handle means no
// ask is owed for the key (not a shared slot, already attributed, over the
// ask cap, or no decision path wired). At most one ask per occurrence: the
// key's pending is the occurrence's one wait, and the cap bounds how many
// occurrences a session may owe an ask in a minute.
func (w *speechWiring) beginAsk(key string, budget time.Duration) *askWait {
	w.askMu.Lock()
	defer w.askMu.Unlock()
	if w.askPending == nil {
		w.askPending = map[string]*askWait{}
	}
	if _, owed := w.askPending[key]; owed {
		return nil // at most one ask per occurrence: the key already owes one
	}
	now := time.Now()
	// The ask rate cap: a minute sliding window of the asks sent, so a burst
	// of committed shared-slot lines spends the decision budget's worth of
	// round trips at most once a minute.
	kept := w.askTimes[:0]
	for _, t := range w.askTimes {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	w.askTimes = kept
	if len(w.askTimes) >= maxSpeakerAsksPerMinute {
		return nil
	}
	w.askTimes = append(w.askTimes, now)
	if budget > 0 {
		w.askBudget = budget
	}
	p := &askWait{done: make(chan struct{})}
	w.askPending[key] = p
	return p
}

// resolveAsk closes the occurrence's speaker-at-commit wait with the answer
// the ask produced and returns it. res is the answer the decision side read
// back from the adjudication's reply (the attribution half); the pending's own
// stored fields are what a wait that observed the close reads. The caller that
// resolves with no answer (the ask gave up, or the session stopped) passes the
// zero attribution, so a bound wait falls back to the local evidence.
func (w *speechWiring) resolveAsk(key string, res attributionAsk) attributionAsk {
	w.askMu.Lock()
	defer w.askMu.Unlock()
	out := attributionAsk{}
	if p, ok := w.askPending[key]; ok && p != nil {
		out = res
		p.speakerID = res.speakerID
		p.speakerKnown = res.speakerKnown
		p.speakerConf = res.speakerConf
		p.speakerKind = res.speakerKind
		p.speakerKindConf = res.speakerKindConf
		delete(w.askPending, key)
		close(p.done)
	}
	return out
}

// waitAsk waits up to the speaker-at-commit budget for the answer to "who is
// speaking" of one committed occurrence (ADR-0167). It reports whether an ask
// was in flight for the key and whether the answer resolved inside the budget; a
// wait that outlives the budget — or the run — resolves empty, so the commit
// proceeds on the local evidence alone and the line speaks the assigned voice,
// as today, without stalling.
func (w *speechWiring) waitAsk(ctx context.Context, key string, budget time.Duration) (attributionAsk, bool) {
	w.askMu.Lock()
	p, ok := w.askPending[key]
	b := w.askBudget
	w.askMu.Unlock()
	if !ok || p == nil {
		return attributionAsk{}, false
	}
	if budget > 0 && budget < b {
		b = budget
	}
	if b <= 0 {
		b = DefaultSpeakerAskBudget
	}
	select {
	case <-p.done:
		// The answer closed the wait: the fields were written under the
		// registry's lock before the close, and receiving from done is
		// ordered after that write, so reading them here needs no lock.
		return attributionAsk{
			speakerID:       p.speakerID,
			speakerKnown:    p.speakerKnown,
			speakerConf:     p.speakerConf,
			speakerKind:     p.speakerKind,
			speakerKindConf: p.speakerKindConf,
		}, true
	case <-ctx.Done():
		w.resolveAsk(key, attributionAsk{})
		return attributionAsk{}, false
	case <-time.After(b):
		w.resolveAsk(key, attributionAsk{})
		return attributionAsk{}, false
	}
}

// hasConfidentSpeaker reports whether one committed occurrence's commit
// payload already carries a confident adjudicator speaker answer — the gate the
// ADR-0167 ask is not owed for (ADR-0156): a speaker named at or above the floor
// is a name, and the line's voice is already the adjudicator's.
func (w *speechWiring) hasConfidentSpeaker(d commitPayload) bool {
	return d.SpeakerKnown && d.SpeakerID != "" && d.SpeakerConfidence >= SpeakerConfidenceFloor
}

// speakerAskOptions narrows the roster to the speaker question's option cap
// (systemone.MaxOptions-1 declared speakers plus "unknown"): Clef rejects a
// choice question with more options as bad_schema, and the real Wind Waker
// roster has ~80 characters, so an uncapped ask failed on every board commit.
// The line's assigned character (the shared slot's link) goes first so the
// answer can confirm or overrule it; the rest keep roster order.
func speakerAskOptions(roster []systemone.Speaker, assigned string, limit int) []systemone.Speaker {
	if limit <= 0 || limit > systemone.MaxOptions-1 {
		limit = systemone.MaxOptions - 1
	}
	out := make([]systemone.Speaker, 0, limit)
	if assigned != "" {
		for _, s := range roster {
			if s.ID == assigned {
				out = append(out, s)
				break
			}
		}
	}
	for _, s := range roster {
		if len(out) >= limit {
			break
		}
		if s.ID == "" || s.ID == assigned {
			continue
		}
		out = append(out, s)
	}
	return out
}

// askSpeakerFor runs one speaker-at-commit ask (ADR-0167) for one committed
// shared-slot occurrence: it asks the decision role through the decision
// wiring's own client — the speaker + speaker_kind questions only, the frame
// included — and when the answer lands (or the ask gives up) resolves the
// occurrence's bounded wait, so the routing worker that waits on it sees the
// attribution, or the fallback when the answer never came inside the budget.
// The ask itself is bounded by the decision wiring's own budget; the wait the
// utterance holds is bounded separately (the ADR-0167 budget), and the
// session stop cancels the ask through the run context.
func (w *speechWiring) askSpeakerFor(d commitPayload, key string) {
	ctx := context.Background()
	if w.endCtx != nil {
		ctx = w.endCtx
	}
	frame := image.Image(nil)
	if w.runner != nil {
		if f, ok := w.runner.LatestFrame(); ok {
			frame = f.Image
		}
	}
	var res attributionAsk
	if w.decision != nil {
		opts := speakerAskOptions(w.decision.speakers, w.speakerByMessage[d.MessageID], w.decision.opts.SpeakerOptions)
		speakerID, kind, conf, kindConf, err := w.decision.AskSpeaker(ctx, frame, d.MessageID, opts)
		if err == nil {
			res = attributionAsk{
				speakerID:       speakerID,
				speakerKnown:    speakerID != "",
				speakerKind:     kind,
				speakerConf:     conf,
				speakerKindConf: kindConf,
			}
		}
	}
	w.resolveAsk(key, res)
}

// newSpeechWiring builds the per-session speech pipeline. It returns nil
// (with a warning already logged) when the sink cannot be built — the
// documented degradation: the session runs silent rather than failing the
// run because audio output is misconfigured.
func (m *Manager) newSpeechWiring(id string, speakerByMessage map[string]string, narratorFallback bool, runCtx context.Context, runner *pipeline.Runner, idx *matcher.Index, prewarmTexts map[string]string, prewarmPages map[string][]string, voiceLat *frameVoiceSampler) *speechWiring {
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
		// The unmatched read-through starts ON (ADR-0136's documented
		// default): a session without a settings call speaks an unmatched
		// box from its OCR text, and the Play toggle is what moves it.
		unmatchedReadThrough: true,
		sem:                  make(chan struct{}, speechWorkerSlots),
		utterances:           map[string]*utteranceInfo{},
		byGeneration:         map[int64]string{},
		genCancel:            map[int64]*synthSlot{},
		settled:              map[int64]bool{},
		catchUpRuns:          map[int64]*utteranceInfo{},
		lineClaim:            map[string]int64{},
		recentRouted:         map[string]routedMark{},
		volume:               1.0,
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
	w.initPrewarm(idx, prewarmTexts, prewarmPages, opts.Prewarm)
	w.playerVotes = map[string]int{}
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
func (w *speechWiring) initPrewarm(idx *matcher.Index, texts map[string]string, pages map[string][]string, opts *PrewarmOptions) {
	w.idx = idx
	w.prewarmTexts = texts
	w.prewarmPages = pages
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
			if _, isRun := w.catchUpRuns[g]; isRun {
				// A catch-up run's bookkeeping generation: its fate is the
				// batch's (the rescue takes it over, the approval keeps it
				// speakable), never the fence's cancel — cancelling it here
				// would drop the pages the amendment must speak.
				continue
			}
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

// playerPlaceholder is textnorm's placeholder for a configured player-name
// spelling: the matcher folds the names onto it before indexing, and the
// runtime's speech text carries it to the TTS stage, which resolves it onto
// the name the player actually speaks (ADR-0165, #48). The literal lives in
// textnorm so every layer spells one thing.
const playerPlaceholder = textnorm.Placeholder

// playerNameVotesNeeded is how many agreeing reads of one player-name
// candidate trust it for the session (ADR-0165): one OCR read is a guess, two
// agreeing reads of the same slot are the name.
const playerNameVotesNeeded = 2

// playerNameMinLen is the shortest plausible spoken player name (ADR-0165):
// below two letters it is address, not a name.
const playerNameMinLen = 2

// playerNameMaxLen is the longest plausible spoken player name (ADR-0165).
const playerNameMaxLen = 12

// resolvePlayerPlaceholder resolves one text the narrator speaks onto the
// player's name (ADR-0165, #48): with a name it substitutes the "{player}"
// slot; without one it drops the slot and tidies the punctuation the drop
// orphans — "Hurry, {player}!" → "Hurry!", "{player}, wait." → "Wait.",
// "It's you, {player}." → "It's you.". The caller resolves with the
// session's learned name; the empty answer is the dropped form.
func resolvePlayerPlaceholder(text, name string) string {
	if !strings.Contains(text, playerPlaceholder) {
		return text
	}
	if name != "" {
		return strings.ReplaceAll(text, playerPlaceholder, name)
	}
	return dropPlayerPlaceholder(text)
}

// dropPlayerPlaceholder removes the player-name slot and tidies the
// punctuation the removal orphans: the vocative comma the address wore goes
// with the slot, a sentence terminator the slot carried keeps its place, and
// a slot that opened the sentence leaves the sentence to speak itself.
func dropPlayerPlaceholder(text string) string {
	s := strings.TrimSpace(strings.ReplaceAll(text, playerPlaceholder, ""))
	s = strings.Join(strings.Fields(s), " ")
	s = tidyPlayerPunctuation(s)
	s = strings.TrimLeft(s, ",;:!?")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if unicode.IsLower(r[0]) {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

// tidyPlayerPunctuation drops the spacing and commas the placeholder drop
// orphaned: a sentence terminator never keeps the separator the removed slot
// left in front of it ("Hurry, {player}!" → "Hurry!"), and the vocative comma
// the address wore in front of the slot goes with the slot itself
// ("It's you, {player}." → "It's you.").
func tidyPlayerPunctuation(s string) string {
	for _, t := range []string{".", "!", "?", "…"} {
		s = strings.ReplaceAll(s, " "+t, t)
		s = strings.ReplaceAll(s, ","+t, t)
	}
	return strings.TrimSpace(s)
}

// playerNameCandidate is the OCR read's word for one player-name slot: the
// slot's position in the script text counts the words before it, and the
// spoken name is the read's word at that index.
func playerNameCandidate(canon, read string) string {
	slot := -1
	wordsBefore := 0
	for _, f := range strings.Fields(canon) {
		if strings.Contains(f, playerPlaceholder) {
			slot = wordsBefore
			break
		}
		wordsBefore++
	}
	if slot < 0 {
		return ""
	}
	var words []string
	for _, t := range strings.Fields(read) {
		if core := wordCore(t); core != nil {
			words = append(words, string(core))
		}
	}
	if slot >= len(words) {
		return ""
	}
	return words[slot]
}

// validPlayerName reports whether s is a plausible spoken player name: two
// to twelve letters, capitalised in the read.
func validPlayerName(s string) bool {
	r := []rune(s)
	if len(r) < playerNameMinLen || len(r) > playerNameMaxLen {
		return false
	}
	return unicode.IsUpper(r[0])
}

// onCommit is the commit watcher's handoff: it runs on the pipeline's
// reducer goroutine and must stay fast — the fence, the cancel and the event
// are the synchronous half; the TTS work is the worker's.
// commitPayload is the message.committed data the speech side reads: the
// commit's correlation ids, the eligibility verdict the read-through gates
// on (ADR-0136), the OCR text and confidence it may speak, and the speaker
// attribution it may voice. Every field is optional on the wire in the
// honest sense — a payload without one keeps the zero value, and the gates
// below treat the zero value as "no", never as "yes".
type commitPayload struct {
	Occurrence   string  `json:"dialogue_occurrence_id"`
	MessageID    string  `json:"message_id"`
	RegionID     string  `json:"region_id"`
	Generation   int64   `json:"generation"`
	Text         string  `json:"text"`
	RawText      string  `json:"raw_text"`
	Eligible     bool    `json:"speech_eligible"`
	Reason       string  `json:"reason"`
	Confidence   float64 `json:"confidence"`
	SpeakerID    string  `json:"speaker_id"`
	SpeakerKnown bool    `json:"speaker_known"`
	// SpeakerConfidence is the adjudicator's confidence for the speaker the
	// answer named (the chosen option's probability; 0 when the answer is
	// absent or the speaker is unknown). The speaker gate reads it (ADR-0156):
	// a speaker named below the floor is not a name, and the line's voice
	// falls back instead of trusting the slot→character link.
	SpeakerConfidence float64 `json:"speaker_confidence"`
	// SpeakerKind is the adjudicator's apparent-kind answer (ADR-0158): the
	// chosen speaker_kind option's id (the kind ids are the option ids, and
	// the roster's opaque kind keys are the same ids under "kind:"), or ""
	// when the answer is absent. SpeakerKindConfidence is that answer's
	// confidence (the chosen option's probability; 0 when absent). The kind
	// gate reads both (ADR-0158): a kind named below the floor is not a kind,
	// and the line's voice — which the speaker gate already sent to the
	// narrator fallback for a shared slot — may instead ride the generic kind
	// voice the profile's roster carries for that apparent kind.
	SpeakerKind           string  `json:"speaker_kind"`
	SpeakerKindConfidence float64 `json:"speaker_kind_confidence"`
	// Page is the 1-based display page the matcher scored for this commit
	// (ADR-0184): the box the reading matched. When set it wins over
	// choosePage's re-derivation from the OCR read — the matcher's comparison
	// is the commitment's own evidence, and one misread word is enough to
	// defeat a containment re-derivation. Zero keeps the historical page
	// choice.
	Page int `json:"page"`
}

func (w *speechWiring) onCommit(e events.Envelope) {
	var d commitPayload
	if err := json.Unmarshal(e.Data, &d); err != nil || d.Occurrence == "" || d.Generation <= 0 {
		return
	}
	text := strings.TrimSpace(d.Text)

	// The commit payload is the eligibility answer: an abstained box
	// (message_id "unknown"), a non-narratable line, or a decision that
	// refused speech is not a line the narrator speaks on the commitment.
	// It gets no speech events at all from that path — and it must NOT
	// fence: a commit with no audio of its own must not cancel the line the
	// player is listening to (issue #6; ADR-0092). Only a speakable commit
	// advances the fence, because only a speakable commit is a line whose
	// audio supersedes the previous one. The one exception the operator's
	// policy admits (ADR-0136) is the unmatched read-through: a box the
	// matcher could not name, spoken from the OCR text instead of silence.
	if !d.Eligible || d.MessageID == "" || d.MessageID == ses.UnknownMessage || text == "" {
		w.maybeUnmatchedReadThrough(e, d)
		return
	}

	// The matched line speaks the profile's canonical text, not the
	// screen's: the commit's text is the recognition tracker's output —
	// the comparison fold (lowercased), a prefix in predictive mode, a
	// typo in a fuzzy match — while the prewarm already synthesised the
	// canonical text under it (FR-11's key identity). Speaking the
	// tracker text would compute a different key for the same line and
	// miss the prewarmed bytes, and a predictive commit would speak the
	// prefix instead of the full line. Only a matched line reaches here:
	// the unmatched read-through above deliberately keeps the screen's
	// text (ADR-0136), and it has no canonical text in the corpus.
	//
	// Display pages (ADR-0155): a matched message can span several boxes,
	// and the box the player reached is the page whose text the OCR reading
	// lives in. The utterance speaks that page and only that page — the
	// boxes after the player reached are not on screen yet ("it's upcoming"),
	// and the next box of the same script message is then its own utterance
	// under its own page-scoped cache key, not a duplicate of the message's
	// first box. A message that resolved to no page keeps the historical
	// whole-message behaviour.
	pageKey := d.MessageID // the utterance's cache identity: the message, or "<message>#p<ordinal>"
	if pages := w.pagesFor(d.MessageID); len(pages) > 0 {
		var ordinal int
		var pageText string
		var found bool
		if d.Page >= 1 && d.Page <= len(pages) {
			// The matcher's page wins over choosePage's re-derivation (ADR-0184):
			// the page the reading actually matched is the one to speak.
			ordinal, pageText, found = d.Page, pages[d.Page-1], true
		} else {
			ordinal, pageText, found = choosePage(w, pages, d)
		}
		if found {
			text = pageText
			// A single-page message keeps the bare message id (the pre-change
			// identity); a multi-box message's box is page-scoped (ADR-0155).
			if len(pages) > 1 {
				pageKey = pageMessageID(d.MessageID, ordinal)
			}
		}
	}
	if canon := strings.TrimSpace(w.prewarmTexts[d.MessageID]); canon != "" && pageKey == d.MessageID {
		text = canon
	}

	// ADR-0165 (issue #48): a matched line whose canonical text carries the
	// player-name placeholder speaks the name the player actually has — the
	// session's learned name once two agreeing OCR reads trusted one, the
	// dropped form meanwhile. The substituted text is the utterance's speech
	// text, so the cache key the prewarm and the commit compute follows it.
	w.learnPlayerName(text, d.RawText)
	text = resolvePlayerPlaceholder(text, w.knownPlayerName())

	character, route := w.commitVoice(d)
	u := &utteranceInfo{
		occurrence:  d.Occurrence,
		messageID:   pageKey,
		regionID:    d.RegionID,
		generation:  d.Generation,
		text:        text,
		traceID:     e.TraceID,
		character:   character,
		voiceRoute:  route,
		committedAt: time.Now().UTC(),
	}
	if !speakable(text) {
		// A punctuation-only box ("......", "! ? !") is a silent beat in the
		// game: there is nothing to say, so it ends skipped rather than
		// sending TTS a text it can only render as silence (which the audio
		// QA then rejects as a failure). Its one terminal settles the
		// generation, the same shape the duplicate skip uses.
		w.mu.Lock()
		closed := w.closed
		if !closed {
			w.localEnded++
		}
		w.mu.Unlock()
		if closed {
			return
		}
		w.settle(u.generation)
		w.emitSpeech(events.SpeechEnded, u, map[string]any{
			"dialogue_occurrence_id": u.occurrence,
			"message_id":             u.messageID,
			"generation":             u.generation,
			"region_id":              u.regionID,
			"result":                 "skipped",
			"reason":                 "no_speakable_text",
			"character_id":           u.character,
			"trace_id":               u.traceID,
		})
		return
	}
	if w.crossRegionDuplicate(pageKey, u) {
		return
	}
	// ADR-0167 (speaker-at-commit): a committed line whose speaker slot is
	// shared (the session's shared-slot view, ADR-0156) and whose payload
	// carries no confident speaker — the shape a confident local commit
	// produces for a shared slot, which never asked Clef — owes the
	// adjudicator's speaker answer before the line speaks: the slot's linked
	// character is a claim about a slot, not a per-occurrence identity, and
	// the adjudicator is the only source that reads the frame. The ask fires
	// here, at commit time, in its own goroutine, and the utterance's voice
	// resolution waits for it — bounded by the ADR-0167 budget — and falls back
	// to the assigned voice when the answer is late or never came. The wait
	// lives in the routing worker (speak), never onCommit's path, and the
	// answer that lands inside the budget is the attribution the confidence
	// gates read (ADR-0156, ADR-0158).
	if w.decision != nil && w.speakerSlotByMessage[d.MessageID] && !w.hasConfidentSpeaker(d) {
		key := askKey(d.RegionID, d.Generation, d.MessageID)
		if p := w.beginAsk(key, w.speakerAskWait()); p != nil {
			u.speakerAskKey = key
			go w.askSpeakerFor(d, key)
		}
	}
	w.noteSpoken(text)
	w.routeUtterance(u)
}

// speakable reports whether a text has anything to say: a letter or a digit.
func speakable(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// choosePage picks the display page of one committed message whose text
// contains the OCR reading the commit carries (ADR-0155): a multi-page message
// means the same script line reached its next box, and the box the player saw is
// that page's text — the narrator must speak the page, not the message's whole
// text. The comparison reuses the matcher's fold (the one the commitment made
// the match with), so the choice is the commitment's own comparison rather than
// a second hand-rolled fold; a normalized containment check is enough for the
// page the reading lives in. Pages are in ordinal order, so the first page that
// contains the reading is the reached one. It reports no page (found = false) for
// an empty reading or a message with no pages, and the caller keeps the whole
// canonical text.
func choosePage(w *speechWiring, pages []string, d commitPayload) (ordinal int, pageText string, found bool) {
	if len(pages) == 0 {
		return 0, "", false
	}
	read := strings.TrimSpace(d.RawText)
	if read == "" {
		read = strings.TrimSpace(d.Text)
	}
	if read == "" {
		return 0, "", false
	}
	fold := func(s string) string {
		if w.idx != nil {
			return w.idx.Fold(s)
		}
		return strings.ToLower(strings.Join(strings.Fields(s), " "))
	}
	foldedRead := fold(read)
	for i, p := range pages {
		fp := fold(p)
		if foldedRead != "" && fp != "" && strings.Contains(fp, foldedRead) {
			return i + 1, pages[i], true
		}
	}
	// No page contains the read verbatim — OCR noise on the box edge ("…all
	// right? 4 = ae") is the usual reason. The page holding the largest share
	// of the read's words (within one edit) is the box on screen, if that
	// share is at least half; otherwise the read names no page and the caller
	// speaks the whole message (the historic behaviour) — a read that cannot
	// place the box must not guess.
	readWords := echoWords(read)
	if len(readWords) == 0 {
		return 0, "", false
	}
	best, bestShare := -1, 0.0
	for i, p := range pages {
		pw := echoWords(p)
		found := 0
		for _, rw := range readWords {
			for _, w := range pw {
				if withinOneEdit(rw, w) {
					found++
					break
				}
			}
		}
		if share := float64(found) / float64(len(readWords)); share > bestShare {
			best, bestShare = i, share
		}
	}
	if best >= 0 && bestShare >= 0.5 {
		return best + 1, pages[best], true
	}
	return 0, "", false
}

// pageMessageID renders the cache-identity form of one display page of a
// message: "<message id>#p<ordinal>". The page is part of the identity so a
// later box of the same script message is a different utterance — spoken under
// its own page key and its own audio — rather than a duplicate of the
// message's first box (ADR-0155).
func pageMessageID(messageID string, ordinal int) string {
	return fmt.Sprintf("%s#p%d", messageID, ordinal)
}

// pagesFor returns one committed message's display pages in ordinal order:
// the profile's page rows when the published content carries them (the
// session fills the wiring from the same content the prewarm texts came
// from), and the runtime's own split of the canonical text otherwise — the
// fallback for profiles imported before paging was derived, so existing
// profiles benefit without a re-import (ADR-0155). A missing entry means
// the message had no pages to speak.
func (w *speechWiring) pagesFor(messageID string) []string {
	if pages := w.prewarmPages[messageID]; len(pages) > 0 {
		return pages
	}
	return splitCanonicalPages(w.prewarmTexts[messageID])
}

// maxBoxLines is the dialogue box's display line limit the runtime's fallback
// page split packs against — the same observed Wind Waker box geometry the
// adapter's derivation packs against (internal/games/adapter/windwaker).
const maxBoxLines = 4

// boxPagesFromOriginal derives a message's display pages from the script's
// own box breaks: the original text's display lines (the 0x0A wrap, stored
// either as a newline or as the literal "\\x0a" escape) group into boxes
// separated by a blank line (two or more consecutive wraps), at most
// maxBoxLines lines a box. Each box's last word is then located in the
// canonical speech text and the canonical text is cut there, so a page speaks
// the speech form (the player placeholder, the folded tags) of exactly the
// box the game draws. It returns nil when the original has no text or a box
// cannot be aligned — the caller falls back to the sentence packing. On the
// board a three-box message (2, 3 and 3 lines) spoke all 269 characters for
// its first box: the sentence packing put four sentences on page one.
func boxPagesFromOriginal(original, canonical string) []string {
	if strings.TrimSpace(original) == "" || strings.TrimSpace(canonical) == "" {
		return nil
	}
	text := strings.ReplaceAll(original, `\x0a`, "\n")
	var boxes [][]string
	var tagTail []bool // the box's last display line ends in a control tag
	var box []string
	lastRaw := ""
	blank := 0
	closeBox := func() {
		if len(box) > 0 {
			boxes = append(boxes, box)
			tagTail = append(tagTail, endsInTag(lastRaw))
			box = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		words := boxWords(line)
		if len(words) == 0 {
			blank++
			continue
		}
		if blank > 0 || len(box) >= maxBoxLines {
			closeBox()
		}
		blank = 0
		box = append(box, strings.Join(words, " "))
		lastRaw = line
	}
	closeBox()
	if len(boxes) <= 1 {
		return []string{strings.TrimSpace(canonical)}
	}
	tokens := strings.Fields(canonical)
	var pages []string
	start := 0
	for i, b := range boxes {
		if i == len(boxes)-1 {
			if start >= len(tokens) {
				return nil
			}
			pages = append(pages, strings.Join(tokens[start:], " "))
			break
		}
		// A box with no word of its own ("...", or a tag for the player's
		// name and a "?") takes only the placeholder/punctuation tokens.
		cut := start - 1
		if last := lastBoxWord(b); last != "" {
			cut = -1
			for j := start; j < len(tokens); j++ {
				if foldToken(tokens[j]) == last {
					cut = j
					break
				}
			}
			if cut < 0 {
				return nil
			}
		}
		// A box that ends in a control tag (a name the game fills in) has
		// no word for it in the original; the canonical text renders it as
		// a placeholder ("{player}?") or bare punctuation. Those tokens close
		// this box, not open the next one.
		for (tagTail[i] || cut < start) && cut+1 < len(tokens) && isBoxTailToken(tokens[cut+1]) {
			cut++
		}
		if cut < start {
			return nil
		}
		pages = append(pages, strings.Join(tokens[start:cut+1], " "))
		start = cut + 1
	}
	return pages
}

// lastBoxWord is the last word of a box that folds to letters or digits:
// a box line can end in a tag-only word ("today, {tag …}?" leaves "?").
func lastBoxWord(box []string) string {
	for i := len(box) - 1; i >= 0; i-- {
		words := strings.Fields(box[i])
		for j := len(words) - 1; j >= 0; j-- {
			if w := foldWord(words[j]); w != "" {
				return w
			}
		}
	}
	return ""
}

// endsInTag reports whether a display line's last word comes from a control
// tag: after its last letter or digit there is a "{…}" ("today, {tag …}?").
func endsInTag(line string) bool {
	depth := 0
	tagAfterWord := false
	for _, r := range line {
		switch {
		case r == '{':
			depth++
		case r == '}' && depth > 0:
			depth--
			if depth == 0 {
				tagAfterWord = true
			}
		case depth == 0 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			tagAfterWord = false
		}
	}
	return tagAfterWord
}

// isBoxTailToken reports a canonical token with no display word of its own:
// a placeholder for a tag ("{player}?") or punctuation alone.
func isBoxTailToken(tok string) bool {
	return strings.HasPrefix(tok, "{") || foldToken(tok) == ""
}

// foldToken folds a canonical speech token for alignment, dropping the
// bracketed markup the canonical text carries ("{player}", "[pause:250ms]")
// that has no display word in the original.
func foldToken(tok string) string {
	var b strings.Builder
	depth := 0
	for _, r := range tok {
		switch r {
		case '{', '[':
			depth++
		case '}', ']':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return foldWord(b.String())
}

// boxWords returns one original display line's words with the control tags
// ("{tag …}") removed.
func boxWords(line string) []string {
	var b strings.Builder
	depth := 0
	for _, r := range line {
		switch {
		case r == '{':
			depth++
		case r == '}' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Fields(b.String())
}

// foldWord lowercases a word and keeps only its letters and digits.
func foldWord(w string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(w) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// splitCanonicalPages splits one message's canonical speech text into display
// pages (ADR-0155's runtime fallback). The canonical text is already folded to
// one line — a display wrap is layout, not a spoken pause — so the fallback has
// no display line structure left to pack and splits on sentence boundaries,
// packing the sentences into boxes of at most maxBoxLines. A text that fits one
// box (the single-page case) splits to a single page, which keeps the pre-change
// behaviour for every message that does not span boxes.
func splitCanonicalPages(canon string) []string {
	sentences := splitSentences(strings.TrimSpace(canon))
	if len(sentences) == 0 {
		return nil
	}
	var pages [][]string
	var box []string
	closeBox := func() {
		if len(box) == 0 {
			return
		}
		pages = append(pages, box)
		box = nil
	}
	for _, s := range sentences {
		if len(box) >= maxBoxLines {
			closeBox()
		}
		box = append(box, s)
	}
	closeBox()
	out := make([]string, 0, len(pages))
	for _, b := range pages {
		out = append(out, strings.Join(b, " "))
	}
	return out
}

// splitSentences splits on sentence-final punctuation, keeping the terminator;
// a trailing fragment without one is its own sentence. A run of terminators
// ("...", "?!", "…!") ends one sentence, not several, and a fragment with no
// letters or digits (a stray closing quote, a lone dot) joins the sentence
// before it: an ellipsis must not turn into dot-only "sentences" that pack
// into a page TTS can only render as silence (the board's presynth found 147
// such pages, all rejected as all-silent audio).
func splitSentences(s string) []string {
	isTerm := func(r rune) bool { return r == '.' || r == '!' || r == '?' || r == '…' }
	hasWord := func(t string) bool {
		for _, r := range t {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return true
			}
		}
		return false
	}
	var out []string
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		if !hasWord(t) && len(out) > 0 {
			out[len(out)-1] += t
			return
		}
		out = append(out, t)
	}
	rs := []rune(s)
	start := 0
	for i := 0; i < len(rs); i++ {
		if !isTerm(rs[i]) {
			continue
		}
		for i+1 < len(rs) && isTerm(rs[i+1]) {
			i++
		}
		add(string(rs[start : i+1]))
		start = i + 1
	}
	add(string(rs[start:]))
	return out
}

// routedMark is one recentRouted entry: where and when a line identity was
// last routed.
type routedMark struct {
	region string
	at     time.Time
}

// crossRegionDupWindow bounds the cross-region duplicate guard (ADR-0186):
// the same page committed from a DIFFERENT region within this window, with
// no different line routed in between, is the same box instance seen through
// two overlapping profile regions — the second region reads the first box
// slower (its own settle/commit latency), seconds after the first region's
// commit — not a repeat line (a repeated line re-shows after a dismissal or
// an advance, and a different line routes in between, resetting the guard).
const crossRegionDupWindow = 20 * time.Second

// crossRegionDuplicate reports whether u (identity key) duplicates a page
// routed from another region: the same key is the freshest routing (no
// different line in between), inside crossRegionDupWindow, and u comes from
// another region — and records u's routing otherwise. It also reports the
// re-commit of a page that is STILL PLAYING, from any region (the ADR-0175
// amendment's live finding: a slow typewriter partial→settled re-read of a
// page the player has not left is not a new pass). A duplicate is not
// routed — it does not fence, batch or speak — and it gets its one terminal
// (speech.ended skipped, duplicate_region) so the "did it speak, and why
// not" record is complete.
func (w *speechWiring) crossRegionDuplicate(key string, u *utteranceInfo) bool {
	if key == "" {
		return false
	}
	now := time.Now()
	w.mu.Lock()
	m, ok := w.recentRouted[key]
	dup := ok && m.region != u.regionID && w.lastRoutedKey == key && now.Sub(m.at) < crossRegionDupWindow
	playingDup := !dup && w.playingPageIs(key)
	if playingDup {
		dup = true
	}
	if !dup {
		if w.recentRouted == nil {
			w.recentRouted = map[string]routedMark{}
		}
		w.recentRouted[key] = routedMark{region: u.regionID, at: now}
		w.lastRoutedKey = key
		// Bounded: drop marks older than the window once the table grows.
		if len(w.recentRouted) > 256 {
			for k, v := range w.recentRouted {
				if now.Sub(v.at) >= crossRegionDupWindow {
					delete(w.recentRouted, k)
				}
			}
		}
	}
	w.mu.Unlock()
	if dup {
		w.log("speech: duplicate commit of a line already routed — not spoken twice",
			"session_id", w.id, "message_id", u.messageID, "region_id", u.regionID,
			"first_region_id", m.region, "since_ms", now.Sub(m.at).Milliseconds(),
			"still_playing", playingDup)
		// The duplicate's one terminal, without moving the status bar's
		// current utterance (the first copy may be playing right now).
		w.mu.Lock()
		closed := w.closed
		if !closed {
			w.localEnded++
		}
		w.mu.Unlock()
		if !closed {
			w.settle(u.generation)
			w.emitSpeech(events.SpeechEnded, u, map[string]any{
				"dialogue_occurrence_id": u.occurrence,
				"message_id":             u.messageID,
				"generation":             u.generation,
				"region_id":              u.regionID,
				"result":                 "skipped",
				"reason":                 "duplicate_region",
				"character_id":           u.character,
				"trace_id":               u.traceID,
			})
		}
	}
	return dup
}

// playingPageIs (the lock is held) reports whether the active playback
// speaks the page key: the playing generation's utterance — standalone or a
// catch-up run — carries the same message id. A re-commit of a page whose
// first copy is still audible is a duplicate of that copy, not a new page.
func (w *speechWiring) playingPageIs(key string) bool {
	if w.playingGen == 0 {
		return false
	}
	if ru := w.catchUpRuns[w.playingGen]; ru != nil {
		return ru.messageID == key
	}
	occ, ok := w.byGeneration[w.playingGen]
	if !ok {
		return false
	}
	u := w.utterances[occ]
	return u != nil && u.messageID == key
}

// maybeUnmatchedReadThrough is the unmatched read-through (ADR-0136): the box
// committed as a line the corpus cannot name and the session's policy says to
// speak what the screen actually shows, so the speaker speaks the OCR text
// (cleaned and bounded) instead of staying silent.
//
// The gate is the commit's own verdict: message_id "unknown" plus the
// COMMITTED_ABSTAINED reason is the session's "no line matched, and nothing
// adjudicated into one". A matched line the policy vetoed (safe_to_speak, the
// narrated classes) keeps its known message id and never reaches here — the
// Clef decision path is untouched. The line speaks the OCR text with the
// speaker's voice when the commit carries a known attribution (the Clef
// speaker answer, or a declared nameplate) and the narrator's otherwise;
// the text is bounded the way docs/spec/14 bounds TTS text (length cap,
// hostile-string scrub) because it is untrusted game content (trust zone C).
//
// Dedupe: the session's commit identity (region + lifetime + text) commits
// one occurrence once — the reducer suppresses the repeat — and the
// utterance table refuses to route an occurrence a second time, so the same
// unmatched box speaks at most once. A new occurrence (the box dismissed and
// re-shown, a repeat NPC) is a new line, the way a matched repeat re-speaks.
//
// The text is the screen's, not the matcher's: the raw OCR read (case and
// punctuation as drawn) — the commit's normalized text is the text machine's
// comparison key, already lower-cased, and a narration of it would flatten
// the line the way a transcript does. The raw read is what the user is
// looking at, and what "plain as day" refers to.
//
// The plausibility gate (readthrough_gate.go, the ADR-0136 amendment): a
// settled read of a screen with no box on it is still noise the moment the
// crop lands on the watermark or the border — the gate keeps the ADR's
// original contract that a read which is not a line says nothing, and a
// rejected text gets the empty read's answer: silent, logged, named
// readthrough_implausible.
func (w *speechWiring) maybeUnmatchedReadThrough(e events.Envelope, d commitPayload) {
	if d.MessageID != ses.UnknownMessage || d.Reason != ses.ReasonCommittedAbstain {
		return
	}
	readText := strings.TrimSpace(d.RawText)
	if readText == "" {
		// The raw read is absent (a payload without it, the honest zero
		// value): the normalized text is still a line, and it is the only
		// one the commit carries.
		readText = strings.TrimSpace(d.Text)
	}
	if readText == "" {
		return
	}
	w.mu.Lock()
	enabled := w.unmatchedReadThrough
	_, routed := w.utterances[d.Occurrence]
	w.mu.Unlock()
	if !enabled || routed {
		return
	}
	if d.Confidence < unmatchedMinConfidence {
		w.log("speech: unmatched read-through below the confidence floor",
			"session_id", w.id, "occurrence", d.Occurrence, "confidence", d.Confidence,
			"floor", unmatchedMinConfidence)
		return
	}
	cleaned := cleanReadThroughText(readText)
	if cleaned == "" || wordCharCount(cleaned) < 4 {
		w.log("speech: unmatched read-through empty after the noise trim",
			"session_id", w.id, "occurrence", d.Occurrence)
		return
	}
	// The plausibility gate (the ADR-0136 amendment): the trim keeps what
	// survives the edge-noise cut, and the gate decides whether that
	// survivor is a line. A rejection is the empty read's answer — silent,
	// logged, no terminal (the line never routed into the pipeline) — named
	// so the operator's "did it speak, and why not" record names the rule.
	if !isReadThroughPlausible(cleaned) {
		w.log("speech: unmatched read-through rejected by the plausibility gate",
			"session_id", w.id, "occurrence", d.Occurrence,
			"reason", "readthrough_implausible", "text", cleaned)
		return
	}
	if w.isRecentEcho(cleaned) {
		// A garbled re-read of a line the narrator just spoke (the box closing
		// or the camera cutting while OCR still reads it): the matcher cannot
		// name the garbage, the read-through would speak it a second time.
		// Seen in the A/V harness as a phantom "You probzbly home and what".
		w.log("speech: unmatched read-through is an echo of a line just spoken",
			"session_id", w.id, "occurrence", d.Occurrence,
			"reason", "readthrough_echo", "text", cleaned)
		return
	}
	character, route := w.commitVoice(d)
	u := &utteranceInfo{
		occurrence:  d.Occurrence,
		messageID:   d.MessageID,
		regionID:    d.RegionID,
		generation:  d.Generation,
		text:        cleaned,
		traceID:     e.TraceID,
		character:   character,
		voiceRoute:  route,
		committedAt: time.Now().UTC(),
		reason:      speechReasonUnmatched,
		confidence:  d.Confidence,
	}
	// Route it through the same batch / fence / backpressure / worker path a
	// matched line uses (one audio pipeline, one set of terminals), and it is
	// a speakable line for the fence: the box advanced, so its audio
	// supersedes the older generations, the catch-up policy applies, and a
	// read-through under backpressure skips with the same reason a matched
	// line does. Its own generation never collides: the commit assigned it.
	if w.crossRegionDuplicate("rt:"+strings.ToLower(cleaned), u) {
		return
	}
	w.routeUtterance(u)
}

// spokenMark is one recentSpoken entry.
type spokenMark struct {
	words []string
	at    time.Time
}

const (
	// echoWindow is how long a spoken matched line stays the reference for
	// the read-through echo check.
	echoWindow = 15 * time.Second
	// echoShare is the share of a read-through's words that must appear in
	// a recent spoken line for the read to count as its echo.
	echoShare = 0.6
	// spokenRing caps recentSpoken.
	spokenRing = 8
)

// knownPlayerName is the session's learned player name (ADR-0165, #48):
// the name a "{player}" slot speaks under, "" while the session has not
// trusted one yet.
func (w *speechWiring) knownPlayerName() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.playerName
}

// learnPlayerName records one matched commit's OCR read for the player-name
// slot and returns the name the line speaks under (ADR-0165, #48): "" while
// the session has not trusted a name, the dropped placeholder form meanwhile.
// The vote is per-session and per candidate, under w.mu, and trusts a
// candidate after playerNameVotesNeeded agreeing reads of it.
func (w *speechWiring) learnPlayerName(canon, read string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.playerName != "" {
		return w.playerName
	}
	if !strings.Contains(canon, playerPlaceholder) {
		return ""
	}
	cand := playerNameCandidate(canon, read)
	if !validPlayerName(cand) {
		return ""
	}
	// A word the script itself says is not the name: a partial read (the
	// box still typing, a cropped line) shifts the slot's index onto a
	// script word ("{player}, wait." read as "Wait." would vote "Wait").
	for _, w := range strings.Fields(canon) {
		if strings.EqualFold(foldWord(w), foldWord(cand)) {
			return ""
		}
	}
	w.playerVotes[cand]++
	if w.playerVotes[cand] >= playerNameVotesNeeded {
		w.playerName = cand
		w.playerVotes = map[string]int{}
	}
	return w.playerName
}

// noteSpoken records a matched line routed to speech for the echo check.
func (w *speechWiring) noteSpoken(text string) {
	words := echoWords(text)
	if len(words) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recentSpoken = append(w.recentSpoken, spokenMark{words: words, at: time.Now()})
	if len(w.recentSpoken) > spokenRing {
		w.recentSpoken = w.recentSpoken[len(w.recentSpoken)-spokenRing:]
	}
}

// isRecentEcho reports whether a read-through's words are mostly (echoShare)
// found, within one edit each, in a matched line spoken in the last
// echoWindow.
func (w *speechWiring) isRecentEcho(text string) bool {
	words := echoWords(text)
	if len(words) == 0 {
		return false
	}
	w.mu.Lock()
	recent := append([]spokenMark(nil), w.recentSpoken...)
	w.mu.Unlock()
	now := time.Now()
	for _, m := range recent {
		if now.Sub(m.at) > echoWindow {
			continue
		}
		found := 0
		for _, rw := range words {
			for _, sw := range m.words {
				if withinOneEdit(rw, sw) {
					found++
					break
				}
			}
		}
		if float64(found) >= echoShare*float64(len(words)) {
			return true
		}
	}
	return false
}

// echoWords folds a text to its lowercase words of three or more letters.
func echoWords(text string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if len([]rune(f)) >= 3 {
			out = append(out, f)
		}
	}
	return out
}

// withinOneEdit reports whether a and b differ by at most one substitution,
// insertion or deletion.
func withinOneEdit(a, b string) bool {
	if a == b {
		return true
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) < len(rb) {
		ra, rb = rb, ra
	}
	if len(ra)-len(rb) > 1 {
		return false
	}
	i, j, edits := 0, 0, 0
	for i < len(ra) && j < len(rb) {
		if ra[i] == rb[j] {
			i++
			j++
			continue
		}
		edits++
		if edits > 1 {
			return false
		}
		if len(ra) == len(rb) {
			j++
		}
		i++
	}
	return edits+(len(ra)-i) <= 1
}

// routeUtterance routes one committed line through the audio pipeline:
// registration, the commit→voice sample, the catch-up decision, the fence
// move, the speech.requested and the TTS worker. The immediate path alone
// prewarms the line's successors (FR-11) — a read-through line has no
// message id in the graph, so it warms nothing and, importantly, does not
// cancel the previous matched line's in-flight prewarm batch (an ineligible
// commit never did).
func (w *speechWiring) routeUtterance(u *utteranceInfo) {
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
	w.fenceAndCancel(u.generation)
	if batched {
		// The batched line's commit is still a request (the counter and the
		// event say the line was asked for), but the line's one terminal is
		// the batch's outcome, not this commit's: the run that speaks
		// it ends it (the run's result), and the backlog cap's drop, the
		// pause
		// or a session end skips it (their reasons). The generation is
		// settled with the batch's claim, so the fence never cancels — and
		// re-emits a terminal for — it again.
		w.mu.Lock()
		w.requested++
		w.current = &UtteranceState{Phase: "requested", Generation: u.generation, UtteranceID: u.occurrence, At: u.committedAt}
		w.mu.Unlock()
		w.emitSpeech(events.SpeechRequested, u, requestedPayload(u))
		// No terminal here: the batch owns the line's one terminal (its
		// outcome — the merged run's, the window's fence, the pause — emits
		// it under the claim the batch holds, ADR-0092's one-terminal rule
		// carried onto the batch's lines by lineClaim).
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
	w.emitSpeech(events.SpeechRequested, u, requestedPayload(u))
	w.wg.Add(1)
	go w.speak(u)
	// Successor prewarming (FR-11), matched lines only: the just-committed
	// line's weighted successors are synthesised in the background into the
	// same cache, so the later commit of one of them resolves from the cache.
	// The fence just moved, so this batch's "likely next" supersedes the
	// previous batch's — the previous batch is cancelled inside the call.
	// (The backpressure skip above returns earlier: the session's TTS slots
	// are all busy, and a prewarm would only queue behind them.)
	if u.reason == "" {
		// Successor prewarming (FR-11), matched lines only: the just-committed
		// line's weighted successors are synthesised in the background into the
		// same cache, so the later commit of one of them resolves from the cache.
		// The fence just moved, so this batch's "likely next" supersedes the
		// previous batch's — the previous batch is cancelled inside the call.
		// (The backpressure skip above returns earlier: the session's TTS slots
		// are all busy, and a prewarm would only queue behind them.)
		//
		// The utterance's cache identity may be page-scoped (ADR-0155: a multi-box
		// message's box is its own utterance), but the successor graph is
		// message-level, so the batch warms the containing message's successors.
		w.prewarm(prewarmSourceOf(u.messageID))
	}
}

// requestedPayload is the speech.requested payload for one routed line. A
// matched line carries exactly the historic fields; a read-through line
// adds the reason and the OCR confidence that explain the audio (ADR-0136
// "an event naming it … so traces show why") — the developer UI joins the
// line's commit (message.committed, reason COMMITTED_ABSTAINED) to the
// speech.requested on the occurrence id and sees the policy's verdict in
// both.
func requestedPayload(u *utteranceInfo) map[string]any {
	data := map[string]any{
		"dialogue_occurrence_id": u.occurrence,
		"message_id":             u.messageID,
		"generation":             u.generation,
		"region_id":              u.regionID,
		"text":                   u.text,
		"character_id":           u.character,
		"trace_id":               u.traceID,
	}
	if u.reason != "" {
		data["reason"] = u.reason
	}
	if u.confidence > 0 {
		data["ocr_confidence"] = u.confidence
	}
	return data
}

// setUnmatchedReadThrough live-moves the session's read-through policy
// (ADR-0136): the settings route (PUT /api/v1/sessions/{id}/settings) calls
// it when the operator flips the Play toggle. The switch decides what
// happens with lines committed *next* — the flag is read at the commit
// gate, so no in-flight state is touched and a line already routed is
// settled by its own terminal, exactly the catch-up switch's shape.
func (w *speechWiring) setUnmatchedReadThrough(v bool) {
	w.mu.Lock()
	w.unmatchedReadThrough = v
	w.mu.Unlock()
}

// cleanReadThroughText prepares the OCR text for speech (ADR-0136): the
// line breaks joined, the control characters dropped, the box-edge glyph
// noise trimmed (ADR-0115's TrimEdgeNoise — the "4 ", "$o?" border reads),
// and the TTS text length bounded (docs/spec/14). The text is untrusted game
// content (trust zone C): every character that leaves this function is
// printable, and the length is capped before it reaches the TTS request —
// the bound is the abuse control, the trim is the quality control.
func cleanReadThroughText(s string) string {
	// The OCR engine's line breaks become word joins: a box that wraps is
	// one sentence, and a TTS reading of "line two after a pause" is not
	// what the screen says.
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
	// The hostile-string half (docs/spec/14 "Treat OCR text as hostile
	// string … and log encoding"): control characters are dropped rather
	// than passed through — a \u0007 in the middle of a line would reach the
	// log line and the TTS request as data it was never meant to carry.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	s = b.String()
	// Collapse the whitespace runs the join and the drop leave behind.
	s = strings.Join(strings.Fields(s), " ")
	// The box-edge glyph noise, trimmed the way the matcher trims it for
	// its own evidence (ADR-0115 §2): up to two leading and two trailing
	// edge tokens of at most two runes — the border and the speaker icon
	// reads — and only when a remainder survives.
	s = strings.TrimSpace(matcher.TrimEdgeNoise(s))
	// The length bound (docs/spec/14 "Bound … TTS text length"), cut at a
	// word boundary so the cut does not land mid-word in the middle of the
	// line.
	if utf8.RuneCountInString(s) > unmatchedMaxRunes {
		s = truncateReadThrough(s, unmatchedMaxRunes)
	}
	// A read that is all noise after the trim ("$o? 4", "……") has nothing to
	// say: the empty answer is the silent one, logged by the caller.
	if !hasWordChar(s) {
		return ""
	}
	return strings.TrimSpace(s)
}

// truncateReadThrough cuts s to at most n runes at the last word boundary
// before n: the TTS length bound's degradation is a shorter line at a word
// edge, not a line ending mid-word. A cut with no boundary in reach (a run
// of unbroken glyphs) lands at n.
func truncateReadThrough(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := r[:n]
	for i := n - 1; i >= 0; i-- {
		if cut[i] == ' ' {
			return strings.TrimRight(string(cut[:i]), " ")
		}
	}
	return string(cut)
}

// catchUpBatchCommit is the catch-up policy's commit-side decision
// (ADR-0122 decision 2), taken under w.mu: it reports whether u goes into
// the batch (true) instead of the immediate path (false), and does the
// bookkeeping of the decision — the rescue of the older generations the
// batch takes over, the backlog-cap drop of the pending batch's oldest
// prefix beyond the bound, the line's append and the flush timer's arm.
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
// their own committed clocks. The backlog's cap (ADR-0175 amendment) fences
// on the spot the OLDEST pages whose estimated audio, beyond the cap, would
// leave the narrator lagging unboundedly (terminal skipped,
// catch_up_backlog_cap, counted and logged): the old window's staleness
// fence is gone — a page the player passed is spoken, not dropped.
//
// The terminal accounting (ADR-0092, one terminal per generation): the
// rescue emits none — a rescued line's terminal is the batch's outcome
// (the merged run's settled line event, the backlog cap's drop, or the
// pause and
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
	if !busy && w.catchUpRuns[u.generation] != nil {
		// The commit's generation is occupied by an in-flight run's
		// bookkeeping generation (the run pool and the commit counter can
		// meet on one number): the immediate path would register the line
		// over the run's correlation entry, and the next commit's fence would
		// cancel it — a page the player passed, lost. The batch is the only
		// path that cannot lose the page, so the collision routes it there.
		busy = true
	}
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
	// The backlog cap (ADR-0175 amendment, the owner's "say all that"): the
	// player no longer loses a page to staleness — every passed page is
	// spoken once, in order — but the narrator must not lag unboundedly:
	// the OLDEST pages beyond the cap's audio budget are fenced on the spot
	// (terminal skipped, catch_up_backlog_cap, counted and logged), and the
	// rest keep waiting to be spoken.
	if n := speech.BacklogCapDrop(w.catchUpPending, w.catchUp.BacklogCap); n > 0 {
		w.log("speech: catch-up backlog past the cap — the oldest pending pages are dropped so the narrator does not lag unboundedly",
			"session_id", w.id, "dropped", n, "remaining", len(w.catchUpPending)-n,
			"cap", w.catchUp.BacklogCap.String())
		for i := 0; i < n; i++ {
			w.catchUpBacklogDropped++
			line := w.lineTerminalInfo(w.catchUpPending[i])
			delete(w.lineClaim, w.catchUpPending[i].Occurrence)
			terminals = append(terminals, terminal{line, "skipped", "catch_up_backlog_cap"})
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
	if w.runGenReserve <= u.generation {
		w.runGenReserve = u.generation + 1
	}
	if w.playingGen == 0 {
		// Nothing is audible (the batch is busy only with pending lines or
		// an in-flight synthesis it just rescued): there is no playback to
		// wait behind, so the window would only delay the newest line. The
		// batch flushes after a short coalescing delay instead (a duplicate
		// commit of the same box lands within it). The A/V harness run
		// av-20261021T210125Z measured the full window here: an idle,
		// cached line spoke 824 ms after its text settled (#41).
		if w.catchUpTimer != nil {
			t := w.catchUpTimer
			w.catchUpTimer = nil
			t()
		}
		w.armCatchUpTimerLocked(catchUpIdleFlush)
	} else if w.catchUpTimer == nil {
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
// catchUpIdleFlush is the batch's flush delay when nothing is playing: long
// enough to coalesce near-simultaneous commits, short against the ≤ 500 ms
// settled→audio p50 budget.
const catchUpIdleFlush = 120 * time.Millisecond

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

	// The line's speaker, resolved on the character the commit named it
	// (ADR-0136): a matched line's character is the curator's assignment for
	// its message id (resolveVoice's speakerFor), and a read-through line's
	// is the commit's own attribution — the Clef speaker answer or a declared
	// nameplate — or the narrator when unattributed. Resolving on the
	// message id would lose the read-through's attribution, because the line
	// has no message id in the corpus to look up.
	// ADR-0167 (speaker-at-commit): a committed shared-slot line that owed a
	// speaker-at-commit ask waits — bounded by the ADR-0167 budget — for the
	// adjudicator's answer before resolving its voice, and falls back to the
	// assigned voice when the answer is late or never came. The wait is in the
	// routing worker (this goroutine), never onCommit's path, and the ask that
	// answers it was started at commit time (onCommit).
	character := u.character
	if u.speakerAskKey != "" {
		var attribution attributionAsk
		if res, pending := w.waitAsk(ctx, u.speakerAskKey, w.speakerAskWait()); pending {
			attribution = res
			// The answer overlays the committed line's own voice resolution:
			// the adjudicator's confident speaker, else the confident apparent
			// kind, else the assigned character the local evidence committed
			// with (the ADR-0156/ADR-0158 gates read the attribution).
			character, _ = w.commitVoice(commitPayload{
				MessageID:             u.messageID,
				SpeakerID:             attribution.speakerID,
				SpeakerKnown:          attribution.speakerKnown,
				SpeakerConfidence:     attribution.speakerConf,
				SpeakerKind:           attribution.speakerKind,
				SpeakerKindConfidence: attribution.speakerKindConf,
			})
		}
	}
	rv, why := w.resolveVoiceForCharacter(ctx, character)
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

	key, err := w.lineCacheKey(rv, u.messageID, u.text)
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
		"voice_route":            u.voiceRoute,
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

// runCatchUpBatch is the flush (ADR-0175 amendment): the batch's lines
// speak as their OWN utterances, in commit order, so each page's cached
// clip plays back-to-back (the merged re-synthesis would be a cache miss
// and slow). The runs share the batch's newest
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
	// ADR-0175 amendment (the owner's "say all that", the evidence is the
	// rapid-aaa live finding): a passed page is never dropped for staleness
	// any more — no max-age drop, no window fence. Every line flushes as its
	// OWN utterance, in commit order, so each page's own cached clip
	// (presynth makes nearly every page a cache hit) plays back-to-back
	// instead of a re-synthesis of a merged text (the merge is a cache miss
	// and slow). The backlog stays bounded by the cap's drop at the commit
	// side (reason catch_up_backlog_cap).
	runs := make([]speech.CatchUpRun, 0, len(batch))
	for _, l := range batch {
		runs = append(runs, speech.CatchUpRun{Speaker: l.Speaker, Text: l.Text, Lines: []speech.CatchUpLine{l}})
	}

	type mergedRun struct {
		u   *utteranceInfo
		run speech.CatchUpRun
		gen int64
	}
	var merged []mergedRun
	for _, run := range runs {
		first := run.Lines[0]
		var traceID string
		if lu := w.lookupUtterance(first.Occurrence); lu != nil {
			traceID = lu.traceID
		}
		u := &utteranceInfo{
			occurrence:  games.NewID(),
			messageID:   speech.MergedOriginalID(run.MessageIDs()),
			regionID:    first.RegionID,
			text:        run.Text,
			traceID:     traceID,
			character:   run.Speaker,
			committedAt: now,
			mergedLines: append([]speech.CatchUpLine(nil), run.Lines...),
		}
		merged = append(merged, mergedRun{u: u, run: run})
	}
	if len(merged) == 0 {
		return
	}
	// Register the merged utterances (the correlation tables the speech
	// events join on) and the bookkeeping generations, and approve the
	// fence — all before the first submit. The generations come from the run
	// pool (runGenReserve), not the batch's newest commit generation: the
	// pool is bumped past every commit by catchUpBatchCommit, so a run
	// generation can never be a commit generation — a commit that lands
	// mid-flush books a different generation instead of colliding with a run
	// the flush is registering.
	w.mu.Lock()
	base := w.runGenReserve
	for i := range merged {
		merged[i].gen = base + int64(i)
		merged[i].u.generation = merged[i].gen
	}
	w.runGenReserve = base + int64(len(merged))
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
	// The fence's approved set evicts from the front at its cap: a flush's
	// runs are all approved before the first submit, so the cap is the
	// larger of the default bound and this flush's run count — a flush
	// beyond the default cap (the backlog's) must not fence its own
	// oldest queued run.
	approveCap := speech.DefaultCatchUpApprovedCap
	if len(merged) > approveCap {
		approveCap = len(merged)
	}
	for _, m := range merged {
		w.fence.approve(m.gen, approveCap)
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
	}
	// The runs speak in commit order, one at a time: the back-to-back clip
	// the owner asked for (ADR-0175 amendment). Each worker's submit is
	// gated behind the previous run's submit, so a TTS round trip that
	// finishes early cannot reorder the playback — the clips play in the
	// order the player passed the pages, and each clip starts as soon as
	// the one before it ends.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for _, m := range merged {
			w.wg.Add(1)
			w.speakMerged(m.u, m.run)
		}
	}()
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
		"voice_route":            u.voiceRoute,
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
	return resolveLineVoice(ctx, w.voices, w.assets, character, w.speakerAliasByID, w.narratorFallback)
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

// prewarmSourceOf maps a committed utterance's cache identity back to the
// message it displays: the bare message id, or the "<message id>#p<ordinal>"
// page form the utterance spoke under (ADR-0155). The successor graph is
// message-level, so a page-scoped commit warms the containing message's
// successors.
func prewarmSourceOf(messageID string) string {
	if i := strings.LastIndexByte(messageID, '#'); i >= 0 {
		return messageID[:i]
	}
	return messageID
}

// speculativePrewarm is the pipeline's OnAdjudicating hook (docs/spec/07,
// the overlap of the Clef round trip with the synthesis): while the
// adjudication is in flight, the top local candidate's line is synthesised
// in the background under the SAME versioned cache key a later commit of the
// line would use, so when the verdict lands and the commit confirms the same
// message, the speech cache's singleflighted GetOrGenerate either joins the
// in-flight generation or resolves from the cache instead of paying the TTS
// round trip.
//
// It runs on the pipeline's rank worker, so it must stay fast: it spawns one
// background worker (the same prewarmWorker the successor prewarming uses —
// one synthesis per key, fenced by the session's end) and returns. No audio
// is scheduled or played here: the commitment and the speech fences are
// untouched, so a verdict that narrows away from the prewarmed line costs at
// most one wasted synthesis, never a stale playback. An empty message id (no
// local candidate) and a stopped wiring are no-ops.
func (w *speechWiring) speculativePrewarm(messageID string) {
	if messageID == "" {
		return
	}
	w.mu.Lock()
	if w.closed || w.endCtx == nil {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	go w.prewarmWorker(w.endCtx, messageID)
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

// PrewarmCandidate speculatively prewarms one profile message into the
// shared TTS cache before it commits: a candidate line the decision layer
// surfaced, or a scene line the operator wants ready, warms through the
// same worker the successor batch uses (same key identity, background
// priority, one synthesis per key), so its later commit resolves from the
// cache. Safe to call from any goroutine; a nil wiring, an unknown or
// empty message id, a closed session, and a message with no canonical text
// do nothing. The job's lifetime is the session's end context — not a
// commit batch's: the speculative candidate belongs to no batch, nothing
// fences it, and a cancellation would only waste what the next commit
// could still use.
func (w *speechWiring) PrewarmCandidate(messageID string) {
	if w == nil || messageID == "" || messageID == ses.UnknownMessage {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	go w.prewarmWorker(w.endCtx, messageID)
}

// PrewarmMessages speculatively prewarms a list of scene lines (the
// PrewarmCandidate contract, one message at a time; the shared
// one-synthesis-per-key booking dedupes a list that overlaps a batch).
func (w *speechWiring) PrewarmMessages(messageIDs []string) {
	for _, id := range messageIDs {
		w.PrewarmCandidate(id)
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
	pages := w.pagesFor(messageID)
	if len(pages) == 0 {
		return // no canonical text in the profile: nothing to synthesise
	}
	rv, _ := w.resolveVoice(batch, messageID)
	if rv.profile.ID == "" {
		return // no approved voice for the speaker (and no narrator voice):
		// the same silence-with-reason the commit path takes; the line's own
		// commit reports it when it comes.
	}
	pctx, cancel := context.WithTimeout(batch, w.prewarmCfg.jobDeadline)
	defer cancel()
	// One synthesis per display page (ADR-0155): a multi-page message's later
	// boxes are separate utterances with page-scoped cache identities, so the
	// worker fills one key per page; a single-box message keeps the bare message
	// id (the pre-change behaviour).
	for i, pageText := range pages {
		pageID := messageID
		if len(pages) > 1 {
			pageID = pageMessageID(messageID, i+1)
		}
		// The prewarm speaks the same text a later commit of this line would:
		// the player-name slot resolved onto the session's learned name (the
		// dropped form while the session has not trusted one, ADR-0165), so
		// the key below is the one the commit path computes for the same line.
		pageText = resolvePlayerPlaceholder(pageText, w.knownPlayerName())
		key, err := w.lineCacheKey(rv, pageID, pageText)
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
			continue
		}
		w.prewarmKeys[string(key)] = struct{}{}
		w.prewarmRequested++
		w.mu.Unlock()

		if batch.Err() != nil {
			w.prewarmKeyWasted(key)
			continue
		}
		res, err := w.voices.Speech.GetOrGenerate(speech.WithKeyInput(pctx, pageID), key, speech.PriorityBackground, func(ctx context.Context) (tts.Audio, error) {
			return rv.generate(ctx, pageText)
		})
		if err != nil {
			if batch.Err() == nil && !w.closedLoad() {
				// A provider failure the batch did not cause (or the job deadline
				// expired while the batch lived): no bytes were committed, so the key
				// goes back to "unbooked" and a later batch may request it again.
				w.prewarmKeyDrop(key)
				w.log("speech: prewarm generation failed",
					"session_id", w.id, "message_id", pageID, "reason", providerErrorKind(err))
				continue
			}
			// Cancelled (fence move, scene change, session end): the job is cut
			// short and its budget is gone.
			w.prewarmKeyWasted(key)
			continue
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
}

// lineCacheKey is a matched line's TTS cache key under a resolved voice: the
// identity the prewarm, the profile pre-synthesis (presynth.go) and a commit
// of the line share, so cached audio is found by whichever path speaks it.
// The utterance's message id is the identity: the bare message id for a
// single-box message, or the page-scoped "<message id>#p<ordinal>" for one
// display page of a multi-box message (ADR-0155) — the page is part of the
// identity, so a later box of the same script message is a different utterance
// under its own key, not a duplicate of the message's first box.
func (w *speechWiring) lineCacheKey(rv resolvedVoice, messageID, text string) (speech.CacheKey, error) {
	return lineKey(w.voices.BackendDigest, rv.profile.Language, rv.profile.Fingerprint, rv.profile.ModelID, messageID, text)
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

// commitSpeaker resolves the speaker for one committed occurrence's voice
// (ADR-0156, ADR-0158): the adjudicator's per-occurrence speaker answer when it
// is confident — the slot→character link cannot be trusted for a slot that is
// shared, and the adjudicator is the only source that reads the frame — else
// the assigned character for a slot that maps to a single character, else the
// neutral voice for a shared slot the answer could not name.
//
// The confidence gate is the same half-threshold the safe_to_speak answer
// already uses: a speaker named below it is not a name, and the line falls
// back rather than trusting the slot→character link that produced the
// mis-attribution in the first place. A slot that maps to a single character
// keeps its assignment (the pre-ADR-0156 behaviour), because there the link
// is the identity rather than a guess about a shared slot.
//
// ADR-0158: when the gate falls back (a shared slot whose answer could not
// name the speaker, or an unattributed line) the adjudicator's apparent-kind
// answer may still route the line: a kind named at or above the same floor is
// a kind, and the profile's roster may carry a character whose aliases hold
// the opaque "kind:<kind>" key — the generic voice for an unnamed speaker of
// that apparent kind. A confident kind voices the line through that character;
// otherwise the narrator fallback stands exactly as before. The resolution is
// recorded by commitVoice alongside the character it chose.
func (w *speechWiring) commitSpeaker(d commitPayload) string {
	character, _ := w.commitVoice(d)
	return character
}

// voiceRoute names how one committed occurrence's voice was resolved (ADR-0158):
// the adjudicator's confident speaker answer ("speaker"), the confident
// apparent-kind answer routed through the roster's generic kind character
// ("speaker_kind"), the neutral narrator fallback ("narrator"), or the
// roster's own assignment ("assigned" — a slot that maps to a single
// character, where the link is the identity). The route is the
// distinguishable half of the ADR-0156/0158 voice decision: the dev UI and the
// A/V harness read it to tell a kind-routed line from a narrator line without
// parsing which character was spoken.
const (
	voiceRouteSpeaker     = "speaker"
	voiceRouteSpeakerKind = "speaker_kind"
	voiceRouteNarrator    = "narrator"
	voiceRouteAssigned    = "assigned"
)

// commitVoice resolves one committed occurrence's voice and the route it
// took: the character the line speaks with, and the route that character was
// reached by (ADR-0156's gate, ADR-0158's kind fallback).
func (w *speechWiring) commitVoice(d commitPayload) (character string, route string) {
	if d.SpeakerKnown && d.SpeakerID != "" && d.SpeakerConfidence >= SpeakerConfidenceFloor {
		return d.SpeakerID, voiceRouteSpeaker
	}
	// A shared slot keeps its assigned character unless the adjudicator
	// confidently names someone else (above): ADR-0156 amendment 3. Speaking
	// shared-slot lines through the narrator took the voice away from 13 of
	// the 29 cast characters (Aryll, Tetra, Medli, Grandma...) on the board,
	// since a confident local match never asks Clef, to fix the occasional
	// villager on a slot the curator linked to a named character.
	character = w.speakerFor(d.MessageID)
	if character == narratorCharacter {
		// Unattributed (no assignment the slot did not settle): the kind may
		// still route the line before the neutral fallback does (ADR-0158).
		if ch, ok := w.kindSpeakerFor(d); ok {
			return ch, voiceRouteSpeakerKind
		}
		return narratorCharacter, voiceRouteNarrator
	}
	// A shared slot whose assigned voice plainly contradicts the speaker the
	// frame shows — a confident apparent kind on the other side of the
	// male / not-male line from the assigned voice's measured pitch — takes
	// the generic kind voice instead (ADR-0167 amendment): the male villager
	// on a slot the curator linked to Rose spoke in her voice on the board.
	if w.speakerSlotByMessage[d.MessageID] {
		if ch, ok := w.kindSpeakerFor(d); ok && w.kindContradictsVoice(d.SpeakerKind, character) {
			return ch, voiceRouteSpeakerKind
		}
	}
	// A slot that maps to a single character keeps its assignment (the
	// pre-ADR-0156 behaviour): the link is the identity there.
	return character, voiceRouteAssigned
}

// kindContradictsVoice reports whether an apparent kind and a character's
// measured voice disagree on male versus not male. Unknown voices and the
// creature kind never contradict.
func (w *speechWiring) kindContradictsVoice(kind, character string) bool {
	if kind == "" || kind == systemone.SpeakerKindCreature || kind == systemone.SpeakerUnknown {
		return false
	}
	w.mu.Lock()
	low, known := w.voiceLow[character]
	w.mu.Unlock()
	if !known {
		return false
	}
	kindLow := kind == systemone.SpeakerKindAdultMale || kind == systemone.SpeakerKindElderMale
	return kindLow != low
}

// warmVoiceClasses measures each assigned character's approved voice once
// (median F0 of the reference WAV, speech.MeasurePitch) and records whether
// it is a low adult-male voice. It runs off the commit path, at session
// start: a character not yet measured simply never contradicts a kind.
func (w *speechWiring) warmVoiceClasses(ctx context.Context) {
	seen := map[string]bool{}
	for _, ch := range w.speakerByMessage {
		if ch == "" || ch == narratorCharacter || seen[ch] {
			continue
		}
		seen[ch] = true
		if ctx.Err() != nil {
			return
		}
		rv, _ := w.approvedVoice(ctx, ch)
		if rv.profile.ID == "" {
			continue
		}
		ref := rv.refWAV
		if len(ref) == 0 {
			var err error
			if ref, err = readReference(ctx, w.assets, rv.profile.ReferenceAssetID); err != nil {
				continue
			}
		}
		p, err := speech.MeasurePitch(ref)
		if err != nil || p.VoicedFrames == 0 {
			continue
		}
		w.mu.Lock()
		if w.voiceLow == nil {
			w.voiceLow = map[string]bool{}
		}
		w.voiceLow[ch] = p.F0 < speech.PitchLowAdultMaleHz
		w.mu.Unlock()
	}
}

// kindSpeakerFor resolves a committed occurrence's confident apparent-kind
// answer onto the character that owns the generic kind voice (ADR-0158): the
// kind the adjudicator chose, carried through the roster's opaque
// "kind:<kind>" key, onto the character whose aliases include it. It reports
// ok only when the kind is named (not the unknown fallback), is at or above
// the speaker-confidence floor, and the roster carries such a character;
// the caller then voices the line with that character's voice, which
// resolveVoiceForCharacter resolves (falling back to the narrator when that
// character has no approved voice — the degraded mode, not a substitute).
func (w *speechWiring) kindSpeakerFor(d commitPayload) (string, bool) {
	kind := d.SpeakerKind
	if kind == "" || kind == systemone.SpeakerUnknown || d.SpeakerKindConfidence < SpeakerConfidenceFloor {
		return "", false
	}
	id, ok := w.kindSpeakerByKey[kindSpeakerKeyPrefix+kind]
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// kindVoiceKinds are the adjudicator's named apparent kinds (ADR-0158), the
// unknown fallback excluded.
var kindVoiceKinds = []string{
	systemone.SpeakerKindAdultMale, systemone.SpeakerKindAdultFemale,
	systemone.SpeakerKindBoy, systemone.SpeakerKindGirl,
	systemone.SpeakerKindElderMale, systemone.SpeakerKindElderFemale,
	systemone.SpeakerKindCreature,
}

// addKindVoiceKeys lets a kind that no roster character claims still route
// when an approved voice exists under the opaque key itself ("kind:boy"):
// the operator auditions and approves the generic kind voices through the
// voices API with the key as the character, with no profile edit (ADR-0158
// amendment). A roster character carrying the kind alias keeps precedence.
func (w *speechWiring) addKindVoiceKeys(ctx context.Context) {
	if w.voices == nil || w.voices.Store == nil {
		return
	}
	if w.kindSpeakerByKey == nil {
		w.kindSpeakerByKey = map[string]string{}
	}
	for _, k := range kindVoiceKinds {
		key := kindSpeakerKeyPrefix + k
		if _, ok := w.kindSpeakerByKey[key]; ok {
			continue
		}
		if rv, _ := w.approvedVoiceForKey(ctx, key); rv.profile.ID != "" {
			w.kindSpeakerByKey[key] = key
		}
	}
}

// approvedVoice resolves one character's current approved voice: the
// highest approved revision carrying a fingerprint and a reference
// (internal/voices' approval invariants). The second return is "" only
// when the character has no approved voice at all; any non-empty value is
// a specific fault (the approved reference is unreadable, the name could
// not be derived).
func (w *speechWiring) approvedVoice(ctx context.Context, character string) (resolvedVoice, string) {
	return approvedVoice(ctx, w.voices, w.assets, character, w.speakerAliasByID)
}

func (w *speechWiring) approvedVoiceForKey(ctx context.Context, character string) (resolvedVoice, string) {
	return approvedVoiceForKey(ctx, w.voices, w.assets, character)
}

// loadReference reads one approved reference clip from the asset store.
func (w *speechWiring) loadReference(ctx context.Context, assetID string) ([]byte, error) {
	return readReference(ctx, w.assets, assetID)
}

// approvedVoiceForKey resolves one character's current approved voice: the
// highest approved revision carrying a fingerprint and a reference (internal/
// voices' approval invariants). The second return is "" only when the character
// has no approved voice at all; any non-empty value is a specific fault (the
// approved reference is unreadable, the name could not be derived).
func approvedVoiceForKey(ctx context.Context, voiceSvc *voices.Service, assets assets.Store, character string) (resolvedVoice, string) {
	profiles, err := voiceSvc.Store.ListVoiceProfiles(ctx, character)
	if err != nil {
		// The store is the appliance's own database: a read failure is a
		// fault to surface, not a silent "no voice".
		return resolvedVoice{}, fmt.Sprintf("voice_store:%v", err)
	}
	for _, p := range profiles { // ordered by revision, descending
		if p.State != store.VoiceStateApproved || p.Fingerprint == "" || p.ReferenceAssetID == "" {
			continue
		}
		reg, regErr := voiceSvc.Store.GetVoiceRegistration(ctx, p.ID, voiceSvc.InstanceID)
		if regErr == nil && reg.State == store.VoiceRegReady && reg.RegisteredName != "" {
			// Ready on this instance: the Base checkpoint speaks the
			// registered name.
			return resolvedVoice{
				profile: p,
				name:    reg.RegisteredName,
				ready:   true,
				client:  voiceSvc.Base,
			}, ""
		}
		// Not ready (or no row): the call carries the approved reference
		// inline, speaking the revision's registry name.
		ref, err := readReference(ctx, assets, p.ReferenceAssetID)
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
			client:  voiceSvc.Base,
		}, ""
	}
	return resolvedVoice{}, ""
}

// approvedVoice resolves one character's current approved voice through the
// roster's speaker-link aliases (ADR-0070/ADR-0101): the character itself
// first, then the display name it answers to (the Cast UI / auto-cast names,
// ADR-0109/ADR-0135, and the imported ids, ADR-0101). The second return is "" only
// when the character has no approved voice at all; any non-empty value is a
// specific fault.
func approvedVoice(ctx context.Context, voiceSvc *voices.Service, assets assets.Store, character string, aliasByID map[string]string) (resolvedVoice, string) {
	if rv, why := approvedVoiceForKey(ctx, voiceSvc, assets, character); rv.profile.ID != "" || why != "" {
		return rv, why
	}
	if alias := aliasByID[character]; alias != "" && alias != character {
		return approvedVoiceForKey(ctx, voiceSvc, assets, alias)
	}
	return resolvedVoice{}, ""
}

// resolveLineVoice resolves the voice one utterance speaks under: the
// approved voice of the speaker character, else the narrator voice when the
// profile's narrator fallback is on (ADR-0101) and the character has none —
// never a different character's voice, never a builtin. The second return is
// the skip reason ("" when the line resolves, else what kept it silent:
// narrator_fallback_disabled, no_approved_voice).
func resolveLineVoice(ctx context.Context, voiceSvc *voices.Service, assets assets.Store, character string, aliasByID map[string]string, narratorFallback bool) (resolvedVoice, string) {
	if rv, why := approvedVoice(ctx, voiceSvc, assets, character, aliasByID); rv.profile.ID != "" || why != "" {
		return rv, why
	}
	if character == narratorCharacter {
		// The narrator's own line has no fallback question to ask: the
		// speaker and the fallback are the same voice.
		return resolvedVoice{}, "no_approved_voice"
	}
	if narratorFallback {
		if rv, why := approvedVoice(ctx, voiceSvc, assets, narratorCharacter, aliasByID); rv.profile.ID != "" || why != "" {
			return rv, why
		}
		return resolvedVoice{}, "no_approved_voice"
	}
	nr, nwhy := approvedVoice(ctx, voiceSvc, assets, narratorCharacter, aliasByID)
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

// readReference reads one approved reference clip from the asset store.
func readReference(ctx context.Context, assets assets.Store, assetID string) ([]byte, error) {
	r, _, err := assets.Get(ctx, assetID)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	return out, err
}

// lineKey computes one utterance's TTS cache key under a resolved voice: the
// identity the live commit, the successor prewarm and the profile
// pre-synthesis all compute, so cached audio is found by whichever path
// speaks it (ADR-0153 "One key identity"; ADR-0155's page-scoped form; ADR-0156's
// slot-adjusted voice). The voice resolution itself lives with the caller; this
// is the one place the key materialization happens.
func lineKey(backendDigest, language, voiceFingerprint, modelID, messageID, text string) (speech.CacheKey, error) {
	return speech.CacheKeyOf(speech.KeyInput{
		UtteranceText:            text,
		OriginalMessageID:        messageID,
		SpeechRulesRevision:      1,
		Language:                 language,
		VoiceRevisionFingerprint: voiceFingerprint,
		TTSModelID:               modelID,
		BackendConfigDigest:      backendDigest,
		Format:                   "wav/pcm16",
	})
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
				Batched:       w.catchUpBatched,
				Dropped:       w.catchUpDropped,
				MergedRuns:    w.catchUpMerged,
				BacklogCapped: w.catchUpBacklogDropped,
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
		// Publish the commit first, then route it: the speech events onCommit
		// emits synchronously (speech.requested, a duplicate's skipped
		// terminal) are effects of the commit and must follow it in the
		// stream. Routing first put them ahead of their own commit, and a
		// consumer that builds lines from commits lost the terminal of a
		// duplicate-region skip (the live catch-up test's claim rule).
		out, err := p.inner.Publish(ctx, e)
		if err == nil {
			p.w.onCommit(out)
		}
		return out, err
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
