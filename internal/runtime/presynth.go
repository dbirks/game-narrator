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

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
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
// A slot is judged shared from the script itself: a character that the
// profile's lines reach through more than one distinct speaker key (on the
// live board Aryll was linked from both `ww:spk:11` and `ww:spk:3`) is a
// curator's merge of slots, and a slot the curator merged onto one character
// is one that several speakers use. A character reached through a single
// speaker key — however many names and aliases it carries — is a
// single-speaker attribution and keeps its assignment. Speaker-link erases
// the per-line key, so a character two or more retired placeholders were
// folded into counts as a merge too (see below). (The first cut counted
// names plus aliases, so every slot-linked character, name + one alias, read
// as shared and the A/V harness's character spoke as the narrator.)
func sharedSpeakerSlots(content store.ProfileContent, resolveSpeaker func(string) string) map[string]bool {
	keysByChar := map[string]map[string]bool{}
	for _, m := range content.Messages {
		if m.SpeakerID == "" {
			continue
		}
		char := resolveSpeaker(m.SpeakerID)
		if char == "" {
			continue
		}
		if keysByChar[char] == nil {
			keysByChar[char] = map[string]bool{}
		}
		keysByChar[char][textnorm.Normalize(m.SpeakerID, textnorm.Options{})] = true
	}
	// Speaker-link (ADR-0070) re-points a placeholder's lines onto the target
	// and retires the placeholder, folding its name (the opaque slot key) into
	// the target's aliases — after a link the lines carry the target's id, so
	// the distinct-key count above sees one key. The folded placeholders are
	// the surviving record of the merge: a character two or more retired
	// placeholders were folded into is a slot merge too (on the board, Aryll
	// took ww:spk:11 and ww:spk:3 and voiced a boy's line).
	retired := map[string]bool{}
	for _, c := range content.Characters {
		if c.Status == games.CharacterRetired && c.Name != "" {
			retired[textnorm.Normalize(c.Name, textnorm.Options{})] = true
		}
	}
	mergedChar := map[string]bool{}
	for _, c := range content.Characters {
		if c.Status == games.CharacterRetired || len(c.Aliases) == 0 {
			continue
		}
		var aliases []string
		_ = json.Unmarshal(c.Aliases, &aliases)
		folded := 0
		for _, a := range aliases {
			if retired[textnorm.Normalize(a, textnorm.Options{})] {
				folded++
			}
		}
		if folded > 1 {
			mergedChar[c.ID] = true
		}
	}
	out := map[string]bool{}
	for _, m := range content.Messages {
		if m.SpeakerID == "" {
			continue
		}
		if char := resolveSpeaker(m.SpeakerID); len(keysByChar[char]) > 1 || mergedChar[char] {
			out[m.ID] = true
		}
	}
	return out
}

// kindSpeakerByKey is the ADR-0158 roster view of a published profile: the
// opaque apparent-kind keys — the adjudicator's kind answers carried through
// the "kind:" prefix (the kind option ids are the opaque keys' suffixes, the
// same grammar the speaker-link aliases use, ADR-0072) — onto the character
// whose aliases include them. A character that answers to a kind key is one
// the roster reached through its generic kind voice: the voice an unnamed
// speaker of that apparent kind speaks with (ADR-0158), so a shared BMG slot
// the adjudicator cannot name still voices an unnamed villager as the kind
// of person they are.
func kindSpeakerByKey(content store.ProfileContent) map[string]string {
	out := map[string]string{}
	for _, c := range content.Characters {
		if c.ID == "" {
			continue
		}
		var aliases []string
		if len(c.Aliases) > 0 {
			_ = json.Unmarshal(c.Aliases, &aliases)
		}
		for _, a := range aliases {
			if a == "" || !strings.HasPrefix(a, kindSpeakerKeyPrefix) {
				continue
			}
			k := kindSpeakerKeyPrefix + strings.TrimPrefix(a, kindSpeakerKeyPrefix)
			if _, taken := out[k]; !taken {
				out[k] = c.ID
			}
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
	// FailedSamples names up to presynthFailureSamples failed utterances
	// (message or page id plus the error), so a deterministic failure class
	// is diagnosable from the job result alone.
	FailedSamples []PresynthFailure `json:"failed_samples,omitempty"`
}

// PresynthFailure is one failed utterance in a run's sample.
type PresynthFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// presynthFailureSamples caps PresynthProgress.FailedSamples.
const presynthFailureSamples = 10

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
		// The ADR-0158 kind view: the opaque kind keys the adjudicator's kind
		// answers resolve through onto the characters that own the generic kind
		// voices.
		kindSpeakerByKey: kindSpeakerByKey(content),
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
				if len(p.FailedSamples) < presynthFailureSamples {
					p.FailedSamples = append(p.FailedSamples, PresynthFailure{ID: id, Error: err.Error()})
				}
				m.log.Warn("presynth: utterance failed", "id", id, "error", err)
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

// SpeechReachableKeys returns the cache keys the live speech path can produce
// for one published profile's current content: one per display page under the
// page's resolved voice (ADR-0155), the approved voice of the speaker's
// character else the narrator voice when the profile's narrator fallback is
// on (ADR-0101), computed under the same key identity the live commit, the
// successor prewarm and the pre-synthesis all use (ADR-0153/0156) — so a key
// the prune may keep is a key the next play of that profile can hit (ADR-0159,
// issue #49).
func (m *Manager) SpeechReachableKeys(ctx context.Context, profileID string) ([]speech.CacheKey, error) {
	if m.opts.Speech == nil || m.opts.Speech.Voices == nil {
		return nil, errors.New("speech cache prune: no speech pipeline is configured")
	}
	return speechReachableKeys(ctx, m.opts.Store, m.opts.Speech.Assets, m.opts.Speech.Voices, profileID)
}

// PruneSpeechCache removes the speech cache's transient entries that no
// current published profile revision can hit (ADR-0159, issue #49): the
// reachable key set is computed over every edition's currently published
// revision, and the cache's own entries outside that set are evicted through
// the reference-checked deletion (ADR-0041) — a blob still named by the
// catalogue (an approved voice reference, an audition clip, a character
// portrait or an imported source) keeps its bytes while its entry is
// dropped, and a cache with no reference checker evicts index entries only,
// safely.
//
// It returns the entries pruned and the bytes actually freed; with dryRun it
// reports the would-be prune without touching anything.
func PruneSpeechCache(ctx context.Context, st *store.Store, assets assets.Store, voiceSvc *voices.Service, cache *speech.Cache, dryRun bool) (int, int64, error) {
	if st == nil || cache == nil {
		return 0, 0, errors.New("speech cache prune: no store or cache to prune against")
	}
	if voiceSvc == nil {
		return 0, 0, errors.New("speech cache prune: no speech pipeline is configured")
	}
	reachable := map[string]bool{}
	published, err := publishedProfiles(ctx, st)
	if err != nil {
		return 0, 0, err
	}
	for _, prof := range published {
		keys, err := speechReachableKeys(ctx, st, assets, voiceSvc, prof.ID)
		if err != nil {
			return 0, 0, err
		}
		for _, k := range keys {
			reachable[string(k)] = true
		}
	}
	return cache.PruneUnreachable(ctx, func(key speech.CacheKey) bool { return reachable[string(key)] }, dryRun)
}

// publishedProfiles returns every edition's currently published revision: the
// profiles a live session could run, which is the keep set of the prune's
// reachability walk. Editions with nothing published are skipped (the live
// start refuses them; they hold no cache keys).
func publishedProfiles(ctx context.Context, st *store.Store) ([]games.Profile, error) {
	var out []games.Profile
	cursor := ""
	for {
		gamesList, next, err := st.ListGames(ctx, cursor, store.MaxListLimit)
		if err != nil {
			return nil, err
		}
		for _, g := range gamesList {
			editions, err := st.ListEditions(ctx, g.ID)
			if err != nil {
				return nil, err
			}
			for _, e := range editions {
				p, _, err := st.PublishedProfile(ctx, e.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						continue // nothing published for this edition
					}
					return nil, err
				}
				out = append(out, p)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// speechReachableKeys returns the cache keys the live speech path can produce
// for one profile's current content: one per display page under the page's
// resolved voice (ADR-0155), the approved voice of the speaker's character else
// the narrator voice when the profile's narrator fallback is on (ADR-0101), under
// the same key identity the live commit, the successor prewarm and the
// pre-synthesis all use (ADR-0153/0156). A line whose speaker resolves to no
// voice at all produces no key (the live path skips it, and a key the live
// path can never produce is not a key the cache may keep).
func speechReachableKeys(ctx context.Context, st *store.Store, assets assets.Store, voiceSvc *voices.Service, profileID string) ([]speech.CacheKey, error) {
	prof, _, err := st.GetProfile(ctx, profileID)
	if err != nil {
		return nil, err
	}
	content, err := st.GetProfileContent(ctx, profileID)
	if err != nil {
		return nil, err
	}
	return reachableKeys(ctx, st, assets, voiceSvc, content, prof.NarratorFallback)
}

// reachableKeys is speechReachableKeys' body over an already-loaded profile
// content: the per-page × per-voice walk.
func reachableKeys(ctx context.Context, st *store.Store, assets assets.Store, voiceSvc *voices.Service, content store.ProfileContent, narratorFallback bool) ([]speech.CacheKey, error) {
	speakers, aliasByID, texts, pages := profileSpeechMaps(content, speakerLinkResolver(content.Characters))
	slots := sharedSpeakerSlots(content, speakerLinkResolver(content.Characters))
	var keys []speech.CacheKey
	for _, msg := range content.Messages {
		msgPages := pages[msg.ID]
		if len(msgPages) > 1 {
			for i, pageText := range msgPages {
				lk, err := reachableKey(ctx, st, assets, voiceSvc, msg.ID, pageMessageID(msg.ID, i+1), pageText, speakers, slots, aliasByID, narratorFallback)
				if err != nil {
					return nil, err
				}
				keys = append(keys, lk...)
			}
			continue
		}
		if t := texts[msg.ID]; t != "" {
			lk, err := reachableKey(ctx, st, assets, voiceSvc, msg.ID, msg.ID, t, speakers, slots, aliasByID, narratorFallback)
			if err != nil {
				return nil, err
			}
			keys = append(keys, lk...)
		}
	}
	return keys, nil
}

// reachableKey computes one utterance's cache keys under every voice the
// live path could speak it with: the assigned character for a single-speaker
// line; for a shared slot the narrator, the linked character and each generic
// kind voice; none for a line no voice resolves. The utterance's identity is the bare
// message id for a single-box message, or the page-scoped "<message id>#p<n>"
// for one display page of a multi-box message — the page is part of the
// identity, so a later box of the same script message is a different utterance
// under its own key, not a duplicate of the message's first box (ADR-0155).
func reachableKey(ctx context.Context, st *store.Store, assets assets.Store, voiceSvc *voices.Service, messageID, id, text string, speakers map[string]string, slots map[string]bool, aliasByID map[string]string, narratorFallback bool) ([]speech.CacheKey, error) {
	base := messageID
	if i := strings.LastIndexByte(base, '#'); i >= 0 {
		base = base[:i]
	}
	candidates := []string{narratorCharacter}
	if s, ok := speakers[base]; ok && s != "" {
		if slots[base] {
			// A shared-slot line's voice is decided per occurrence (ADR-0156,
			// ADR-0158): the narrator, the linked character when Clef names
			// it, or a generic kind voice. Every one of them is a key the
			// live path can produce, so none of them is pruned.
			candidates = append(candidates, s)
			for _, k := range kindVoiceKinds {
				candidates = append(candidates, kindSpeakerKeyPrefix+k)
			}
		} else {
			candidates = []string{s}
		}
	}
	var keys []speech.CacheKey
	seen := map[string]bool{}
	for _, speaker := range candidates {
		rv, _ := resolveLineVoice(ctx, voiceSvc, assets, speaker, aliasByID, narratorFallback)
		if rv.profile.ID == "" || seen[rv.profile.ID] {
			continue // the live path skips voiceless lines: no key
		}
		seen[rv.profile.ID] = true
		key, err := lineKey(voiceSvc.BackendDigest, rv.profile.Language, rv.profile.Fingerprint, rv.profile.ModelID, id, text)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// PruneUnreachableSpeechCache removes the speech cache's transient entries
// that no current published profile revision can hit (ADR-0159, issue #49):
// the reachable key set is computed over every edition's currently published
// revision, and the cache's own entries outside that set are evicted through
// the reference-checked deletion (ADR-0041) — a blob still named by the
// catalogue (an approved voice reference, an audition clip, a character
// portrait or an imported source) keeps its bytes while its entry is
// dropped, and a cache with no reference checker evicts index entries only,
// safely.
//
// It returns the entries pruned and the bytes actually freed; with dryRun it
// reports the would-be prune without touching anything.
func (m *Manager) PruneUnreachableSpeechCache(ctx context.Context, dryRun bool) (int, int64, error) {
	if m.opts.Speech == nil || m.opts.Speech.Voices == nil || m.opts.Speech.Voices.Speech == nil {
		return 0, 0, errors.New("speech cache prune: no speech pipeline is configured")
	}
	return PruneSpeechCache(ctx, m.opts.Store, m.opts.Speech.Assets, m.opts.Speech.Voices, m.opts.Speech.Voices.Speech, dryRun)
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
	if !speakable(text) {
		return false, 0, true, nil // a silent beat: nothing to synthesize
	}
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
