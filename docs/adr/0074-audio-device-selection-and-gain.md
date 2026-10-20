# ADR-0074: Audio output as a product surface — device enumeration, persisted selection, test tone, per-voice gain, output stats

Date: 2026-10-19
Status: accepted
Owners: agent worker (g5-audio-devices)
Related requirements/tests: docs/spec/07 ("Audio scheduler"; "Audio coexistence and acoustics" — test tone, per-voice gain), docs/spec/08 (routes, scope table), docs/spec/09 (/settings "audio sinks"; /setup "run test audio"), docs/spec/10 ("List ALSA/PipeWire audio devices; play deterministic test tone … reselect by stable ID on reboot"), docs/spec/12 (stop latency; no acoustic claims without a mic), docs/spec/13 ("playback is a consented action"; "playback underruns" counter), ADR-0023 (subprocess sink, stable identity, virtual-sink evidence), ADR-0039 (env-configured sink at boot), AGENTS.md rules 3/5/6/10. Tests: `internal/audio/` (enumeration parsers, synthetic list, selection precedence, tone), `internal/api/audio_test.go` (handlers, consent gate, validation, authz), `internal/api/audio_live_test.go` (PipeWire null-sink round trip, NOT RUN without the host tools), `internal/store/settings_store_test.go` (migration backup/restore), `internal/doctor/audio_check_test.go`, `web/e2e/audio-output.spec.ts` (Playwright on the labelled-synthetic device list).

## Context and constraints

Gap audit G5 (docs/status/gap-audit-2026-10-19.md): the audio output is chosen only by two environment variables (`GAME_NARRATOR_AUDIO_SINK`, `GAME_NARRATOR_AUDIO_DEVICE`), rendered read-only on the Settings page. There is no device enumeration, no product test tone, no per-voice gain, no master volume and no output latency/underrun surface. On the UNO Q the user attaches a USB DAC next to the HDMI dongle, and the dongle may expose a UAC audio interface that must not become the default output. docs/spec/10 requires selection "by stable ID on reboot", docs/spec/13 makes playback a consented action and names "playback underruns" as a counter.

The existing pieces this ADR builds on: the `pactl list [short] sinks` enumeration the doctor and hwsmoke already use (sink NAME as the stable identity), the deterministic 440 Hz / 1 s / 48 kHz tone constant in `internal/hwsmoke/audio_test.go`, the `speech.AudioSink` family (pipewire subprocess sink, filesink evidence, dummy), the session's live volume wrapper (`gainSink`), and the store's migration discipline (AGENTS.md rule 10: every schema change ships a forward migration and a backup/restore test).

## Options

1. **Enumeration package + persisted selection + tone endpoint + voice-profile gain** (adopted): a new `internal/audio` package owns device listing (PipeWire via `pactl` when available, ALSA via `aplay -L` as the fallback, a labelled-synthetic list for the test environment), the deterministic test tone and the selection precedence; the store gains an `app_settings` table (key/value) and a `voice_profiles.gain` column; three API routes expose the surface.
2. Extend the doctor CLI only (no product surface) — rejected: the spec names the Settings/Setup pages and the counters; the doctor already enumerates sinks, so the product needs the enumeration as a library.
3. A generic per-utterance "effects" pipeline (resampling, filters) — rejected: scope. Gain is the one named effect; a generic DSP path would be a large surface nobody has asked for.

## Decision

### Enumeration and selection

- `internal/audio.ListDevices(ctx, lookupEnv)` returns `[]Device{ID, Name, Kind, SampleRate, Channels, PlatformDefault}` plus the backend name (`pipewire` | `alsa` | `synthetic` | `none`). Kinds are `usb` | `hdmi` | `builtin` | `null` | `unknown`. The PipeWire ID is the sink NAME (the same stable identity the app's sink and the hwsmoke gate use); the ALSA ID is the `aplay -L` device name. Kind inference is name/property based (factory `null` sink → `null`; `usb`/`uac` → `usb`; `hdmi`/`displayport`/`optical`/`iec958` → `hdmi`; the ALSA `default`/`front`/`surround` set and unmapped PipeWire cards → `builtin`); an unrecognised row is `unknown`, never silently re-classified.
- `GAME_NARRATOR_AUDIO_FAKE_DEVICES` (comma list of `name[:kind[:rate]]`) makes the lister return a fixed synthetic list flagged `synthetic: true`. It is a test seam (the Playwright suite has no USB DAC); the flag travels into the API response and the UI label so a synthetic list is never mistaken for hardware.
- Precedence (documented on the API and the page): for the sink kind and the device, an environment variable, when set, **overrides** the persisted store value; the store value (the user's selection) beats the built-in default (`filesink` / `default`). Master volume has no environment form — it is the store value or the default 1.0. The effective selection and the source of each part (`env` | `store` | `default`) are reported, so the page can say why a value wins.
- The `cmd/game-narrator serve` wiring resolves the session's sink from store → env → default with the same function the API uses, so the process and the page cannot disagree. (ADR-0039's env-only boot is amended: env stays the override; the store becomes the default layer beneath it.)

### API

- `GET /api/v1/audio/devices` (admin, like the settings page it feeds): the device list with the effective selection, the per-device `selected` flag and the `synthetic` flag.
- `PUT /api/v1/settings/audio` (admin): partial update of `device`, `sink_kind`, `master_volume`; the device is validated against the current enumeration (or `default`); a save writes one store row and the answer is the fresh effective selection. A store-less process answers 503.
- `POST /api/v1/audio/test-tone` (admin, **consent-gated**: the body must set `consent: true` — docs/spec/13 makes playback a consented action; anything else is 400 before any audio is touched): plays the deterministic tone through the selected sink, or through `device` when the body names one. Reuse of the existing sinks: pipewire → the `pipewire` sink aimed at the named sink; filesink → a one-shot filesink under a temp dir (the answer carries the WAV path — evidence, with the docs/spec/11 note that a file is not proof of audibility); dummy → paces and reports that nothing was delivered. The answer carries the tone parameters, the sink used, the device and the measured duration.
- Voice gain: `POST /api/v1/voices/{id}/gain` (curator, the tier that edits voices) sets `voice_profiles.gain` (0..10, default 1.0) and is reflected in the `GET /api/v1/voices` list and detail rows. The runtime applies the approved voice's gain to the utterance's samples at playback (decode → scale → re-encode) — **no re-synthesis**: the cache key is unchanged, so a gain change takes effect at the next playback of a line that was already cached.
- Session stats: `pipeline.Stats` gains `audio_underruns` (a started playback whose terminal result is a sink failure — samples lost mid-utterance; the docs/spec/13 "playback underruns" counter) and `audio_output_latency` (a bucketed histogram of `Play() accepted → first sample handed to the device`, reported by the pipewire sink's first successful pipe write; other sinks report nothing and the summary reads count 0). The session's live `AudioInfo` carries both, and the terminal `session.stopped`/`session.failed` payload (which the bottleneck summary and the support bundle reduce) carries them through `pipeline.Stats`.

### UI and doctor

- Settings page: an **Audio output** section — the device list with kind and the selected flag, a select, the test-tone button (the consent is the click: the page posts `consent: true` only in response to the user action), and the master volume. The section is labelled when the list is synthetic. The Voices page gets a per-voice gain slider on each row's detail.
- Doctor: a new `audio.selected_device` check — the selected device exists in the current enumeration (or is `default`), and when the capture dongle's UAC interface is enumerable it is **not** the selected output (docs/spec/10: the HDMI dongle's audio must not be the speech output). The check never plays anything.

## Tradeoffs

- **Subprocess enumeration (`pactl`/`aplay`) instead of a native PipeWire client**: consistent with ADR-0023's subprocess sink and with the doctor/hwsmoke precedent; no new Go dependency; the cost is parse fragility, mitigated by fixture-based parser tests and by reporting, not guessing, when a row is unrecognised.
- **Storing the selection in a new `app_settings` table** instead of extending an existing table: the appliance has no settings table yet (settings are env-only, ADR-0047), and a key/value table is the shape later settings (G18's named `PUT /api/v1/config` contract) can grow into without a second migration.
- **Gain on the voice profile row** (not a per-session override): the spec calls it per-voice; a revision is one row, so the gain travels with the revision the line was cached under. The session's live volume (existing) still multiplies on top at playback.
- **Consent via the request body** (not a separate persistent consent record): the click that triggers the tone is the consent; the endpoint refuses to run without `consent: true`, which is the docs/spec/13 gate the hwsmoke tone already follows in its environment form.
- **The synthetic device list is an environment variable**: a production process that has it set is serving a fake list — the flag is reported in every response and the doctor check will fail a non-`default` selection against a synthetic list, so the misconfiguration is loud.

## Validation

- `go test ./internal/audio/... ./internal/api/... ./internal/store/... ./internal/doctor/... ./internal/runtime/... ./internal/pipeline/...` (race on): parser fixtures (pactl long/short, aplay -L, synthetic env), consent gate (no tone without it), device validation (unknown device rejected against the enumeration), selection precedence (env beats store beats default, all four orderings), gain applied without a cache miss (same cache key, scaled samples asserted), underrun/latency counters visible in the session snapshot and the terminal payload, doctor check (missing selected device → FAIL; dongle UAC selected → FAIL; clean host → OK).
- Live host check (this dev host, PipeWire present): `TestAudioTestTonePipeWireNullSink` — a dedicated null sink (`module-null-sink`), the endpoint's tone played through it, the sink's monitor source recorded and asserted non-silent near 440 Hz (the `TestPipeWireNullSinkRoundTrip` evidence standard; NOT RUN with the documented marker when the host lacks the tools).
- Web: `pnpm typecheck && pnpm lint && pnpm fmt:check && pnpm test` plus the Playwright `audio-output` spec against the real binary with the synthetic device list declared in the config (labelled synthetic in the DOM, selection persisted across a refetch, tone consent posted on click).
- Migration: `0013_app_settings.sql` + `0014_voice_profile_gain.sql` with the backup/restore test (AGENTS.md rule 10) and the existing migration-suite assertions (checksum, ordering).

## Consequences

- ADR-0047's "nothing is mutable" claim gains a documented exception: the audio output selection and master volume are runtime-mutable (admin, persisted), and the Settings page's mutable note says so. The env variables remain overrides and keep working exactly as ADR-0039 documents them.
- The `pipeline.Stats` shape grows two fields (additive, JSON-tagged; old payloads without them decode as zero — the backward-compatible direction).
- A USB DAC attached to the appliance is selectable from the browser without an env edit and a restart; the dongle's UAC interface is detectable as the wrong default.

## Reversal plan

The two migrations are additive (new table; new column with a constant default); reversing them means dropping the table/column in a forward migration and removing `internal/audio`'s store reads — the env-only ADR-0047 posture is fully restored because every value still resolves from the environment when the store row is absent. The API routes and the UI section are removable without touching the session runtime's sink contract.
