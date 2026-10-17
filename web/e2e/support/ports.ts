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
} as const;
