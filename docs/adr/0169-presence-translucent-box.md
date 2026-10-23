# ADR 0169: The open-sea/sky clause — a translucent box band is present without its arrow; the open sea behind it is not

- Status: accepted (implemented)
- Date: 2026-10-23
- Issue: #44 (follow-up to ADR-0137's sea amendment)

## Context and constraints

A translucent dialogue box over open sea was reported to read absent the
moment its advance arrow disappeared. The advance arrow is a marker the box
happens to carry, not the box: the panel and the line are still on screen, so
the presence gate must say the band is present with or without the arrow.

A 2026-10-23 probe ran both detectors on four private frames (three box frames
— with arrow, without arrow, bottom close-up — and one no-box open-sea/sky
scene) through the standard board-band crop (`0.10, 0.62, 0.80, 0.38`). The
measured pipeline `PanelPresence` on the crop:

| Frame | Dark | Text | SaturatedDark | TopDark | Pipeline gate | Dolphin `BoxPresent` |
|---|---|---|---|---|---|---|
| box-with-arrow | 0.9339 | 0.014520 | 0.0007 | 1.000 | present | present (class=box) |
| box-no-arrow | 0.9394 | 0.014466 | 0.0000 | 1.000 | present | present (class=box) |
| box-bottom-closeup | 0.9830 | 0.004153 | 0.0034 | 1.000 | present | present (class=box) |
| no-box open sea | 0.7311 | 0.002221 | 0.0276 | 0.062 | **present (bug)** | absent (class=continue) |

The feature that fails is the pipeline presence gate: it reads the 2026-10-23
open-sea/sky band as present. ADR-0137's amendment (2026-10-21) closed the
DARK saturated sea — its dark pixels read SaturatedDark 0.86–0.97 and were
rejected by the `MaxSaturatedDarkShare` (0.5) ceiling. This sea is a different
shape: its water is only weakly saturated (0.0276, far under the 0.5 ceiling)
and its top of the crop is the bright sky. Every other clause passes on it —
Dark 0.7311 clears the 0.30 floor, Text 0.002221 clears the 0.0002 floor, and
SaturatedDark 0.0276 clears the 0.5 ceiling — so nothing rejects it and the
gate calls the sea a box.

The box crops, by contrast, are dark at the top of their region (TopDark
1.000): a translucent panel reaches the top edge of the band crop regardless
of the text that rides lower down. The no-tracker sign (a desaturated panel
whose top of the crop is bright, reported as still present on the same board)
is the counter-example that bounds the fix: a bright top alone is not the sea —
the sign carries no saturation, so the sea clause must require a little
saturation, not just a bright top.

## Decision

1. **Add a top-band feature to the presence read.** `vision.PanelPresence`
   gains `TopDark` — the mean dark-pixel share of the crop's top ~1/8 of rows
   (`vision.topBandDark`). A textbox region is a dark band that reaches the top
   edge of the crop, so a box reads TopDark ≈ 1; an open-sea/sky band is bright
   at the top and reads TopDark ≈ 0.
2. **Add the open-sea/sky clause to the gate.** `PresenceConfig.Present`
   rejects the band when it is a weakly saturated scene with a bright top:
   `SaturatedDark > MinSeaSat AND TopDark < MinTopDark`. A box never has a
   bright top (its panel reaches the top), so the clause cannot fire for a box;
   a desaturated band with a bright top (the no-tracker sign) never has the
   saturation, so the clause cannot fire for it either.
3. **Thresholds sit between the measured shapes.** `MinSeaSat = 0.02` — the
   sea reads 0.0276 above it and every box reads ≤ 0.0034 below it, while the
   captured box fixtures (up to 0.053 SaturatedDark) are cleared by the
   `MinTopDark` half, not by this floor. `MinTopDark = 0.5` — the sea reads
   0.062 below it and every box reads ≥ 0.78 above it. Both are unset-friendly
   (zero takes the default, matching every other presence field).

## Options considered

1. **Raise the dark-saturated ceiling (`MaxSaturatedDarkShare`).** The sea's
   SaturatedDark is 0.0276, far below the box fixtures' up to 0.053 — the
   ceiling cannot separate them without silencing real boxes. Rejected.
2. **A stand-alone TopDark floor** (reject whenever TopDark < 0.5). Rejected:
   it rejects the no-tracker sign — a desaturated panel whose top of the crop
   is bright that the same board reports as still present (TopDark 0.000).
3. **A stand-alone SaturatedDark floor** (reject whenever SaturatedDark >
   0.02). Rejected: the captured box fixtures read up to 0.053 SaturatedDark; a
   floor at 0.02 would drop them. The floor is only safe because the box also
   has a dark top, which is exactly the conjunction the clause adds.
4. **Tighten `MinRowDark` / `OnPanel` / `PanelRows`.** Each attempt broke a box
   fixture (the black-water box reads PanelRows 0.758; the wide-text HDMI box
   reads MinRowDark 0.098). The sea's failure is not in those features;
   rejected.

The conjunction is the smallest change that separates the measured sea
(weakly saturated, bright top) from every measured box and the no-tracker sign
(dark top, or no saturation).

## Consequences

- The open-sea/sky band the 2026-10-23 probe exposed reads absent; the
  translucent box reads present with or without its advance arrow; and the
  no-tracker sign (bright top, no saturation) stays present.
- The clause is a pure pixel predicate on the crop, so it is deterministic and
  unit-reproducible without a provider — the committed synthetic corpus
  (`presence_synthetic_test.go`) pins all five shapes (box without arrow
  present; bright sea gradient absent by the dark clause; open sea with
  sparkle absent by the sea clause; desaturated bright-top sign present; dark
  flat band absent by the text floor) without any game content.
- The `sat8` quirk is noted, not changed: it measures channel spread as
  `max(r,g)−min(r,g,b)` (the blue channel counts toward the minimum). The
  ADR-0137 dark-sea measurement stands on that arithmetic, so ADR-0169 leaves
  it in place and documents it on the `SaturatedDark` field.
- The one new feature (`TopDark`) is computed in a single luma pass over the
  top ~1/8 of the crop's rows, so the per-frame cost is negligible against the
  walk the read already runs.

## Residual risk

- The sea's SaturatedDark margin is thin (0.0276 vs the 0.02 floor, ~27%
  above). The clause's other half — TopDark 0.062 vs the 0.5 floor — is the
  wide margin, and a box's TopDark (≥ 0.78) keeps the clause from firing for
  any box even if a capture card reports more sea saturation. A sea that is
  weakly saturated AND dark at the top of the crop (TopDark ≥ 0.5) would still
  read present; none of the measured seas has that shape, and the probe test
  (`GN_PRESENCE_FRAMES`) is the standing check if one appears.
- The dolphin `BoxPresent` classifier is independent of this gate (it uses the
  screen signature, not the panel features) and already reads the no-box frame
  absent (class=continue); no dolphin change is required.

## Validation

- Probe on the four private frames: `GN_PRESENCE_FRAMES=<dir> go test -count=1
  -run PresenceProbeFrames -v ./internal/pipeline/ ./internal/dolphin/` — all
  four verdicts match (three boxes present, the sea absent on both detectors).
- `go test -race -count=1 -timeout 20m ./internal/pipeline/ ./internal/dolphin/`
- `systemd-run --user --scope -q -p MemoryMax=4G make test-replay` (junit 147
  PASS)
- `make lint`

## Reversal plan

Set `MinSeaSat: 0` in the profile's presence config (zero is unset, so the
clause's saturation half can never fire) — or set `MinTopDark: 0` to disable the
bright-top half. The gate returns to the ADR-0137 triple gate and the sea false
positive returns; the `TopDark` feature is left in place, unused, for a tighter
re-tune.

## Links / references

- ADR-0137 (the textbox presence gate; Amendment: the 2026-10-21 dark-sea
  false positive, #41/#44), ADR-0115 (escalation), docs/spec/05 (Clef
  adjudication and commit policy), docs/spec/13 (observability and resilience).
- `internal/vision/textpresence.go` (`PanelPresence.TopDark`, `topBandDark`),
  `internal/pipeline/config.go` (`MinSeaSat`, `MinTopDark`, `Present`),
  `internal/pipeline/presence_synthetic_test.go` (committed synthetic corpus),
  `internal/pipeline/presence_probe_frames_test.go` (private-frame probe, gated
  on `GN_PRESENCE_FRAMES`).
