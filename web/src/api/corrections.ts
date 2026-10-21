/**
 * `src/api/corrections.ts` — typed access to the user-fix labels
 * (ADR-0052, docs/spec/08 "user fixes become labels"): the Play operator
 * view's Wrong speaker / Wrong text buttons and the trace actions record a
 * label through this. The label lands as *proposed* with the caller as
 * actor; it never applies a fix (applying is the corresponding content
 * write). The route is curator-scoped (the API 403s below it).
 */

import { api } from "./client";
import type { Schemas } from "./client";

export type CorrectionKind = Schemas["CorrectionKind"];
export type CorrectionTarget = Schemas["CorrectionTarget"];
export type Correction = Schemas["Correction"];

/** The body of POST /api/v1/corrections (the contract's CreateCorrectionRequest). */
export interface RecordCorrectionBody {
  profile_id: string;
  kind: CorrectionKind;
  target_type: CorrectionTarget;
  /** The row the correction is about (for a message target: the message id). */
  target_id: string;
  /** For a message target: must equal target_id when present. */
  message_id?: string;
  /** What was wrong. */
  before_value: string;
  /** What it should be; the proposal's asserted fix (optional on a proposal). */
  after_value?: string;
  reason?: string;
  /** Frame/observation ids, asset hashes — the evidence pointers. */
  evidence?: string[];
}

/** Record one correction label. The 200 body names the stored row. */
export function recordCorrection(
  body: RecordCorrectionBody,
  signal?: AbortSignal,
): Promise<{ correction: Correction }> {
  return api
    .post("/api/v1/corrections", { body, signal })
    .then((result) => result as unknown as { correction: Correction });
}
