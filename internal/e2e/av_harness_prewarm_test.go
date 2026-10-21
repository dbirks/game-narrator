// Package e2e — the A/V harness's warm-cache pre-synthesis (ADR 0143):
// before the live run, the fixture's first-dialogue lines are mapped onto
// the imported corpus and each mapped line's TTS texts are synthesised,
// through the real Base endpoint, under BOTH the scene character's and the
// narrator's voices, into the speech cache under the runtime's exact cache
// key. The live run then reads the bytes from the cache (from_cache=true)
// so the latency gate measures the session's own stages (box → OCR → commit
// → route → sink), not a TTS round trip. This is the harness's one
// deliberate deviation from a "cold" run: the ADR-0143 gate is a
// session-latency bound, and the TTS provider is not the stage under test.
//
// Log-safe per AGENTS.md rule 5: counts, ids and digests only — the line
// texts stay in the (gitignored) cache and the bundle, never the logs.
package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// avPrewarmVoice is one voice's cache-key source, resolved the way the
// session's speech wiring resolves it (internal/runtime/speech.go
// approvedVoiceForKey): the approved revision's fingerprint/model/language
// and, when the voice is registered on this instance, its registered name.
type avPrewarmVoice struct {
	profile store.VoiceProfile
	name    string // the checkpoint name the Base endpoint speaks
	ready   bool   // registered on this instance (the ready path)
}

// avResolvePrewarmVoice mirrors the session's approved-voice resolution for
// one character: the highest approved revision with a reference, and its
// registration on this instance. A character with no ready registration
// reports zero (the caller logs the deviation and the line is a cold miss
// live — the honest shape, not a fabricated warm entry).
func avResolvePrewarmVoice(t *testing.T, ctx context.Context, st *store.Store, svc *voices.Service, character string) avPrewarmVoice {
	t.Helper()
	profiles, err := st.ListVoiceProfiles(ctx, character)
	if err != nil {
		t.Fatalf("prewarm: ListVoiceProfiles(%s): %v", character, err)
	}
	for _, p := range profiles { // ordered by revision, descending
		if p.State != store.VoiceStateApproved || p.Fingerprint == "" || p.ReferenceAssetID == "" {
			continue
		}
		reg, err := st.GetVoiceRegistration(ctx, p.ID, svc.InstanceID)
		if err == nil && reg.State == store.VoiceRegReady && reg.RegisteredName != "" {
			return avPrewarmVoice{profile: p, name: reg.RegisteredName, ready: true}
		}
	}
	return avPrewarmVoice{}
}

// avPreSynthesize warms the cache for the fixture's first-dialogue lines and
// reports how many (voice, line-text) entries it inserted. The mapping is
// the bench's own rule (pcDominantKey): a folded-key exact match first, then
// the nearest corpus line within a bounded edit distance (the record-time
// reading and the corpus are two renderings of the same line), never a
// guess. Each mapped line's NormalizedText and SpeechText (the two TTS
// candidates; the runtime speaks the tracker's normalized reading, which is
// the corpus's normalized form for a clean box) are synthesised under both
// voices so whichever the session resolves has a warm entry.
func avPreSynthesize(t *testing.T, ctx context.Context, st *store.Store, svc *voices.Service, profileID, fdDir string, char, narr pcVoiceOutcome) int {
	t.Helper()
	prof := pcLoadFixtureProfile(t, fdDir)

	// The corpus side, from the imported store rows (the commit's message_id
	// is the store's id, so the key's OriginalMessageID must be it, not the
	// in-process parse's id).
	type corpusLine struct {
		key string
		msg *storeMessage
	}
	var lines []corpusLine
	exact := map[string]*storeMessage{}
	var all []games.Message
	cursor := ""
	for {
		page, next, err := st.ListMessages(ctx, profileID, store.MessageFilter{}, cursor, 500)
		if err != nil {
			t.Fatalf("prewarm: ListMessages: %v", err)
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	for i := range all {
		m := &all[i]
		k := pcScriptKey(m.OriginalText)
		if k == "" {
			continue
		}
		lines = append(lines, corpusLine{key: k, msg: &storeMessage{
			idx: i, id: m.ID, norm: strings.TrimSpace(m.NormalizedText), speech: strings.TrimSpace(m.SpeechText),
		}})
		if _, ok := exact[k]; !ok {
			exact[k] = &storeMessage{idx: i, id: m.ID, norm: strings.TrimSpace(m.NormalizedText), speech: strings.TrimSpace(m.SpeechText)}
		}
	}
	if len(all) == 0 {
		t.Fatalf("prewarm: the profile %s has no imported messages (the import must run before prewarm)", profileID)
	}

	// The two voices the session resolves: the scene speaker's and the
	// narrator's. Either may be unresolved (a logged cold-miss deviation).
	charSrc := avResolvePrewarmVoice(t, ctx, st, svc, char.Character)
	narrSrc := avResolvePrewarmVoice(t, ctx, st, svc, "narrator")
	if !charSrc.ready {
		t.Logf("prewarm: the character voice %s is not ready on this instance — its lines will be cold misses (a logged deviation)", char.VoiceID)
	}
	if !narrSrc.ready {
		t.Logf("prewarm: the narrator voice is not ready on this instance — unattributed lines will be cold misses (a logged deviation)")
	}
	srcs := []avPrewarmVoice{}
	if charSrc.ready {
		srcs = append(srcs, charSrc)
	}
	if narrSrc.ready {
		srcs = append(srcs, narrSrc)
	}
	if len(srcs) == 0 {
		t.Fatalf("prewarm: neither voice is ready on this instance — nothing to warm")
	}

	inserted := 0
	warmedIDs := map[string]bool{}
	var targets []*storeMessage
	for _, fix := range prof.Messages {
		k := pcScriptKey(fix.OriginalText)
		if k == "" {
			continue
		}
		var target *storeMessage
		if m, ok := exact[k]; ok {
			target = m
		} else {
			// Nearest within the bench's bound (the same rule pcDominantKey
			// uses to map a reading onto the script).
			tol := len(k) / 10
			if tol < 3 {
				tol = 3
			}
			var best *storeMessage
			bestD := -1
			for i := range lines {
				if len(lines[i].key) < len(k)-tol || len(lines[i].key) > len(k)+tol {
					continue
				}
				d := dolphinLevenshtein([]rune(k), []rune(lines[i].key))
				if d <= tol && (bestD < 0 || d < bestD) {
					best, bestD = lines[i].msg, d
				}
			}
			target = best
		}
		if target == nil {
			t.Logf("prewarm: fixture line did not map onto the corpus (a cold miss live; the reading and the import must share the lines)")
			continue
		}
		// Warm the mapped fixture line plus its contiguous conversation block
		// in corpus order (the first-dialogue fixture records the opening
		// pages; cuDriveConversation drives the rest of the same watchtower
		// conversation).
		for off := 0; off <= 6 && target.idx+off < len(all); off++ {
			m := &all[target.idx+off]
			if warmedIDs[m.ID] {
				continue
			}
			warmedIDs[m.ID] = true
			targets = append(targets, &storeMessage{
				idx: target.idx + off, id: m.ID,
				norm: strings.TrimSpace(m.NormalizedText), speech: strings.TrimSpace(m.SpeechText),
			})
		}
	}
	for _, target := range targets {
		// The line's TTS text candidates (deduped): the normalized reading
		// (the runtime's committed text) and the speech form.
		variants := []string{}
		for _, v := range []string{target.norm, target.speech} {
			if v == "" {
				continue
			}
			dup := false
			for _, x := range variants {
				if x == v {
					dup = true
					break
				}
			}
			if !dup {
				variants = append(variants, v)
			}
		}
		for _, v := range variants {
			for _, src := range srcs {
				if avPrewarmOne(t, ctx, svc, src, target.id, v) {
					inserted++
				}
			}
		}
	}
	t.Logf("prewarm: warmed %d (voice, line-text) cache entries across %d conversation lines (the live run's latency gate now measures the session, not TTS)", inserted, len(targets))
	return inserted
}

// storeMessage is the corpus row's prewarm-relevant fields (id + the two
// TTS text candidates).
type storeMessage struct {
	idx              int
	id, norm, speech string
}

// avPrewarmOne synthesises one (voice, line-text) entry under the runtime's
// exact cache key and stores it in the speech cache. It reports whether a
// fresh entry was written (false when the key already exists — a prior line
// warmed it — or on a synthesis failure, which is logged, not fatal: the
// line degrades to a cold miss live).
func avPrewarmOne(t *testing.T, ctx context.Context, svc *voices.Service, src avPrewarmVoice, messageID, text string) bool {
	t.Helper()
	key, err := speech.CacheKeyOf(speech.KeyInput{
		UtteranceText:            text,
		OriginalMessageID:        messageID,
		SpeechRulesRevision:      1,
		Language:                 src.profile.Language,
		VoiceRevisionFingerprint: src.profile.Fingerprint,
		TTSModelID:               src.profile.ModelID,
		BackendConfigDigest:      svc.BackendDigest,
		Format:                   "wav/pcm16",
	})
	if err != nil {
		t.Fatalf("prewarm: CacheKeyOf: %v", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := svc.Speech.GetOrGenerate(pctx, key, speech.PriorityCurrent, func(ctx context.Context) (tts.Audio, error) {
		return svc.Base.Generate(ctx, tts.GenerateRequest{Text: text, Voice: src.name, Language: src.profile.Language})
	})
	if err != nil {
		t.Logf("prewarm: synthesis failed for one entry (message %s, voice %s): %v — the line will be a cold miss live", messageID, src.profile.ID, err)
		return false
	}
	if res.FromCache {
		return false // already warm from a prior line
	}
	return true
}
