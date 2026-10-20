# ADR-0103: the fixture content-kind mark (per-message text_kind on a captured fixture)

## Status

Accepted (2026-10-20, G8 item recording; implements the unblock recipe step 5
already documented in `docs/status/blocked.md`, row "Dolphin sign savestate").

## Context

The G8 item/sign gate needs a **captured** scenario whose message commits
with the corpus's content class — `item` for the Dolphin item recording
(the Telescope, from Aryll's lookout), `sign` later. ADR-0089 gave the
committed scenario loader the per-message `text_kind` (a message's content
class with the profile level as default) and the pipeline the region-role
hint (`games.RegionType.TextKindHint`: an item region's readings rank with
the item class). The synthetic half of the gate (GN-T010/GN-T011 S mode)
runs green on that path.

The captured half was impossible with the M6 fixture recorder
(`internal/dolphin`): `writeProfile` emitted a profile-level
`text_kind: "dialogue"` and a box region typed `textbox` — every line of a
captured fixture was dialogue by construction, and there was no way to say
"this box line is the item popup the recording captured". The recorder's
finalised fixture is the honest artifact (AGENTS.md rule 5: the game text
lives only in the gitignored fixture), so the mark had to live in the
recorder and the manifest, not in a hand-edited fixture file.

## Decision

1. **The mark is set by the recording drive, resolved at Finalize.** The
   drive calls `FixtureRecorder.SetContentKind(kind)` (the ADR-0089
   vocabulary, validated against `games.ParseTextKind` — a typo is a
   failure, not a silent mislabel) and `MarkContentFrame(idx)`: the index
   of the first recorded frame that observed the content box. Finalize
   resolves the frame to the box line its reveal run canonicalises
   (`canonicalizeRuns` membership — the same run the truth/profile already
   built from) and folds the result into the manifest and the profile. A
   mark that cannot resolve (frame outside a text-bearing box run) is a
   Finalize **error**: the mark would name a line the recording never
   showed, and a mislabeled fixture is worse than a failed recording.
2. **The manifest gains an optional `content` block**
   (`{"kind": "item", "frame": N}`): the kind plus the first frame that
   observed the content box. Schema stays 2 (an optional field — the
   versioned gate already accepts schema 1 and 2; legacy fixtures verify
   unchanged, the block simply absent). The frame index is the correlation
   key into the manifest's own frame order (no game text in the block —
   the line's id and reading are the recorder's own).
3. **The profile gains per-message `text_kind` for every line** (the
   content line carries the kind; every other line carries `dialogue`) and
   the box region's type becomes the content kind (the region is typed
   `item`, so `regionKindHint` ranks its readings with the class and the
   harness replays against it as Regions[0]). The profile-level
   `text_kind` stays `dialogue`: a pre-ADR-0089 reader (profile level only)
   reads exactly what it read before, and the per-message override is what
   the committed loader (ADR-0089) already consumes.
4. **The re-OCR recipe revision carries the mark** (TestRealReOCRFixture
   re-finalises the OCR layer over a recorded fixture): it re-resolves the
   recorded `content.frame` against the re-canonicalised box runs and
   re-emits the same per-message kinds and region role — a recipe revision
   cannot drop the mark.

## Options considered

- **Hand-edit the fixture's profile.json.** Rejected: the profile is the
  recorder's artifact (its description says so); a manual edit diverges
  from the re-OCR path and from a re-recording, and puts a human in the
  middle of the hash-pinned chain (the scenario pins the manifest SHA;
  the profile must be what the manifest's recipe reproduces).
- **Mark by text (the line's reading).** Rejected: the mark would carry
  game text into the drive's code path (AGENTS.md rule 5); the frame
  index + run resolution names the same line without the text.
- **A separate "item banner" region instead of re-typing the box.**
  Rejected for the box-line case: the record-time OCR reads the item
  line from the box's adaptive crop (the truth row's text), so the region
  that owns the reading is the box region; a second region would own no
  truth text (the truth engine answers the primary region with the row's
  text and the others empty). The box's on-screen role **is** the content
  role in a content recording, which is exactly what the region's type
  expresses.

## Validation

- Unit: `record_content_test.go` (mark validation, resolution errors,
  per-message kinds, the region's role, the manifest block's JSON shape
  and round trip, the `FramesPublished` index contract the mark relies
  on); the existing recorder unit suite still passes (the unmarked path is
  byte-identical in effect: profile-level `dialogue`, box typed
  `textbox`, no content block).
- Live: `TestRealRecordTelescopeFixture` (`GN_GATE=test-dolphin`) records
  the fixture and logs the manifest SHA the scenario pins; the replay is
  `dolphin/item-telescope` under the same gate (`committed_kind` = item,
  one commit, the narrator voice, no substitution).

## Consequences

- Captured fixtures can now state a content class per line; the sign
  recording reuses the same mark (kind `sign`) with no further recorder
  change.
- The drive must name the kind before marking; a mark without a kind is a
  load-time error (SetContentKind validates against the vocabulary).
- A re-recorded fixture gets a new manifest SHA (the scenario re-pins);
  the mark rides along in the new manifest.

## Reversal plan

Drop the `content` field from `FixtureManifest` (readers ignore unknown
fields; the profile's per-message `text_kind` is likewise optional to
readers), revert `writeProfile` to the profile-level kind, and delete the
drive's Set/Mark calls. Legacy fixtures verify unchanged by the versioned
gate; a marked fixture simply loses its per-message kinds (it reads as an
all-dialogue fixture, the pre-ADR shape).
