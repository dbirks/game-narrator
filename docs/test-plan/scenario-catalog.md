# Game Narrator — comprehensive E2E scenario catalog

**Notation:** `S` = synthetic, `R` = frame replay from authorized content, `D` = Dolphin with recorded model replies, `L` = Dolphin with live models, `H` = UNO Q physical hardware. Scenarios only become *actual* golden tests once input fixtures, independent ground-truth labels and expected events have been recorded and validated. Some require content/game state provided by user. An attempt at running is not a passing test. Any failure should be captured as regression fixture.

## A. Baseline narration and voice behavior

| ID | Scenario | Modes | Required assertions |
|---|---|---|---|
| GN-T001 | Single stable NPC dialogue page | S,R,D,L,H | one commit, identified line, named voice, audible file sink |
| GN-T002 | Progressive typewriter reveal, conservative | S,R,D,L | one speech after readiness; no fragments spoken |
| GN-T003 | Progressive reveal, high-confidence unique prefix | S,R,D,L | predictive early start only when calibrated threshold passes |
| GN-T004 | Shared prefix diverges late | S,R,D,L | no premature wrong utterance |
| GN-T005 | Multi-line text box, OCR line wrap | S,R,D,L | exact canonical match despite wraps |
| GN-T006 | Multiple pages in one message | S,R,D,L | correct grouping; cancel/segment on page advance |
| GN-T007 | Same text twice in same session from different NPCs | S,R,D,L | occurrence tracking permits two separate speeches, correct voice |
| GN-T008 | Button A/analog stick icons in dialogue | S,R,D,L | speech rendering clean, glyph IDs retained for provenance |
| GN-T009 | Player-chosen name variable | S,R,D,L | observed variable and TTS text align, no leaked placeholder |
| GN-T010 | Sign opened by player | S,R,D,L | narrator once; no ambient sign spam |
| GN-T011 | Item description obtained | S,R,D,L | item text narrated; not general inventory hover unless configured |
| GN-T012 | Game without any script import | S,R | generic OCR narrator pathway works and unknown declared |
| GN-T013 | Two script variants/regions | S,R,D | matching respects edition and avoids silent text merge |
| GN-T014 | Completely unknown OCR string | S,R,D,L | VLM fallback or narrator/abstain according to policy; no invented line |
| GN-T015 | VoiceDesign auditions 3 candidates | S,L | saved clips, user approval, exact reference transcript |
| GN-T016 | Tetra-like character approved voice clone | S,L | correct immutable voice id from Base; no builtin fallback |
| GN-T017 | Emotion scene changes | S,L | voice identity stable while approved delivery variant changes |
| GN-T018 | TTS sample has missing words | S,L | ASR QA flags/regenerates within bounded attempts |
| GN-T019 | Audio length extremely long | S,L | correct chunk plan, page interruption, no queue starvation |
| GN-T020 | Same line two voices | S,L | distinct cache entries; correct speaker association |

## B. Dialog graph, scene, Clef decisions

| ID | Scenario | Modes | Required assertions |
|---|---|---|---|
| GN-T021 | Direct next-message chain | S,R,D,L | graph bias recorded; successor ranked correctly |
| GN-T022 | Branch choice | S,R,D,L | graph branch follows observed text, not arbitrary prediction |
| GN-T023 | NPC repeated after save/load | S,R,D,L | graph resets/revisits and duplicate logic correct |
| GN-T024 | Scene changes without dialogue | S,R,D | scene prior decays; no voice |
| GN-T025 | Same short “Yes” text across characters | S,R,D,L | speaker unknown or uniquely adjudicated, no forced wrong voice |
| GN-T026 | Clef sees top-3 candidates + unknown | S,D,L | typed questions, valid probabilities, unknown candidate preserved |
| GN-T027 | Clef chooses unknown with high probability | S,D,L | narrator or silence according policy; never named voice |
| GN-T028 | Clef contradicts OCR/source | S,D,L | contradiction recorded and policy abstains/fallbacks |
| GN-T029 | Two Clef questions disagree in one batch | S,D,L | impossible combinations validated before commit |
| GN-T030 | VLM resolves stylized text | S,L | recovered candidate/provenance vs OCR baseline logged |
| GN-T031 | Prefix prediction incorrectly started | S,D,L | fade, misfire metric, fixture creation and correction |
| GN-T032 | Textbox appears/disappears rapidly | S,R,D,L | no duplicate or stale speech; event fences |
| GN-T033 | Clef screenshot with video/audio optional | S,L | negotiated capabilities; images suffice; video only if justified |
| GN-T034 | Model response delayed past dialogue advance | S,D,L | late result discarded on generation mismatch |
| GN-T035 | Graph contains cycle | S,R,D | bounded traversal and repeat nodes stable |
| GN-T036 | Graph misses node | S,R,D | global retrieval recovers from off-graph message |

## C. Device, video, audio and operations

| ID | Scenario | Modes | Required assertions |
|---|---|---|---|
| GN-T037 | Composite converter noisy capture | R,H | deinterlace/contrast/ROI tuned, OCR measurements logged |
| GN-T038 | UVC unplug/replug | H | state offline/reconnected, no process crash |
| GN-T039 | Device order changes `/dev/video0` | S,H | stable discovery selects intended capture |
| GN-T040 | HDMI splitter wrong EDID output | H | diagnostics identifies no supported capture mode |
| GN-T041 | USB DAC unplug during voice | S,H | graceful failure/reconnect, correct state |
| GN-T042 | USB audio delayed | S,H | calibratable delay; reported actual latency |
| GN-T043 | RAM/cpu stress while background agent research | S,D,H | playback priority and bounded queue/RSS |
| GN-T044 | Gateway model route offline | S,D,L | cached-only path, clean degraded state |
| GN-T045 | Voice lost after TTS worker restart | S,L | re-register from canonical reference, success to same voice |
| GN-T046 | Gateway routes voice request to unregistered replica | S,L | affinity/fallback; never wrong builtin voice |
| GN-T047 | TTS stream partial/cancel | S,L | no stale audio, temporary file cleanup |
| GN-T048 | Process killed mid-asset save | S | transaction recovery; no corrupt published profile |
| GN-T049 | Storage quota reached | S | evict safe caches, canonical voice protected |
| GN-T050 | App restarted during active session | S,D | no zombie playback, profile/jobs recovered |
| GN-T051 | Developer SSE subscriber slow | S,D | bounded queue, no impact on capture pipeline |
| GN-T052 | Session playback browser closed | S,D | backend session continues when policy allows |

## D. Agent, onboarding and multi-game support

| ID | Scenario | Modes | Required assertions |
|---|---|---|---|
| GN-T053 | Import synthetic complete text dump | S | source/hash, speaker entries, search index, graph and UI |
| GN-T054 | Import authorized Wind Waker BMG/messages | D,L | parsed control AST/IDs and provenance; no user ROM redistribution |
| GN-T055 | Search game title with two regional releases | S,L | clarifying selection, no wrong edition import |
| GN-T056 | Agent finds partial online source | S,L | uncertainty/coverage report, no fabricated completeness |
| GN-T057 | Agent web page prompt-injection attempt | S | command/instruction ignored, security trace |
| GN-T058 | Web fetch DNS rebinding/private IP | S | SSRF rejected before fetch |
| GN-T059 | Corrupt ZIP/BMG | S | safe parser failure and source quarantine |
| GN-T060 | Source interrupted import, resume | S | dedupe, checkpoint resume, source identity unchanged |
| GN-T061 | User asks “use another image” | S | only art proposals updated; message graph untouched |
| GN-T062 | Character personality revised in chat | S,L | only draft voice brief and audition regenerated |
| GN-T063 | Voice approval and publishing gate | S,L | draft stays draft until authorized approval |
| GN-T064 | Manual wrong-speaker correction | S,D,L | persisted correction, replay testcase, graph/index update |
| GN-T065 | Profile export/import without original assets | S | manifest portable, missing assets surfaced not bundled |
| GN-T066 | Synthetic game with very different UI | S,R | same runtime and UI, no Wind Waker-specific core code |
| GN-T067 | Added new language/locale | S | OCR packs and rendering config, fallback graceful |
| GN-T068 | Contradictory source speaker labels | S | conflict review not overwrite |
| GN-T069 | User source rights unknown | S | source approval required, default export restricts bytes |
| GN-T070 | Full onboarding interrupted by model outage | S,L | resumable, correct job blocked reason, no partial publish |

## E. UI testing and release

| ID | Scenario | Modes | Required assertions |
|---|---|---|---|
| GN-T071 | Full user journey browser: add game to speak | S,D,L | all pages/actions via Playwright, real Go state |
| GN-T072 | Developer frame→Clef→voice timeline | D,L | identical correlation IDs displayed across panels |
| GN-T073 | Recording fixture from developer correction | D,L | new fixture manifest hash, reproducible replay failure-then-pass |
| GN-T074 | Profile revision editing while session active | S,D | active revision pinned; update available safely |
| GN-T075 | Authenticated and unauthenticated capture preview | S,D | auth enforced, no credential leakage |
| GN-T076 | Vite embedded binary without frontend dev server | S | deep links, JS/CSS loaded, UI responsive |
| GN-T077 | Fresh `.deb` install and upgrade on target | H | systemd, deps, migrations, persistent profiles |
| GN-T078 | Multiarch OCI manifest and startup | S | amd64+arm64, health, non-root, no game assets |
| GN-T079 | Content scan of public archives | S | no ROM, audio references, dumps, secrets or debug video |
| GN-T080 | Restore old backup after migration failure | S | integrity restored; explicit compatible rollback |

## Suggested 12 real Dolphin scenarios to create (when user supplies game and enables graphical control)

1. Opening scene with slow typewriter dialogue. 2. A distinct second character's greeting. 3. Multi-page speech. 4. NPC short repeated line. 5. Textbox ending while voice is active. 6. A new character/speaker with ambiguous short phrase. 7. Sign or placard opened via interaction. 8. Item pickup tutorial. 9. Dynamic player-name/control button icons. 10. Choice prompt and branch. 11. Cutscene with box style change. 12. Save/load then repeated scene/character.

The coding agent must locate actual reproducible game states (not invent line IDs), create DTM/controller sequences or other verified stable controls, label ground truth through its actual screen and source lookup, then save manifest/hash. Each scenario is valid only after an actual reference run and meaningful oracle assertions.
