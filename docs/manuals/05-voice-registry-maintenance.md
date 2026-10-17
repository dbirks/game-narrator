# Voice registry maintenance

## Purpose

Keep registered voices correct across profile revisions, service restarts
and gateway replica changes. The registry is **persistent state on the
direct Base service, never behind the collective gateway** (the gateway
returns 404 on `POST /audio/voices` — it is not proxied; ADR 0022). Today
the registry path exists **only on the direct Base service**; multi-replica
registry routing is not built.

## Preconditions

- The Base service reachable directly: `GAME_NARRATOR_TTS_BASE_DIRECT_URL`.
- `GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL` may name the registry
  endpoint explicitly; the `voice_clone` doctor probe uses it, falling back
  to `GAME_NARRATOR_TTS_BASE_DIRECT_URL`. If neither is set, doctor
  synthesises with `default` and reports the registry as **not probed** —
  it never pretends the registry was checked.
- A `voice_clone` model configured (04-inference-gateway-connection).

## Voice names

The canonical shape is `gn_<profile8>_<character>_v<rev>_<digest8>`
(`tts.MakeName`, `rev ≥ 1`; `ValidateName` enforces the shape and the hex
fields). Identity is the 8-hex `digest8` of the reference payload and is
carried in the name, so a name collision with different audio is detectable
from the name alone.

## Steps

1. **Register via the product path** (not ad-hoc HTTP): the voice workflow
   (ADR 0030) — audition from `/games/:id/voices`
   (`POST /api/v1/voices/auditions`), approve
   (`POST /api/v1/voices/{id}/approve`), then register
   (`POST /api/v1/voices/{id}/register`, operator role). The web client
   sends multipart `audio_sample` + `name` + `consent` (+ `ref_text`); a
   consent string is mandatory (`ErrConsentRequired`).
2. **Check state:** `GET /api/v1/voices`, `GET /api/v1/voices/{id}`,
   `GET /api/v1/voices/{id}/registrations` (per-instance registration
   state), and `bin/game-narrator doctor` (`providers.voice_clone` probes
   the registry list and synthesises with the first `gn_*` voice).
3. **Re-registration after a revision bump:** a new voice revision produces
   a new `digest8`, hence a **new name** — the old name is a different voice
   and must not be overwritten. Reconciliation is `Ensure`: missing →
   register; a conflicting name with a different digest is
   `ErrVoiceConflict` and is reported, never silently re-registered or
   overwritten.
4. **Removal:** only through the workflow. The Go client's `Remove` refuses
   anything outside the configured namespace (default `gn_test_`) as a hard
   guard against deleting user voices — the live tests exploit exactly this
   by registering only `gn_test_<random8>` voices and removing them in
   cleanup.

## Replica affinity

A voice missing on a replica is a `voice_missing` fault: the runtime
reconciles or falls back to the known built-in default, and never switches
to a wrong voice (docs/spec/07, docs/spec/13 resilience matrix; README
invariant 7 — the gateway must not let registrations silently disappear on
replica rerouting). If a registered voice stops resolving, check that the
role still points at the Base service that owns the registry
(`GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL` /
`GAME_NARRATOR_TTS_BASE_DIRECT_URL`) before touching any voice.

## Verification

- `doctor` reports `providers.voice_clone` `OK` with the registry probe
  detail (list + synthesis with a `gn_*` voice), or an honest
  `NOT CONFIGURED` / "registry not probed" note.
- `GET /api/v1/voices/{id}/registrations` shows the voice ready on the
  expected instance.

## What NOT to do

- Never point the app at the collective gateway for registry calls — it does
  not serve them.
- Never hand-edit a registered name to "reuse" an old revision's slot; the
  digest is the identity and a collision is a conflict, by design.
- Never register a voice without a consent string, and never delete a voice
  outside the workflow's namespace guard (AGENTS.md: approved voice
  references are user data the package must not delete).
- Do not present a `default`-voice synthesis as proof the registry works —
  that is exactly the degraded mode the doctor distinguishes.
