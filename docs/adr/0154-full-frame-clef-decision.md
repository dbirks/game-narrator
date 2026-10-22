# ADR 0154: Clef decides from the full frame, so the box can be anywhere

- Status: accepted (step 1 implemented; steps 2–3 planned)
- Date: 2026-10-22
- Issue: #41

## Context

Wind Waker draws its dialogue box at the top, middle or bottom of the picture. On the UNO Q's capture card the top box sits at y ≈ 0.18–0.33, higher than the Dolphin-calibrated "high" region (0.25–0.62). On 2026-10-22 the owner played a top-box conversation and nothing was spoken. The region crop caught only the second line, plus scenery, and the fixed placement candidates (ADR 0137: top of frame, the calibrated y, bottom of frame) cut the box in half.

The owner asked the direct question: why crop at all, instead of giving Clef the whole picture?

The System One protocol accepts up to 4 images per request, and `BuildAdjudication`'s questions already say "in this screenshot", yet the runtime never attached one. A measured probe (2026-10-22, clef-omni through the collective gateway, **no OCR text in the request**) asked "is there a dialogue box?" and "which of these lines is it, or none?":

| Frame | Options | Result |
|---|---|---|
| capture-card top box | 15, 6 sharing words with the truth | right line, conf 0.72; box yes 0.93 |
| Dolphin bottom box | 15 | right line, conf 0.70; box yes 0.96 |
| capture-card sailing, no box | 15 | "none", conf 0.62; box no 0.98 |

All 6 answers were correct across two runs. Latency was about 0.6–0.95 s warm and about 4 s cold.

## Decision

1. **Every adjudication carries the frame** (implemented).
   - `pipeline.DecisionRequest.Frame` is the newest immutable frame (`Runner.latestImage`, the live view's slot).
   - The runtime's decision wiring JPEG-encodes it (quality 85; downscaled when wider than 960 px; a 640×480 capture is about 30–40 KB) into `bundle.Images`.
   - With no frame, the request stays text-only, as before.
2. **A box anywhere asks Clef with the picture** (planned).
   - When any band of the frame reads as a dialogue panel (the presence gate on a full-height scan, not only the profile's rects), but the local reading abstains or is implausible, the runtime asks Clef with the frame.
   - The candidates are the dialogue graph's successors of the last committed line, plus the matcher's fragment shortlist.
   - Clef's "none" is silence. Its pick commits with its speaker.
3. **Adaptive crop for the local path** (planned).
   - The placement candidates become a scan: bands every ~5% of the frame height, scored by the existing box-likeness. The local OCR then reads the box where it actually is.
   - This keeps narration working, faster and offline, when Clef or the gateway is slow or down.

Clef stays a chooser among candidates, not a free-text reader. The candidates come from the local index and the dialogue graph, so a line Clef names is always a script line with a speaker and a cached voice.

## Tradeoffs

- **Payload:** each decision uploads about 35 KB more, which is small next to the ~0.6 s round trip.
- **Gateway dependence:** step 1 only enriches a call that already happens. Steps 2–3 keep the local path, so an outage degrades narration to OCR plus the cache rather than to silence.
- **Probe size:** the probe used 3 frames. A bigger labelled set (the A/V harness frames, the board's retained trace frames once #46 lands) should gate the "none" threshold before step 2 commits without OCR.

## Validation

- `TestDecisionAdjudicatesAmbiguousCommit` asserts that every request carries exactly one JPEG data URL. It fails with the attach disabled.
- The probe above, with gateway request ids `06GP2M8…` and `06GP2M9…` in the session log.
- Live: after step 2, `make test-av` and a board session with top-box dialogue.

## Reversal

Drop `Frame` from `DecisionRequest`, or skip the attach in `Decide`. The request then returns to the text-only shape.
