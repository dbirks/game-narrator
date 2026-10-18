// The ports the e2e servers listen on, in one place. playwright.config.ts reads
// them to start the servers; the specs read them to assert which server a
// project is actually talking to. A port that lives in only one of the two is
// the config/spec drift e2e/isolation.spec.ts exists to catch.
export const ports = {
  /** `go run ./cmd/game-narrator serve` behind the Vite dev server. */
  api: 18090,
  /** Vite dev server — the chromium project's baseURL. */
  web: 5173,
  /** the packaged binary serving the embedded UI (embedded.spec.ts). */
  embedded: 18095,
  /** the packaged binary the catalogue walk-through claims (catalog.spec.ts). */
  catalog: 18096,
  /** the packaged binary the developer inspector drives a replay on (developer.spec.ts). */
  developer: 18097,
  /** the packaged binary the onboarding agent runs against (onboarding.spec.ts). */
  onboarding: 18098,
  /** the scripted model gateway + malicious fixture page (web/e2e/fake-model.mjs). */
  fakeModel: 18099,
  /** the scripted TTS instance (web/e2e/fake-tts.mjs) the voice spec points the
   *  speech roles and the registry at. */
  fakeTTS: 18100,
  /** the packaged binary the voice workflow drives (voices.spec.ts). */
  voices: 18101,
  /** the packaged binary the m5-part1 walk (play / characters / publish / diagnostics)
   *  drives, with the repo's testdata admitted to the replay allowlist. */
  play: 18102,
  /** the packaged binary the m5 graph/regions walk drives: the replay allowlist
   *  admits the repo's testdata (the preview's fixture), so OCR runs against
   *  the synthetic textbox frame. */
  graphRegions: 18103,
} as const;
