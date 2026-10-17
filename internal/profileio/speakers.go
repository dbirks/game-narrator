package profileio

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/store"
)

// Named speakers in a source file are source evidence, not a parser guess: the
// "never invent a speaker" rule (docs/spec/04 "Generic adapter minimum") covers
// rows without a label, while a speaker the user's own file names is a fact the
// importer may turn into a characters row. This file does that binding:
//
//   - every distinct label the parse reported is matched against the profile's
//     existing roster by the same textnorm key the parse computed, so a roster
//     entry and a file label that differ only in case or spacing are one
//     person;
//   - unmatched labels create a character (status confirmed: a verified source
//     row names them, and the source id is recorded in verified_source_refs);
//   - every message the source named is pointed at its character with
//     attribution confirmed at confidence 1.0 and the source column recorded as
//     evidence;
//   - every such message gets a speaker_assertion (method script_order,
//     posterior 1.0) so the audit trail separates the claim from the resolved
//     speaker, the way speaker_assertions exists to do (docs/spec/02).
//
// Idempotence is per-triple: a re-import of the same bytes finds the same
// characters, the same message ids (reconciled) and the same (message,
// character, method) assertions, and writes nothing new.

// speakerResolution is the outcome of binding one parse's labels to the
// profile's roster: the character rows to write, the key-to-character map the
// message linker uses, and the shared evidence payloads.
type speakerResolution struct {
	// characters are the rows to upsert: newly created ones, plus reused ones
	// whose aliases or verified_source_refs changed. Rows that already covered
	// every spelling the parse saw are not rewritten.
	characters []games.Character
	// byKey maps a parse key to the character id its messages point at.
	byKey map[string]string
	// evidence is the messages.attribution_evidence for every linked message:
	// one shared payload, since the whole file came from one source column.
	evidence json.RawMessage
	// assertionEvidence is the speaker_assertions.evidence for the same claim.
	assertionEvidence json.RawMessage
	// warnings are roster defects the import survived (an ambiguous match, a
	// name too long to store) and must report.
	warnings []adapter.Warning
	// created counts the characters this resolution wrote that did not exist
	// before; linked and linkedIDs are filled in by linkSpeakers.
	created   int
	linked    int
	linkedIDs map[string]bool
}

// resolveSpeakers matches parsed.Speakers against the profile's roster and
// prepares the rows that make the binding durable. It writes nothing: the
// caller owns the write order (characters before messages, messages before
// assertions — the foreign keys run that way).
func resolveSpeakers(ctx context.Context, s *store.Store, profileID string, norm textnorm.Options, parsed *adapter.ParseResult, sourceID string) (*speakerResolution, error) {
	res := &speakerResolution{byKey: map[string]string{}, linkedIDs: map[string]bool{}}
	if len(parsed.Speakers) == 0 {
		return res, nil
	}

	existing, err := s.ListCharacters(ctx, profileID)
	if err != nil {
		return nil, fmt.Errorf("profileio: list characters for speaker binding: %w", err)
	}

	// keyByChar maps a roster entry onto every key its name and aliases fold to,
	// so a label matches a character through any of its known spellings. Two
	// roster entries folding to one key is a curator contradiction the import
	// must not resolve silently: the first (by roster order) stands and the
	// conflict is reported.
	keyByChar := map[string]games.Character{}
	for _, c := range existing {
		keys := rosterKeys(c, norm)
		for k := range keys {
			if other, ok := keyByChar[k]; ok {
				res.warnings = append(res.warnings, adapter.Warning{
					Line: 0, Column: "speaker",
					Msg: fmt.Sprintf("characters %s (%q) and %s (%q) fold to the same speaker key %q; messages matching it bind to the first, so merge or rename one of the two",
						other.ID, other.Name, c.ID, c.Name, k),
				})
				continue
			}
			keyByChar[k] = c
		}
	}

	column := parsed.SpeakerColumn
	res.evidence = messageEvidence(sourceID, column)
	res.assertionEvidence = assertionEvidenceJSON(sourceID, column)

	for _, ref := range parsed.Speakers {
		spellings := append([]string{ref.DisplayName}, ref.Aliases...)

		if c, ok := keyByChar[ref.Key]; ok {
			// Reuse: the roster already names this person. New spellings the
			// file brought, and this source as a verified reference, are merged
			// in; a second import of the same bytes merges nothing, which is the
			// idempotence the reconcile-not-duplicate rule asks for.
			bind, changed := mergeCharacter(c, spellings, sourceID)
			res.byKey[ref.Key] = bind.ID
			if changed {
				res.characters = append(res.characters, bind)
			}
			continue
		}

		// Create. A name longer than the characters column holds cannot be
		// stored whole; the character is skipped and its messages keep the
		// parser's proposed attribution with the label preserved in flags,
		// which is what a curator can act on.
		if n := len([]rune(ref.DisplayName)); n > 200 {
			res.warnings = append(res.warnings, adapter.Warning{
				Line: 0, Column: "speaker",
				Msg: fmt.Sprintf("speaker name %q is %d characters, above the 200 the roster holds; no character was created and its messages stay unattributed",
					truncateRune(ref.DisplayName, 40), n),
			})
			continue
		}
		c := games.Character{
			ID:                 games.NewID(),
			ProfileID:          profileID,
			Name:               ref.DisplayName,
			Aliases:            spellingsJSON(ref.Aliases),
			VerifiedSourceRefs: json.RawMessage("[" + jsonString(sourceID) + "]"),
			Status:             games.CharacterConfirmed,
			CreatedAt:          nowStamp(),
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("profileio: speaker character for %q failed validation: %w", ref.DisplayName, err)
		}
		res.characters = append(res.characters, c)
		res.byKey[ref.Key] = c.ID
		res.created++
	}
	return res, nil
}

// rosterKeys is every key one roster entry answers to: its name and each
// alias, folded the way the importer folds file labels.
func rosterKeys(c games.Character, norm textnorm.Options) map[string]bool {
	keys := map[string]bool{}
	for _, s := range append([]string{c.Name}, jsonStringSlice(c.Aliases)...) {
		if k := textnorm.Normalize(s, norm); k != "" {
			keys[k] = true
		}
	}
	return keys
}

// mergeCharacter folds new spellings and a verified source reference into a
// reused roster entry. It reports whether anything changed, which decides
// whether the row is rewritten at all — a re-import that would rewrite every
// character row just to say "unchanged" would move the profile's row version
// for nothing.
func mergeCharacter(c games.Character, spellings []string, sourceID string) (games.Character, bool) {
	changed := false
	exact := map[string]bool{c.Name: true}
	aliases := append([]string(nil), jsonStringSlice(c.Aliases)...)
	for _, s := range spellings {
		if exact[s] {
			continue
		}
		exact[s] = true
		aliases = append(aliases, s)
		changed = true
	}
	var refs []string
	json.Unmarshal(c.VerifiedSourceRefs, &refs) // invalid JSON reads as nil: the column is CHECK json_valid, so that state cannot be stored
	if !containsString(refs, sourceID) {
		refs = append(refs, sourceID)
		changed = true
	}
	if !changed {
		return c, false
	}
	out := c
	out.Aliases = spellingsJSON(aliases)
	out.VerifiedSourceRefs = spellingsJSON(refs)
	return out, true
}

// linkSpeakers points every message the source named at its character and
// records the attribution the source column earns: confirmed, at full
// confidence, with the source and column as evidence.
//
// The domain's AttributionStatus vocabulary (internal/games/enums.go) has no
// "from source" value; the closest is "confirmed" — its definition is
// "evidence or a reviewer settled it", and the source file itself is the
// evidence (docs/spec/02 "Speaker attribution": a name the source records is
// stronger than a name printed on screen, which is only a proposal). Rows the
// source left unnamed are left exactly as the parser left them: unknown
// attribution, no speaker id, nothing invented.
func linkSpeakers(parsed *adapter.ParseResult, res *speakerResolution) {
	for i := range parsed.Messages {
		key := speakerKeyAt(parsed, i)
		cid, ok := res.byKey[key]
		if key == "" || !ok {
			continue
		}
		m := &parsed.Messages[i]
		m.SpeakerID = cid
		m.AttributionStatus = games.AttributionConfirmed
		m.AttributionConfidence = 1.0
		m.AttributionEvidence = res.evidence
		res.linked++
		res.linkedIDs[m.ID] = true
	}
}

// speakerKeyAt reads the sidecar without assuming the adapter filled it: an
// adapter that reports no speakers (or an old one) has no sidecar, and its
// messages keep whatever attribution the parse gave them.
func speakerKeyAt(p *adapter.ParseResult, i int) string {
	if i < len(p.SpeakerKeys) {
		return p.SpeakerKeys[i]
	}
	return ""
}

// writeSpeakerAssertions records one script_order assertion per linked message.
//
// The row id is derived from (message id, character id, method) rather than
// minted: a re-import recomputes the same id for the same claim, so the store's
// upsert rewrites the row instead of adding a copy. The (message, character,
// method) triples the profile already holds are skipped regardless of their id,
// which is what keeps a re-import into a *derived* revision safe — NewRevision
// copies the parent's assertions under fresh ids, so a derived triple would
// otherwise violate speaker_assertions' UNIQUE (message_id, character_id,
// method) index.
func writeSpeakerAssertions(ctx context.Context, deps Deps, profileID string, parsed *adapter.ParseResult, res *speakerResolution, report *ImportReport) error {
	if res.linked == 0 {
		return nil
	}
	existing, err := existingAssertionTriples(ctx, deps.Store, profileID)
	if err != nil {
		return err
	}
	now := nowStamp()
	var rows []games.SpeakerAssertion
	for i := range parsed.Messages {
		m := &parsed.Messages[i]
		// Only the messages this import linked: a message confirmed by an
		// earlier import of a different file, or by a curator, is not a
		// source-column claim of this file, and would gain a false provenance.
		if !res.linkedIDs[m.ID] {
			continue
		}
		method := games.AssertionScriptOrder
		if existing[m.ID+"\x00"+m.SpeakerID+"\x00"+string(method)] {
			continue
		}
		rows = append(rows, games.SpeakerAssertion{
			ID:          assertionID(m.ID, m.SpeakerID, method),
			ProfileID:   profileID,
			MessageID:   m.ID,
			CharacterID: m.SpeakerID,
			Method:      method,
			AssertedAt:  now,
			Evidence:    res.assertionEvidence,
			Posterior:   1.0,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	for start := 0; start < len(rows); start += ImportBatchSize {
		end := min(start+ImportBatchSize, len(rows))
		if err := deps.Store.PutSpeakerAssertions(ctx, profileID, rows[start:end]); err != nil {
			return fmt.Errorf("profileio: write speaker assertions %d-%d: %w", start, end-1, err)
		}
	}
	return nil
}

// existingAssertionTriples indexes the profile's speaker assertions by the
// triple the unique index enforces, so the writer can skip a claim the
// profile already holds under any id.
func existingAssertionTriples(ctx context.Context, s *store.Store, profileID string) (map[string]bool, error) {
	rows, err := s.ListSpeakerAssertions(ctx, profileID)
	if err != nil {
		return nil, fmt.Errorf("profileio: list speaker assertions: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, a := range rows {
		out[a.MessageID+"\x00"+a.CharacterID+"\x00"+string(a.Method)] = true
	}
	return out, nil
}

// assertionID renders the (message, character, method) triple as the 26-
// character identifier the schema requires. It is a content hash, not a
// timestamp: the id must be the same on every re-import, which a minted ULID
// cannot be. The top two bits are zeroed so the first character's value stays
// in the 0-7 range games.ValidID requires of an id's timestamp position.
func assertionID(messageID, characterID string, method games.AssertionMethod) string {
	h := sha256.Sum256([]byte(messageID + "\x00" + characterID + "\x00" + string(method)))
	var buf [17]byte // 136 bits; only the low 130 (26 chars) are used
	copy(buf[:], h[:])
	buf[0] &= 0x3f
	var out [26]byte
	for i := range out {
		var field byte
		for bit := 0; bit < 5; bit++ {
			pos := i*5 + bit
			byteIdx := pos / 8
			bitIdx := 7 - (pos % 8)
			if buf[byteIdx]&(1<<uint(bitIdx)) != 0 {
				field |= 1 << uint(4-bit)
			}
		}
		out[i] = crockford[field]
	}
	return string(out[:])
}

// crockford repeats the alphabet internal/games/ids.go uses so a derived id is
// a canonical id. It is restated here rather than imported because games keeps
// it private, and a derived id that failed games.ValidID would surface as a
// confusing validation error at write time.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// messageEvidence is the messages.attribution_evidence of a source-column
// attribution. The column requires a JSON *array* of evidence entries, so the
// record the importer writes is the one-object form of it ({"method":
// "source_column", ...}) rather than a bare object.
func messageEvidence(sourceID, column string) json.RawMessage {
	rec := map[string]string{"method": "source_column", "source_id": sourceID}
	if column != "" {
		rec["column"] = column
	}
	return oneElementJSON(rec)
}

// assertionEvidenceJSON is the speaker_assertions.evidence of the same claim:
// the schema documents the array as {kind, ref, excerpt} entries.
func assertionEvidenceJSON(sourceID, column string) json.RawMessage {
	rec := map[string]string{"kind": "source_column", "ref": sourceID}
	if column != "" {
		rec["excerpt"] = "column " + column
	}
	return oneElementJSON(rec)
}

func oneElementJSON(rec map[string]string) json.RawMessage {
	raw, err := json.Marshal([]map[string]string{rec})
	if err != nil {
		// A map of strings cannot fail to marshal; the fallback keeps the
		// import alive with the schema's own empty value rather than crashing.
		return json.RawMessage("[]")
	}
	return raw
}

//
// ── JSON string helpers ─────────────────────────────────────────────────────
//

// jsonStringSlice reads a character aliases / verified_source_refs column as a
// list of strings. The columns are CHECK json_valid with a [] default, so the
// only shapes that reach here are an array of strings or, from hand-written
// rows, something else: that reads as empty, never an error, because speaker
// binding must survive a messy roster.
func jsonStringSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func spellingsJSON(spellings []string) json.RawMessage {
	if len(spellings) == 0 {
		return json.RawMessage("[]")
	}
	raw, err := json.Marshal(spellings)
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func truncateRune(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
