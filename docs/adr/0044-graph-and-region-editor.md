# ADR-0044: the dialogue graph and the region editor ride the profile-scoped API; the region write is the message-edit's whole-set If-Match pattern

Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/09 ("/games/:id/graph", "/games/:id/regions", "UX robustness"), docs/spec/05 (normalized region geometry), ADR-0015 (profile row version as the draft-write token), ADR-0024 (replay allowlist, test-on-fixture); tests `internal/api` (graph filters/pagination bounds, If-Match conflict, geometry validation, preview with the `testdata/synthetic/textbox` frame), `web/src/lib/regionGeometry.test.ts`, `web/src/pages/Regions.test.tsx`, `web/e2e/m5-graph-regions.spec.ts`

## Context and constraints

The milestone 5 editor needs two surfaces the catalogue pages never had: a
virtualized scene/message graph (branch/loop/repeat edges, confidence and
source labels, server-side speaker/kind/scene filters) and a calibration
editor that draws boxes over a frame, runs OCR over them, and saves the set.
Three constraints shaped the design:

1. **The browser must never mount thousands of nodes.** The graph can hold
   thousands of messages; the page paints a window (`visibleRange`) of one
   server-bounded page (`limit ≤ 2000`), and the server answers edges only
   when both endpoints landed on the page, so the client holds one page's
   worth.
2. **A half-saved region set cannot exist.** Boxes are edited as a set; a
   per-box write is a read-modify-write race the revision token exists to
   catch. The message edit already settled this shape (ADR-0015): the
   profile's `row_version` is the `If-Match` token, 412 means "someone else
   saved", 409 means "the revision is frozen (derive)", 422 names the box
   that is wrong.
3. **The editor is a canvas, not a form.** The frame — an uploaded capture or
   a fixture frame admitted by the replay allowlist (ADR-0024) — is the
   surface; boxes are normalized [0,1] frame fractions (docs/spec/05) so a
   480i and a 1080p capture share the same region. Everything that is not a
   box (the placeholder, the uploaded image) must not eat the pointer walk.

## Decision

- **Both surfaces are profile-scoped routes on the existing API process.**
  No new service, no new store: `GET /api/v1/profiles/{id}/graph` (filters
  `speaker`/`kind`/`scene` applied server-side; `cursor`/`limit` pagination,
  bounded; nodes carry the short normalized text ≤ 80 chars, speaker, kind,
  scene and attribution; edges carry kind, confidence and source/evidence)
  and the region triple `GET` (read, beside the other profile-scoped reads) /
  `PUT` (whole-set write) / `POST …/regions/preview`. Store helpers live in
  new `internal/store` files; handlers in `internal/api/graph.go` and
  `internal/api/regions*.go`; editor-role writes, as on the message edit.
- **The region write is the message edit's pattern, verbatim.** One
  whole-set `PUT` with the profile's `If-Match` row-version token; 412/409/
  422 semantics identical to the message edit, and the client's 428
  once-only etag heal applies unchanged. A `PUT` that names a subset is a
  race the token exists to catch, and a body that cannot express "the boxes
  I see now, all of them" is not the write.
- **Geometry is validated against the unit square, in the store's own
  convention.** Normalized 0..1 boxes; `x + w ≤ 1`, `y + h ≤ 1`; the editor
  applies the same clamp locally (so a drag never paints a box the save will
  refuse) and the pixel presets stamp `min/max_aspect` plus the minimum
  resolution the preset was calibrated on.
- **The preview is a what-if read over the real OCR engine.** Multipart image
  ≤ 4 MB, or `fixture` = a frame under the configured replay roots; per box
  four variants (PSM 6/7 × preprocess on/off) with confidence and per-variant
  timings, plus the textbox-presence verdict. The image is never stored
  unless dev trace is on — the preview reads a frame, it does not collect
  them.
- **The editor's canvas owns the pointer; nothing over the frame may intercept
  it.** The placeholder and the uploaded image render at `pointer-events:
  none` so the canvas sees every press; boxes and handles stop propagation.
  This is the shape the editor must keep: the first implementation let the
  placeholder sit over the canvas, and a later revision that re-rendered
  unboundedly after a drawn box left the renderer unable to produce a frame —
  the Playwright screenshot after the draw never returned and the e2e walk
  hung. The regression is pinned in the unit lane: `Regions.test.tsx`
  performs the pointer walk (down / 12 moves / up) against a mocked frame and
  asserts the render count stays bounded and settles; the e2e walk keeps the
  real draw → preview → save → reload sequence.

## Options considered

- **Per-box region writes (POST/DELETE one region).** Rejected: the read-
  modify-write race the `If-Match` token exists to catch, and a page that
  "saves two boxes" cannot express "these are the boxes I see, all of them".
- **A separate graph service / a websocket-fed live graph.** Rejected: the
  data is the same content tables the catalogue already reads; a bounded
  server page plus a client window is the whole requirement, and a second
  process buys nothing the deadline budget would not pay for.
- **Client-side OCR preview (a WASM engine).** Rejected: the preview's point
  is that the *same* engine the runtime uses reads *this* frame; the server
  already carries the engine and the replay allowlist (ADR-0024), and a
  second OCR in the browser would be a second calibration to keep honest.

## Tradeoffs

- The whole-set `PUT` means the editor resends every box on every save; for
  the sizes a calibration set has (dozens of boxes) that is trivial, and the
  alternative (partial writes) is the race the token exists to catch.
- Edges are page-bounded by construction: an edge whose endpoints land on
  different pages is not answered. The page limit (2000) keeps a single
  page's graph readable; a revision that needs the whole graph in one view
  is a different tool, not this page.
- The preview refuses to store the frame outside dev trace, so a "preview
  later" workflow must re-run the preview; the trade is deliberate —
  calibration frames are capture material, and the asset store is not a
  staging bin for them.

## Validation

- `go test -race ./internal/api/... ./internal/store/...` (filters,
  pagination bounds, If-Match conflict, geometry validation, preview over
  the synthetic textbox frame).
- `web/src/lib/regionGeometry.test.ts` (clamp/normalize/nudge/resize/preset
  invariants) and `web/src/pages/Regions.test.tsx` (the pointer walk settles:
  bounded renders, the drawn box lands).
- `web/e2e/m5-graph-regions.spec.ts` on the packaged binary with the repo's
  `testdata` on the replay allowlist: the graph renders the seeded lines with
  their attribution labels and the speaker filter is server-side; the
  regions walk loads the stored box, draws a second, runs the real engine
  over the synthetic frame (the variant that reads the recorded line), saves
  with the profile's `If-Match`, and a reload shows the saved set. Screenshots
  per step; no unexpected console errors.

## Consequences and reversal

- New OpenAPI paths and generated client types (`pnpm gen:api`); the editor
  pages are curator-writable, viewer-readable.
- Reversal: the routes are additive — deleting the handlers and the two
  pages (and the OpenAPI entries) returns the catalogue to its prior shape;
  the `message_edges` and region columns stay, owned by the content schema.
  The pointer-events shape of the editor canvas is a product requirement,
  not a coupling: the regression test is the record of why.
