# 05 — Runtime perception, matching, scene belief and predictive speech

## Runtime stages

`FrameSource → format/aspect normalizer → visual-change regions → textbox classification → OCR extraction → normalization and prefix aggregation → candidate retrieval → graph-aware scoring → optional Clef/VLM adjudication → commitment policy → speech scheduler`.

Keep originals for diagnostics with retention cap; process cropped ROIs for CPU efficiency. Local UNO Q capture runs FFmpeg/V4L2 with supervised subprocess lifecycle; prefer UVC MJPEG or YUYV depending on hardware. Detect fps/dropped frames, device unplug/reconnect and interlaced/scaled capture artifacts. No assumption that `/dev/video0` is correct.

## Dialogue observation state machine

States: `ABSENT`, `APPEARING`, `REVEALING`, `STABILIZING`, `READY`, `COMMITTED`, `ADVANCING`, `DISMISSED`, `ERROR`. Key transitions:

- `ABSENT→APPEARING`: region detector sees a plausible textbox for minimum consecutive frames.
- `APPEARING→REVEALING`: OCR or edge/pixel differences show increasing text content.
- `REVEALING→STABILIZING`: prefix unchanged for configurable window or animation-end cue appears.
- `STABILIZING→READY`: OCR confidence and/or graph-aware complete-text evidence sufficient; no active contradictory observations.
- `REVEALING→COMMITTED`: *only predictive mode* with unique strong prefix candidate, scene match and explicit risk policy.
- `READY→COMMITTED`: one atomic message occurrence committed and deduplicated.
- `COMMITTED→ADVANCING`: text changes materially within same box or game page changes; cancel previous speech generation and increment epoch.
- `ANY→DISMISSED`: region vanishes; policy fade/cancel; preserve recent graph context.

All transitions are driven by immutable observations into a serialized reducer; model responses are advisory messages accepted only when event epoch, profile revision and generation ID match.

## OCR pipeline

- Search bounding box using game-provided ROI(s) or runtime detector. Mask portraits, UI cursor, decorative borders and animated icon backgrounds.
- Preprocess: crop, deinterlace as needed, scale 2–4× with candidate filters, contrast/threshold adaptively, morphological cleanup, pad border; log chosen transform.
- Tesseract PSM 6 for uniform text blocks, PSM 7 for a single line, configurable language pack. Compare lightweight OCR against optional VLM fallback for difficult fonts; include both and discrepancy.
- Return OCR word boxes and confidence, normalized tokens and alternative glyph hypotheses, not just concatenated text.
- Use temporal voting between several adjacent frames; suppress duplicate OCR calls on unchanged region hash. A typewriter prefix is not a new distinct utterance.

## Text processing

Preserve raw observed OCR and normalized text separately. Normalize Unicode, whitespace, punctuation, line wrapping and known button symbols; substitute variable tokens using observed player name/locale when possible; use contextual candidate alignment for unknown icons without changing canonical source. Ignore low-confidence artifacts such as textbox edge cursors. Track per-page prefixes and their edit history.

## Local candidate retrieval

Candidate generation order: (1) exact normalized match, (2) unique full prefix (or predictive trie) and FTS, (3) trigram/inverted index, (4) RapidFuzz-equivalent token/character similarity within bounded top-K, (5) graph-neighbor expansion, (6) global fallback. Levenshtein alone is not adequate for short/repeated strings; compare weighted character/token similarities, OCR confusion allowances, scene, text kind, length, recent message context and speaker changes.

Score decomposition recorded per candidate: `text`, `prefix_coverage`, `scene`, `edge_prior`, `visibility`, `text_kind`, `speaker_context`, `edition`, `source_reliability`. Combined score through a tunable log-linear scorer or learned calibrator; human-corrected evaluation data calibrates acceptance. Expose top N and constituent scores in dev mode. Short strings and overlapping prefixes require more evidence; unique ID alone isn't enough if extraction version mismatches.

## Scene belief graph

Store a probability/belief distribution over last known message nodes or scene states, not just a single pointer. Observations update belief; soft graph transitions favor predicted successors but don't prevent jumping to a remote node. Belief decays or resets on scene change, pause, large time gaps, intentional menu opening or contradiction. Track previous `N=10` confirmed messages (configurable) and candidate graph path. Tests cover loops, branches, revisits, missed textbox, NPC repeated dialogue, item popups and emulator save/load.

## Clef adjudication and commit policy

Bundle related typed questions about one screenshot+context: `is_textbox` (noul), `text_kind` (choice), `speaker` (choice including unknown), `candidate_message` (choice including none), `is_complete` (noul), `safe_to_speak` (noul). Limit candidate options to supported request cap; include dynamic descriptions and deterministic candidate IDs. The model's probability distribution **is not assumed calibrated**; benchmark against truth labels and adjust thresholds per game/scene/text length. High-confidence correct speaker and speech eligibility both required before speaking as named actor.

If model confidence low: confirmed sign/item → narrator; confirmed character text but speaker unknown → configurable neutral/narrator or silent based on user preference; unclear whether text should be spoken → silence. Never invent dialogue to fill uncertainty. Fast LLM/VLM called only for unresolved OCR, unusual layout, scene transitions or speaker anomalies; can send screenshot crop plus recent context bounded by privacy policy.

## Predictive playback policy

- Default conservative full-line readiness, enable predictive mode explicitly.
- Predict only if a single complete canonical message or a very small agreeing set share identical `speech_text`, normalized observed prefix has sufficient nontrivial length and margin to runner-up, and edition/scene checks pass.
- Prefer cached candidate speech. If prefix turns out wrong before audible start, drop the audio; if already playing, fade and mark `prediction_misfire`, correct model and store fixture.
- Never speak unrevealed sensitive values or scripted branches from guesswork where player choice determines line.
- Use holdoff for snippets like “Yes.” or “No.” and low-specificity phrases, with stronger scene evidence.

## Utterance grouping and voice pacing

Use observed page boundaries and source punctuation to group or split sentences; do not generate on every OCR fragment. Precomputed full message uses canonical sentence context, but if multi-page utterance spans an advancing page, only speak what is already semantically committed unless player explicitly opted into ahead-of-page narration. Keep a `SpeechPlan` of ordered chunks, dependencies and cancellation. User progression always wins; never advance game automatically.

## Timings and benchmarks

Start with visual change detection 5–10Hz, OCR up to 2–5Hz *while relevant*, not guaranteed hardware throughput. Benchmark CPU/RSS/temp on UNO Q. Maintain timestamps: frame received, crop detected, OCR done, candidate ranked, decision request/answer, utterance committed, cache lookup, TTS request/response, audio started and cancelled. Derive p50/p95 distribution by game and failure type. Reject outlier tail latencies and memory growth for release; see test plan.
