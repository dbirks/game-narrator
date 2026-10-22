package runtime

// Profile pre-synthesis (ADR 0153, #41): fill the TTS cache with a published
// profile's lines ahead of play, under the exact keys a live session's
// commits compute, so gameplay hits the cache instead of paying a 0.5–1 s
// synthesis (and surviving a gateway outage on cached lines). The session's
// own per-line prewarm (prewarmWorker) predicts a few successors during play;
// this is the whole profile, offline, at background priority.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// canonicalSpeechText is the text a matched commit speaks for one message:
// the speech text, else the original text, else the normalized key — what a
// matched commit speaks, not the tracker's folded read.
func canonicalSpeechText(m games.Message) string {
	t := m.SpeechText
	if t == "" {
		t = m.OriginalText
	}
	if t == "" {
		t = m.NormalizedText
	}
	return strings.TrimSpace(t)
}

// profileSpeechMaps is the speech-side view of a profile's content that a
// session and the pre-synthesis share, so both compute the same cache key
// for a line: message id → speaker character (through the curator's speaker
// link, ADR-0070/ADR-0101), character id → display name (voices created via
// the Cast UI or auto-cast store the name, ADR-0109/ADR-0135; imported ones
// the id, ADR-0101), and message id → canonical TTS text (the speech text,
// else the original text, else the normalized key — what a matched commit
// speaks, not the tracker's folded read).
// It also returns message id → display pages (ADR-0155): the content's page
// rows when the profile carries them, else the runtime's own split of the
// canonical text (the fallback for profiles imported before paging was
// derived).
func profileSpeechMaps(content store.ProfileContent, resolveSpeaker func(string) string) (speakers, aliasByID, texts map[string]string, pages map[string][]string) {
	speakers = make(map[string]string, len(content.Messages))
	aliasByID = make(map[string]string, len(content.Characters))
	for _, ch := range content.Characters {
		if ch.ID != "" && ch.Name != "" && ch.Name != ch.ID {
			aliasByID[ch.ID] = ch.Name
		}
	}
	texts = make(map[string]string, len(content.Messages))
	pages = make(map[string][]string, len(content.Messages))
	for _, p := range content.Pages {
		pages[p.MessageID] = append(pages[p.MessageID], p.PageText)
	}
	for _, msg := range content.Messages {
		if msg.SpeakerID != "" {
			speakers[msg.ID] = resolveSpeaker(msg.SpeakerID)
		}
		t := canonicalSpeechText(msg)
		if t != "" {
			texts[msg.ID] = t
		}
		if _, ok := pages[msg.ID]; !ok {
			// ADR-0155 fallback: a profile that carries no page rows for the
			// message gets the runtime's split of its canonical text, so the
			// session and the pre-synthesis speak reached pages without a
			// re-import.
			if pt := splitCanonicalPages(t); len(pt) > 0 {
				pages[msg.ID] = pt
			}
		}
	}
	return speakers, aliasByID, texts, pages
}

// sharedSpeakerSlots is the ADR-0156 speaker-attribution view of a published
// profile: for each message that carries a speaker, whether that speaker's
// slot is shared — the slot takes more than one speaker, so the curator's
// slot→character link is a claim about a slot rather than a per-occurrence
// identity, and a line on a shared slot must not take its voice from the
// link alone.
//
// A character answers to the speaker keys it is known by — its name plus each
// alias, folded the way the roster folds labels (the same textnorm the
// importer uses). A character that answers to more than one key is one the
// roster reached through a slot link (the Wind Waker's opaque `ww:spk:<n>`
// keys), which is the shared shape the live play showed: the slot is a shared
// slot, and the lines that resolved through it are the ones that must be
// re-attributed per occurrence rather than voiced through the linked
// character. A character that answers to a single key is a single-speaker
// attribution and keeps its assignment.
func sharedSpeakerSlots(content store.ProfileContent, resolveSpeaker func(string) string) map[string]bool {
	keysByChar := map[string][]string{}
	for _, c := range content.Characters {
		if c.ID == "" {
			continue
		}
		seen := map[string]bool{}
		var keys []string
		addKey := func(s string) {
			k := textnorm.Normalize(s, textnorm.Options{})
			if k == "" || seen[k] {
				return
			}
			seen[k] = true
			keys = append(keys, k)
		}
		addKey(c.Name)
		if len(c.Aliases) > 0 {
			var aliases []string
			_ = json.Unmarshal(c.Aliases, &aliases)
			for _, a := range aliases {
				addKey(a)
			}
		}
		keysByChar[c.ID] = keys
	}
	out := map[string]bool{}
	for _, m := range content.Messages {
		if m.SpeakerID == "" {
			continue
		}
		char := resolveSpeaker(m.SpeakerID)
		if len(keysByChar[char]) > 1 {
			out[m.ID] = true
		}
	}
	return out
}

// PresynthOptions bound one pre-synthesis run.
type PresynthOptions struct {
	// Limit caps the lines considered (profile order); 0 = all.
	Limit int
	// Concurrency is the parallel syntheses (default 1, max 4): the remote
	// TTS is shared with live play.
	Concurrency int
	// MaxConsecutiveFailures stops the run (a gateway outage) after this
	// many provider failures in a row (default 5).
	MaxConsecutiveFailures int
	// FreeBytes reports the free space of the cache's volume; nil skips the
	// disk guard. ReserveBytes is kept free (default 500 MB).
	FreeBytes    func() (uint64, error)
	ReserveBytes uint64
}

// PresynthProgress is the run's honest count.
type PresynthProgress struct {
	Total       int   `json:"total"`
	Done        int   `json:"done"`
	CacheHits   int   `json:"cache_hits"`
	Synthesized int   `json:"synthesized"`
	Failed      int   `json:"failed"`
	Skipped     int   `json:"skipped"` // no approved voice for the speaker (and no narrator voice)
	Bytes       int64 `json:"bytes"`   // bytes newly synthesized
}

// ErrPresynthDiskBudget stops a run whose projected cache growth would eat
// into the reserve.
var ErrPresynthDiskBudget = errors.New("presynth: projected cache size exceeds the free space minus the reserve")

// FreeBytesAt is the production disk probe for PresynthOptions.FreeBytes.
func FreeBytesAt(dir string) func() (uint64, error) {
	return func() (uint64, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return 0, err
		}
		return st.Bavail * uint64(st.Bsize), nil
	}
}

// PresynthesizeProfile synthesizes every line of the profile whose speaker
// resolves to an approved voice into the shared TTS cache. It is idempotent
// (a cached line costs one lookup) and so resumable; progress is reported
// after every line.
func (m *Manager) PresynthesizeProfile(ctx context.Context, profileID string, opt PresynthOptions, progress func(PresynthProgress)) (PresynthProgress, error) {
	var p PresynthProgress
	if m.opts.Speech == nil || m.opts.Speech.Voices == nil || m.opts.Speech.Voices.Speech == nil {
		return p, errors.New("presynth: no speech pipeline is configured")
	}
	prof, _, err := m.opts.Store.GetProfile(ctx, profileID)
	if err != nil {
		return p, err
	}
	content, err := m.opts.Store.GetProfileContent(ctx, profileID)
	if err != nil {
		return p, err
	}
	speakers, aliases, texts, pages := profileSpeechMaps(content, speakerLinkResolver(content.Characters))
	w := &speechWiring{
		id:               "presynth:" + profileID,
		log:              m.log.Debug,
		voices:           m.opts.Speech.Voices,
		assets:           m.opts.Speech.Assets,
		speakerByMessage: speakers,
		narratorFallback: prof.NarratorFallback,
		speakerAliasByID: aliases,
		// The shared-slot view the live commit applies (ADR-0156).
		speakerSlotByMessage: sharedSpeakerSlots(content, speakerLinkResolver(content.Characters)),
	}

	// One entry per utterance the pre-synthesis fills: a multi-page message
	// contributes one entry per display page (each under its own page-scoped
	// cache key), a single-page message one entry under the bare message id —
	// the same key the live commit path computes, so a pre-synthesized page is
	// a hit for the commit that speaks it (ADR-0155).
	entries := make([]struct {
		id   string
		text string
	}, 0, len(content.Messages))
	for _, msg := range content.Messages {
		msgPages := pages[msg.ID]
		if len(msgPages) > 1 {
			for i, pageText := range msgPages {
				entries = append(entries, struct {
					id   string
					text string
				}{pageMessageID(msg.ID, i+1), pageText})
			}
			continue
		}
		if t := texts[msg.ID]; t != "" {
			entries = append(entries, struct {
				id   string
				text string
			}{msg.ID, t})
		}
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.id)
	}
	if opt.Limit > 0 && len(ids) > opt.Limit {
		ids = ids[:opt.Limit]
	}
	conc := opt.Concurrency
	if conc <= 0 {
		conc = 1
	}
	if conc > 4 {
		conc = 4
	}
	maxFail := opt.MaxConsecutiveFailures
	if maxFail <= 0 {
		maxFail = 5
	}
	reserve := opt.ReserveBytes
	if reserve == 0 {
		reserve = 500 << 20
	}
	p.Total = len(ids)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu      sync.Mutex
		consec  int
		stopErr error
		wg      sync.WaitGroup
		sem     = make(chan struct{}, conc)
		report  = func() {
			if progress != nil {
				progress(p)
			}
		}
	)
	stop := func(err error) { // mu held
		if stopErr == nil {
			stopErr = err
			cancel()
		}
	}
	entriesByID := map[string]string{}
	for _, e := range entries {
		entriesByID[e.id] = e.text
	}
	for _, id := range ids {
		text := entriesByID[id]
		select {
		case sem <- struct{}{}:
		case <-runCtx.Done():
		}
		if runCtx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(id, text string) {
			defer wg.Done()
			defer func() { <-sem }()
			hit, n, skipped, err := w.presynthOne(runCtx, id, text)
			mu.Lock()
			defer mu.Unlock()
			p.Done++
			switch {
			case skipped:
				p.Skipped++
			case err != nil:
				if runCtx.Err() != nil {
					p.Done-- // cut short by the stop or the caller, not a failure
					return
				}
				p.Failed++
				consec++
				if consec >= maxFail {
					stop(fmt.Errorf("presynth: %d consecutive provider failures (last: %s); the gateway may be down", consec, providerErrorKind(err)))
				}
			case hit:
				p.CacheHits++
				consec = 0
			default:
				p.Synthesized++
				p.Bytes += n
				consec = 0
				// The disk guard: after 20 syntheses, project the rest at the
				// mean size and refuse to eat into the reserve.
				if opt.FreeBytes != nil && p.Synthesized == 20 {
					if free, ferr := opt.FreeBytes(); ferr == nil {
						remaining := uint64(p.Total - p.Done)
						projected := remaining * uint64(p.Bytes/int64(p.Synthesized))
						if free < reserve || projected > free-reserve {
							stop(fmt.Errorf("%w: ~%d MB more needed, %d MB free, %d MB reserve", ErrPresynthDiskBudget, projected>>20, free>>20, reserve>>20))
						}
					}
				}
			}
			report()
		}(id, text)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if stopErr != nil {
		return p, stopErr
	}
	return p, ctx.Err()
}

// presynthOne is one line under the live commit path's key identity (the
// prewarmWorker's: same speaker resolution, canonical text, voice and
// backend identity), at background priority. skipped = no approved voice.
func (w *speechWiring) presynthOne(ctx context.Context, messageID, text string) (hit bool, n int64, skipped bool, err error) {
	// The voice a plain matched commit of this line resolves (ADR-0156): a
	// line on a shared speaker slot speaks the neutral voice unless the
	// adjudicator names its speaker, so pre-synthesis voices it that way too
	// and the commit hits the cache. messageID may be a page key
	// ("<id>#p<n>", ADR-0155); the slot view is keyed by the message.
	base, _, _ := strings.Cut(messageID, "#p")
	rv, _ := w.resolveVoiceForCharacter(ctx, w.commitSpeaker(commitPayload{MessageID: base}))
	if rv.profile.ID == "" {
		return false, 0, true, nil
	}
	key, err := w.lineCacheKey(rv, messageID, text)
	if err != nil {
		return false, 0, false, err
	}
	res, err := w.voices.Speech.GetOrGenerate(speech.WithKeyInput(ctx, messageID), key, speech.PriorityBackground, func(ctx context.Context) (tts.Audio, error) {
		return rv.generate(ctx, text)
	})
	if err != nil {
		return false, 0, false, err
	}
	return res.FromCache, int64(len(res.WAV)), false, nil
}
