package windwaker

// Speaker attribution from the BMG INF1 per-message attribute bytes
// (ADR-0070).
//
// The decision is measurement-driven, not documentation-driven: on the user's
// own disc (counts and field indices only, no message text — AGENTS.md rule 5)
// the first attribute byte of every INF1 entry (the byte after the u32 text
// offset word)
//
//   - takes 50 distinct values over the 4,411 entries, with a long-tail
//     per-value distribution (the shape of a character roster);
//   - agrees with the next entry's value on 92.4% of adjacent pool-ordered
//     pairs against a 2.6% random baseline computed from its own distribution;
//   - forms 62 constant runs over the pool (the pool is grouped by speaker in
//     long blocks);
//   - is the near-determinant of the low-cardinality side bytes (the
//     per-speaker box/sound/camera presentation fields);
//   - collapses to 1–2 values across the messages of every Dolphin-captured
//     scene fixture, where random per-message values would give ~19;
//
// while every in-text control-tag family on the same disc carries a constant
// first argument and the sparse "voice" tag groups occur on 1.4% of messages
// only. That makes the attribute byte the dump's own line-to-speaker linkage,
// and the tag stream no speaker signal at all. ADR-0070 records the rest.
//
// The game's data provides no speaker *name* in any archive this import reads,
// so the mapping emits an opaque, stable key per line — never a guessed
// name — and the importer turns each distinct key into a proposed placeholder
// character the curator links (docs/spec/04 "never invent a speaker" is
// honoured: the row the import creates is identified by the id itself).

import (
	"encoding/json"
	"fmt"

	"github.com/dbirks/game-narrator/internal/games/bmg"
)

// SpeakerFieldIndex is the index, within the INF1 entry's attribute bytes
// (the bytes after the u32 text offset word), of the speaker slot.
const SpeakerFieldIndex = 0

// SpeakerColumn is the provenance label the adapter writes into
// ParseResult.SpeakerColumn and the attribution evidence: which source field
// a speaker attribution came from.
const SpeakerColumn = "inf1:attribute_0"

// SpeakerKeyPrefix namespaces the opaque speaker ids so a key can never be
// confused with a display name or a character id.
const SpeakerKeyPrefix = "ww:spk:"

// SpeakerKey renders one speaker-slot value as its stable opaque key. The
// key is a function of the raw byte only: a re-import of the same bytes yields
// the same keys, which is what the importer's reconcile-not-duplicate rule
// binds against.
func SpeakerKey(value byte) string {
	return fmt.Sprintf("%s%d", SpeakerKeyPrefix, int(value))
}

// speakerKeyOf maps one parsed message onto its speaker key, or "" when the
// entry carries no attribute byte (defensive: the layout this mapping was
// measured against always does, and an entry without the byte is exactly the
// case that must degrade to "no speaker" rather than a guess).
func speakerKeyOf(m bmg.Message) string {
	if len(m.Attributes) <= SpeakerFieldIndex {
		return ""
	}
	return SpeakerKey(m.Attributes[SpeakerFieldIndex])
}

// speakerEvidence is the messages.attribution_evidence of a source-field
// attribution: one array entry, the method that names the claim ("the dump's
// own linkage"), the field it read, the opaque key it read, and the source
// it read them from. The speaker id is an identifier, not a name, so the
// evidence carries the id verbatim and nothing the game says about a person.
func speakerEvidence(sourceID, key string) json.RawMessage {
	rec := map[string]any{
		"method":    "source_field",
		"field":     SpeakerColumn,
		"key":       key,
		"source_id": sourceID,
		"posterior": 1.0,
	}
	raw, err := json.Marshal([]map[string]any{rec})
	if err != nil {
		// A map of scalars cannot fail to marshal; the fallback keeps the row
		// importable with the schema's empty evidence rather than failing it.
		return json.RawMessage("[]")
	}
	return raw
}
