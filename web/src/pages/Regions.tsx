import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type ChangeEvent,
  type KeyboardEvent,
  type PointerEvent as ReactPointerEvent,
} from "react";
import { useParams, useSearchParams } from "react-router";

import { api, catalogApi, refreshProfileEtag } from "../api/client";
import { useAuth } from "../auth/auth-context";
import { ErrorState, Loading, ProblemList } from "../components/states";
import {
  ASPECT_PRESETS,
  applyPreset,
  nudgeBox,
  nudgeStep,
  normalizeBox,
  resizeBox,
  type RegionBox,
} from "../lib/regionGeometry";

/**
 * `/games/:id/regions` — the OCR region editor of docs/spec/09.
 *
 * The page is a canvas of normalized boxes over a frame the editor chose: an
 * uploaded capture (multipart preview, the image part is the frame) or a
 * fixture path under the replay roots ("test on fixture"). Boxes are drawn by
 * dragging, moved and resized by pointer or keyboard (arrows nudge, Shift
 * moves ten times as far, Delete removes), and the aspect presets stamp a box
 * with the min/max aspect and pixel floor it was calibrated on.
 *
 * The save is a whole-set PUT with If-Match, exactly like the message edit:
 * 412 means somebody else saved (reload), 409 means the revision is frozen
 * (derive), 422 lists the box that is wrong. "Preview" is a what-if read —
 * four OCR variants per box (PSM 6/7 × preprocess) plus the textbox presence
 * verdict — and the page renders the timings it carries.
 */

interface DraftBox extends RegionBox {
  region_type: RegionType;
  min_aspect?: number;
  max_aspect?: number;
  min_width_px?: number;
  min_height_px?: number;
}

type Drag =
  | { mode: "draw"; startX: number; startY: number }
  | { mode: "move"; index: number; startX: number; startY: number; orig: RegionBox }
  | {
      mode: "resize";
      index: number;
      handle: "nw" | "ne" | "sw" | "se";
      startX: number;
      startY: number;
      orig: RegionBox;
    };

const REGION_TYPES = [
  "textbox",
  "speaker_label",
  "subtitle",
  "sign",
  "item",
  "menu",
  "tutorial",
  "hud",
  "other",
] as const;
type RegionType = (typeof REGION_TYPES)[number];
const DEFAULT_FIXTURE = "synthetic/textbox/plain-dejavu-01.png";

export function RegionsPage() {
  const { id: gameId = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const { atLeast } = useAuth();
  const queryClient = useQueryClient();

  const profileId = params.get("profile") ?? "";
  const fallback = useDefaultProfile(gameId, profileId === "");
  useEffect(() => {
    if (profileId === "" && fallback) {
      const next = new URLSearchParams(params);
      next.set("profile", fallback);
      setParams(next, { replace: true });
    }
  }, [profileId, fallback, params, setParams]);

  const [boxes, setBoxes] = useState<DraftBox[]>([]);
  const [selected, setSelected] = useState<number | null>(null);
  const [newType, setNewType] = useState<RegionType>("textbox");
  const [file, setFile] = useState<File | null>(null);
  const [objectUrl, setObjectUrl] = useState<string | null>(null);
  const [fixture, setFixture] = useState(DEFAULT_FIXTURE);
  const [frame, setFrame] = useState<{ w: number; h: number } | null>(null);
  const [conflict, setConflict] = useState<"stale" | "frozen" | null>(null);

  const canvasRef = useRef<HTMLDivElement>(null);
  const dragRef = useRef<Drag | null>(null);
  // A 428 ("If-Match required") is retried at most once: re-read the revision's
  // token and save again. A second 428 means the token read itself is broken.
  const etagHealRetried = useRef(false);

  // The stored set is the starting point; the If-Match token is the profile's.
  const regions = useQuery({
    queryKey: ["regions", profileId],
    queryFn: ({ signal }) =>
      api.get("/api/v1/profiles/{id}/regions", { params: { id: profileId }, signal }),
    enabled: profileId !== "",
  });
  useEffect(() => {
    setBoxes(
      (regions.data?.regions ?? []).map((r) => ({
        id: r.id,
        region_type: r.region_type as RegionType,
        x: r.rect_x,
        y: r.rect_y,
        w: r.rect_w,
        h: r.rect_h,
        ...(r.min_aspect != null ? { min_aspect: r.min_aspect } : {}),
        ...(r.max_aspect != null ? { max_aspect: r.max_aspect } : {}),
        ...(r.min_width_px != null ? { min_width_px: r.min_width_px } : {}),
        ...(r.min_height_px != null ? { min_height_px: r.min_height_px } : {}),
      })),
    );
    setFrame({ w: 640, h: 480 });
  }, [regions.data, profileId]);

  const previewMutation = useMutation({
    mutationFn: async ({ signal }: { signal: AbortSignal }) => {
      const wire = boxes.map((b) => ({ id: b.id, x: b.x, y: b.y, w: b.w, h: b.h }));
      if (file) {
        const fd = new FormData();
        fd.append("image", file, file.name);
        fd.append("regions", JSON.stringify(wire));
        fd.append("dev_trace", "true");
        return api.post("/api/v1/profiles/{id}/regions/preview", {
          params: { id: profileId },
          form: fd,
          signal,
        });
      }
      return api.post("/api/v1/profiles/{id}/regions/preview", {
        params: { id: profileId },
        body: { fixture, regions: wire },
        signal,
      });
    },
  });
  const preview = previewMutation.data;
  useEffect(() => {
    if (preview && (preview.width > 0 || preview.height > 0)) {
      setFrame({ w: preview.width, h: preview.height });
    }
  }, [preview]);

  const saveMutation = useMutation({
    mutationFn: async ({ signal }: { signal: AbortSignal }) =>
      api.put("/api/v1/profiles/{id}/regions", {
        params: { id: profileId },
        body: {
          regions: boxes.map((b) => ({
            ...(b.id ? { id: b.id } : {}),
            region_type: b.region_type,
            x: round3(b.x),
            y: round3(b.y),
            w: round3(b.w),
            h: round3(b.h),
            ...(b.min_aspect != null ? { min_aspect: b.min_aspect } : {}),
            ...(b.max_aspect != null ? { max_aspect: b.max_aspect } : {}),
            ...(b.min_width_px != null ? { min_width_px: b.min_width_px } : {}),
            ...(b.min_height_px != null ? { min_height_px: b.min_height_px } : {}),
          })),
        },
        signal,
      }),
    onSuccess: () => {
      setConflict(null);
      setBoxes((prev) =>
        prev.map((b, i) => ({ ...b, id: saveMutation.data?.regions?.[i]?.id ?? b.id })),
      );
      queryClient.invalidateQueries({ queryKey: ["regions", profileId] });
    },
    onError: async (err) => {
      const status = err && (err as { status?: number }).status;
      if (status === 428 && !etagHealRetried.current) {
        // The write was refused for lacking If-Match even though the page just
        // loaded the set: the token cache came up empty (a fresh load that
        // raced its own read). The 428 names the remedy — GET the revision
        // first — so re-read the token and retry once. A save that is never
        // attempted silently is worse than the 412 it would have caught.
        etagHealRetried.current = true;
        try {
          await refreshProfileEtag(profileId);
        } catch {
          return; // the profile read failed; the page's queries show the error
        }
        saveMutation.mutate({ signal: new AbortController().signal });
        return;
      }
      if (status === 412) setConflict("stale");
      else if (status === 409) setConflict("frozen");
      else setConflict(null);
    },
  });

  // ── pointer geometry ─────────────────────────────────────────────────────
  const toNorm = (clientX: number, clientY: number) => {
    const el = canvasRef.current;
    if (!el) return { x: 0, y: 0 };
    const rect = el.getBoundingClientRect();
    return {
      x: Math.min(1, Math.max(0, (clientX - rect.left) / rect.width)),
      y: Math.min(1, Math.max(0, (clientY - rect.top) / rect.height)),
    };
  };

  const onCanvasPointerDown = (e: ReactPointerEvent) => {
    if (e.target !== e.currentTarget) return; // a box or handle got the press
    const p = toNorm(e.clientX, e.clientY);
    dragRef.current = { mode: "draw", startX: p.x, startY: p.y };
    setBoxes((prev) => [...prev, { id: "", region_type: newType, x: p.x, y: p.y, w: 0, h: 0 }]);
    setSelected(boxes.length); // the draft lands at the current end of the list
    e.currentTarget.setPointerCapture(e.pointerId);
  };
  const onHandleDown = (e: ReactPointerEvent, index: number, handle: "nw" | "ne" | "sw" | "se") => {
    e.stopPropagation();
    const p = toNorm(e.clientX, e.clientY);
    dragRef.current = {
      mode: "resize",
      index,
      handle,
      startX: p.x,
      startY: p.y,
      orig: boxes[index],
    };
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
  };
  const onBoxDown = (e: ReactPointerEvent, index: number) => {
    e.stopPropagation();
    setSelected(index);
    const p = toNorm(e.clientX, e.clientY);
    dragRef.current = { mode: "move", index, startX: p.x, startY: p.y, orig: boxes[index] };
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
  };
  const onPointerMove = (e: ReactPointerEvent) => {
    const drag = dragRef.current;
    if (!drag) return;
    const p = toNorm(e.clientX, e.clientY);
    if (drag.mode === "draw") {
      const geometry = normalizeBox({
        x: Math.min(drag.startX, p.x),
        y: Math.min(drag.startY, p.y),
        w: Math.abs(p.x - drag.startX),
        h: Math.abs(p.y - drag.startY),
      });
      setBoxes((prev) => {
        const last = prev.length - 1;
        return prev.map((b, i) => (i === last ? { ...b, ...geometry } : b));
      });
    } else if (drag.mode === "move") {
      const moved = nudgeBox(drag.orig, p.x - drag.startX, p.y - drag.startY);
      setBoxes((prev) => prev.map((b, i) => (i === drag.index ? { ...b, ...moved } : b)));
    } else {
      const moved = resizeBox(drag.orig, drag.handle, p.x - drag.startX, p.y - drag.startY);
      setBoxes((prev) => prev.map((b, i) => (i === drag.index ? { ...b, ...moved } : b)));
    }
  };
  const onPointerUp = () => {
    const drag = dragRef.current;
    dragRef.current = null;
    if (drag?.mode === "draw") {
      // A click that never dragged is not a box: drop the zero-size draft.
      setBoxes((prev) => {
        const b = prev[prev.length - 1];
        return b && b.w < 0.005 && b.h < 0.005 ? prev.slice(0, -1) : prev;
      });
    }
  };

  // ── keyboard ─────────────────────────────────────────────────────────────
  const onKey = (e: KeyboardEvent) => {
    if (selected === null || boxes[selected] === undefined) return;
    const step = nudgeStep(e.shiftKey);
    if (e.key === "ArrowLeft")
      setBoxes((p) => p.map((b, i) => (i === selected ? { ...b, ...nudgeBox(b, -step, 0) } : b)));
    else if (e.key === "ArrowRight")
      setBoxes((p) => p.map((b, i) => (i === selected ? { ...b, ...nudgeBox(b, step, 0) } : b)));
    else if (e.key === "ArrowUp")
      setBoxes((p) => p.map((b, i) => (i === selected ? { ...b, ...nudgeBox(b, 0, -step) } : b)));
    else if (e.key === "ArrowDown")
      setBoxes((p) => p.map((b, i) => (i === selected ? { ...b, ...nudgeBox(b, 0, step) } : b)));
    else if (e.key === "Delete" || e.key === "Backspace") {
      e.preventDefault();
      setBoxes((p) => p.filter((_, i) => i !== selected));
      setSelected(null);
    } else return;
    e.preventDefault();
  };

  const applyAspectPreset = (preset: (typeof ASPECT_PRESETS)[number]) => {
    if (selected === null) return;
    const result = applyPreset(boxes[selected], preset);
    setBoxes((p) =>
      p.map((b, i) =>
        i === selected
          ? {
              ...b,
              ...result.box,
              min_aspect: result.minAspect,
              max_aspect: result.maxAspect,
              min_width_px: result.minWidthPx,
              min_height_px: result.minHeightPx,
            }
          : b,
      ),
    );
  };

  const onFile = (e: ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0] ?? null;
    if (objectUrl) URL.revokeObjectURL(objectUrl);
    setFile(f);
    setObjectUrl(f ? URL.createObjectURL(f) : null);
    setFrame(null);
  };

  const sel = selected !== null ? boxes[selected] : null;

  if (regions.isError) {
    return (
      <ErrorState
        error={regions.error}
        action="Loading the revision's regions"
        onRetry={() => regions.refetch()}
      />
    );
  }
  if (regions.isPending) return <Loading label="Loading regions…" />;

  const canWrite = atLeast("curator");
  return (
    <div className="regions-page" data-testid="regions-page" onKeyDown={onKey} tabIndex={0}>
      <header className="regions-header">
        <h1>OCR regions</h1>
        <div className="regions-frame-src" data-testid="regions-frame-src">
          <label>
            Fixture
            <input
              value={fixture}
              onChange={(e) => setFixture(e.target.value)}
              data-testid="regions-fixture-input"
            />
          </label>
          <label>
            Or upload a frame
            <input
              type="file"
              accept="image/*"
              onChange={onFile}
              data-testid="regions-file-input"
              disabled={!canWrite}
            />
          </label>
        </div>
        {canWrite ? (
          <>
            <label>
              New box type
              <select value={newType} onChange={(e) => setNewType(e.target.value as RegionType)}>
                {REGION_TYPES.map((t) => (
                  <option key={t}>{t}</option>
                ))}
              </select>
            </label>
            {ASPECT_PRESETS.map((preset) => (
              <button
                key={preset.label}
                type="button"
                onClick={() => applyAspectPreset(preset)}
                disabled={!sel}
                data-testid={`regions-preset-${preset.label}`}
              >
                {preset.label}
              </button>
            ))}
            <button
              type="button"
              data-testid="regions-preview"
              onClick={() => previewMutation.mutate({ signal: new AbortController().signal })}
              disabled={boxes.length === 0}
            >
              Preview OCR
            </button>
            <button
              type="button"
              data-testid="regions-save"
              disabled={saveMutation.isPending}
              onClick={() =>
                saveMutation.mutate({
                  signal: new AbortController().signal,
                })
              }
            >
              Save regions
            </button>
          </>
        ) : (
          <p className="muted">Read-only: a curator account saves region sets.</p>
        )}
      </header>

      {conflict === "stale" ? (
        <div role="alert" data-testid="regions-conflict-stale" className="error">
          <p>
            Someone else saved this revision since you loaded it. Reload the regions and re-apply
            your boxes — the save will take their If-Match, not yours.
          </p>
          <button
            type="button"
            onClick={() => queryClient.invalidateQueries({ queryKey: ["regions", profileId] })}
          >
            Reload
          </button>
        </div>
      ) : null}
      {conflict === "frozen" ? (
        <div role="alert" data-testid="regions-conflict-frozen" className="error">
          <p>
            This revision is published — its regions are frozen. Derive a new revision to keep
            calibrating.
          </p>
        </div>
      ) : null}
      {saveMutation.isError &&
      saveMutation.error instanceof Error &&
      saveMutation.error.name === "ApiError" &&
      (saveMutation.error as { problems?: string[] }).problems?.length ? (
        <ProblemList
          problems={(saveMutation.error as unknown as { problems?: string[] }).problems ?? []}
          heading="The save was refused"
        />
      ) : null}

      <div className="regions-columns">
        <div className="regions-canvas-wrap">
          <div
            ref={canvasRef}
            className="regions-canvas"
            data-testid="regions-canvas"
            style={{
              aspectRatio: frame ? `${frame.w} / ${frame.h}` : "4 / 3",
              position: "relative",
              background: "#111",
              border: "1px solid #444",
            }}
            onPointerDown={onCanvasPointerDown}
            onPointerMove={onPointerMove}
            onPointerUp={onPointerUp}
          >
            {objectUrl ? (
              <img src={objectUrl} alt="Uploaded frame" draggable={false} />
            ) : (
              <div className="regions-fixture-ph" data-testid="regions-frame-placeholder">
                {fixture || "no frame"}
                {frame ? ` · ${frame.w}×${frame.h}` : ""}
              </div>
            )}
            {boxes.map((b, i) => (
              <div
                key={b.id || `draft-${i}`}
                className={i === selected ? "region-box region-box-selected" : "region-box"}
                data-testid={`region-box-${b.id || i}`}
                style={{
                  left: `${b.x * 100}%`,
                  top: `${b.y * 100}%`,
                  width: `${b.w * 100}%`,
                  height: `${b.h * 100}%`,
                  position: "absolute",
                  border: i === selected ? "2px solid #22d3ee" : "1px solid #22d3ee88",
                  background: "#22d3ee11",
                  cursor: "move",
                }}
                onPointerDown={(e) => onBoxDown(e, i)}
              >
                <span className="region-box-label">{b.region_type}</span>
                <span
                  className="region-handle nw"
                  onPointerDown={(e) => onHandleDown(e, i, "nw")}
                />
                <span
                  className="region-handle ne"
                  onPointerDown={(e) => onHandleDown(e, i, "ne")}
                />
                <span
                  className="region-handle sw"
                  onPointerDown={(e) => onHandleDown(e, i, "sw")}
                />
                <span
                  className="region-handle se"
                  onPointerDown={(e) => onHandleDown(e, i, "se")}
                />
              </div>
            ))}
          </div>
          <p className="muted">
            Drag on the canvas to draw a box · drag a box to move it · corner handles resize ·
            arrows nudge (Shift = 10×) · Delete removes the selected box
          </p>
        </div>

        <aside className="regions-side">
          {previewMutation.isPending ? <Loading label="Running the OCR preview…" /> : null}
          {previewMutation.isError ? (
            <ErrorState
              error={previewMutation.error}
              action="Previewing OCR"
              onRetry={() => previewMutation.mutate({ signal: new AbortController().signal })}
            />
          ) : null}
          {preview ? (
            <div data-testid="regions-preview-result">
              <p className="muted">
                {preview.source === "fixture"
                  ? `fixture ${preview.fixture_path}`
                  : "uploaded frame"}{" "}
                · {preview.width}×{preview.height} · engine {preview.engine || "?"} ·{" "}
                {preview.duration_ms} ms · textbox{" "}
                <strong data-testid="regions-verdict">{preview.textbox_verdict}</strong>
              </p>
              {preview.regions.map((r, i) => (
                <div
                  key={r.id || i}
                  data-testid={`regions-variants-${i}`}
                  className="regions-variants"
                >
                  <p>
                    Box {i + 1}
                    {r.empty ? " — empty crop" : ""} · presence {r.textbox.dark_share.toFixed(2)}
                    {r.textbox.present ? " (present)" : ""}
                  </p>
                  <table>
                    <thead>
                      <tr>
                        <th>PSM</th>
                        <th>preprocess</th>
                        <th>text</th>
                        <th>conf</th>
                        <th>ms</th>
                      </tr>
                    </thead>
                    <tbody>
                      {r.variants.map((v) => (
                        <tr key={`${v.psm}-${v.preprocess}`}>
                          <td>{v.psm}</td>
                          <td>{v.preprocess ? "on" : "off"}</td>
                          <td
                            data-testid={`regions-variant-text-${i}-${v.psm}-${v.preprocess ? 1 : 0}`}
                          >
                            {v.error ? v.error : v.text}
                          </td>
                          <td>{v.mean_confidence.toFixed(2)}</td>
                          <td>{v.duration_ms}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ))}
            </div>
          ) : (
            <p className="muted">
              Preview runs the real engine over the boxes: four variants each, with timings.
            </p>
          )}
        </aside>
      </div>
    </div>
  );
}

function round3(n: number): number {
  return Math.round(n * 1000) / 1000;
}

// Same default-revision rule the other per-game pages spell: published first,
// then the newest.
function useDefaultProfile(gameId: string, enabled: boolean): string | null {
  const editions = useQuery({
    queryKey: ["editions", gameId],
    queryFn: ({ signal }) => catalogApi.listEditions(gameId, signal),
    enabled,
  });
  const editionId = (editions.data as { editions: { id: string }[] } | undefined)?.editions?.[0]
    ?.id;
  const profiles = useQuery({
    queryKey: ["profiles", editionId],
    queryFn: ({ signal }) => catalogApi.listProfiles(editionId as string, signal),
    enabled: editionId !== undefined,
  });
  return useMemo(() => {
    const list =
      (
        profiles.data as
          | { profiles: { id: string; revision_num: number; status?: string }[] }
          | undefined
      )?.profiles ?? [];
    if (list.length === 0) return null;
    const sorted = [...list].sort((a, b) => {
      const ap = a.status === "published" ? 0 : 1;
      const bp = b.status === "published" ? 0 : 1;
      return ap - bp || b.revision_num - a.revision_num;
    });
    return sorted[0]?.id ?? null;
  }, [profiles.data]);
}
