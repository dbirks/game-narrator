/**
 * `src/pages/cast/CastDetailPanel.tsx` — the Cast page's right-hand panel
 * (docs/design/plum-peach 2d): the character's name tab and dithered
 * portrait, the facts (lines · attribution, voice version · approval state,
 * first line id, the frozen consent record), the editable voice brief, the
 * current-take player (peach pill, waveform + ink play), the actions
 * (↻ Regenerate, 3 more takes, Hear lines) and the one-step "keep old take"
 * undo (ADR-0093 open question (d), UX-03d).
 *
 * Regeneration is a new revision: with the auto-cast dial on the job
 * auto-approves it and it becomes current; the previous take is not deleted
 * and stays playable, one undo away. With the dial off the new take lands
 * pending (auditioning) and the audition cards take over.
 */

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { catalogApi, type Character, type Message } from "../../api/client";
import {
  voicesApi,
  type VoiceAudition,
  type VoiceDetail,
  type VoiceRevision,
} from "../../api/voices";
import { ErrorState, Loading } from "../../components/states";
import { DitherCover, NameTab, PillButton, Switch, Waveform } from "../../components/kit";
import { CastAuditionPanel, VoiceGainControl } from "./CastAuditions";
import {
  attributionOf,
  attributionStatusList,
  currentAndWorking,
  DEFAULT_BRIEF,
  hasPreviousApproved,
  isUnnamedSpeaker,
  pollVoice,
} from "./castShared";

export interface CastDetailPanelProps {
  character: Character;
  /** Every message of the profile (the panel filters the character's own). */
  messages: Message[];
  /** The character's revisions, newest first. */
  revisions: VoiceRevision[];
  /** Detail of the newest non-approved revision (the run the panel moves through). */
  workingDetail: VoiceDetail | undefined;
  workingDetailLoading: boolean;
  workingDetailError: unknown;
  /** Detail of the highest approved revision (the take the player plays). */
  currentDetail: VoiceDetail | undefined;
  currentDetailLoading: boolean;
  currentDetailError: unknown;
  profileId: string;
  /** The profile's auto-cast dial, as the page last read it (ADR-0109). */
  autoCast: boolean;
  /** The profile's narrator fallback dial, as the page last read it
   *  (ADR-0101): the take work's other escape hatch, in the panel the way
   *  the header's dial is at the screen's level. */
  narratorFallback: boolean;
  /** Toggle the profile's narrator fallback dial (the same write as the
   *  header's switch — both read and set the profile's one flag). */
  onNarratorChange: (enabled: boolean) => void;
  /** The voice workflow is unwired on this appliance (503, docs/spec/13):
   *  the voice work degrades to a note — the roster and the facts stay. */
  voicesUnavailable: boolean;
  canCurate: boolean;
  onRefresh: () => void;
  /** Close the picker (the ✕ and the Escape key): the selection is the
   *  URL's `character` param, so closing is the parent clearing it. */
  onClose: () => void;
  /** The speaker-link resolved: the URL's selection follows the name the
   *  row now carries (ADR-0135), so the panel re-mounts on the named row. */
  onNamed?: (name: string) => void;
}

export function CastDetailPanel(p: CastDetailPanelProps) {
  const queryClient = useQueryClient();
  const { character } = p;
  const { working, current } = currentAndWorking(p.revisions);
  const previewInput = useRef<HTMLInputElement>(null);

  // The picker is a selection: the ✕ and the Escape key both clear it
  // (keyboard access to the sheet, ADR-0135). The listener is on the
  // window while the panel is mounted — the selection's own frame.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") p.onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [p.onClose]);

  // The consent record's copy button (the id is long; the line truncates):
  // the clipboard API first, a selected textarea + execCommand as the
  // fallback where the clipboard permission is unavailable (headless).
  const [consentCopied, setConsentCopied] = useState(false);
  const copyConsent = async () => {
    const text = facts?.consent_provenance ?? "";
    if (text === "") return;
    let ok = false;
    try {
      await navigator.clipboard.writeText(text);
      ok = true;
    } catch {
      try {
        const area = document.createElement("textarea");
        area.value = text;
        // CSSOM, not a style attribute (ADR-0011): the fallback target is
        // off-screen for the one frame the selection copy needs it.
        area.style.position = "fixed";
        area.style.opacity = "0";
        document.body.appendChild(area);
        area.select();
        ok = document.execCommand("copy");
        document.body.removeChild(area);
      } catch {
        ok = false;
      }
    }
    if (ok) {
      setConsentCopied(true);
      window.setTimeout(() => setConsentCopied(false), 2000);
    }
  };

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["voices"] });
    void queryClient.invalidateQueries({ queryKey: ["voice-detail"] });
  };

  // The brief: the editable input the next run carries. The panel is keyed by
  // the character id (its parent), so a remount reads that character's last
  // brief and nothing else has to reset.
  const [brief, setBrief] = useState(
    working?.design_instruction ?? current?.design_instruction ?? DEFAULT_BRIEF,
  );

  const create = useMutation({
    mutationFn: (body: { character: string; brief: string; count?: number }) =>
      voicesApi.createAuditions({ ...body, profile_id: p.profileId }),
    onSuccess: (res) => {
      refresh();
      void pollVoice((res as { voice_id: string }).voice_id, refresh);
    },
  });

  const undo = useMutation({
    mutationFn: () => voicesApi.undoVoice(working?.id ?? current?.id ?? ""),
    onSuccess: refresh,
  });

  const runBusy = working !== null && working.state !== "approved";
  const canRun = p.canCurate && !runBusy && brief.trim() !== "";

  // ── the facts ─────────────────────────────────────────────────────────────
  const lines = p.messages.filter((m) => m.speaker_id === character.id);
  const attribution = attributionOf(lines);
  const firstLine = lines[0]?.id;
  const facts = working ?? current;

  // The current take's clip: the audition the approval froze (its transcript
  // is the reference), first audition as the fallback.
  const currentAuditions = p.currentDetail?.auditions ?? [];
  const referenceAudition: VoiceAudition | null = current
    ? (currentAuditions.find((a) => a.transcript === current.ref_transcript) ??
      (currentAuditions.length > 0 ? currentAuditions[0] : null))
    : null;

  const undoAvailable =
    current !== null && working !== null && hasPreviousApproved(p.revisions, working);

  return (
    <aside className="cast-detail" data-testid="cast-detail">
      <div className="cast-detail-head">
        <NameTab testid="cast-detail-name">{character.name}</NameTab>
        <DitherCover
          tone="peach"
          testid="cast-portrait"
          label={`${character.name} portrait placeholder`}
          className="cast-portrait"
        />
        <button
          type="button"
          className="cast-detail-close"
          data-testid="cast-detail-close"
          onClick={p.onClose}
          aria-label="Close the voice picker"
        >
          ✕
        </button>
      </div>

      {/* The facts: a two-column mono definition list (docs/design/plum-peach
          2d) — label · value per line, the state pills inline beside their
          value, the long records (the consent id, the reference line)
          truncated on the line with the full text in the title and a copy
          button beside them. */}
      <dl className="facts cast-facts" data-testid="voice-state">
        <div data-testid="cast-facts-lines">
          <dt>lines</dt>
          <dd>
            {lines.length}{" "}
            {attribution === "none" ? (
              <span className="muted">no assigned lines</span>
            ) : (
              <span className={`badge badge-${attribution}`}>{attribution}</span>
            )}
            {attribution === "inferred" ? (
              <span className="muted">
                (speaker attribution is {attributionStatusList(lines)}, not confirmed)
              </span>
            ) : null}
          </dd>
        </div>
        {facts ? (
          <div data-testid="cast-facts-version">
            <dt>voice version</dt>
            <dd>
              v{facts.revision}{" "}
              <span
                className={`badge badge-${badgeClassOf(facts.state)}`}
                data-testid="voice-state-badge"
              >
                {facts.state}
              </span>
            </dd>
          </div>
        ) : null}
        {current?.ref_transcript ? (
          <div>
            <dt>reference line</dt>
            <dd data-testid="voice-reference-line">
              <code className="cast-facts-trunc" title={current.ref_transcript}>
                {current.ref_transcript}
              </code>
            </dd>
          </div>
        ) : null}
        {facts?.fingerprint ? (
          <div>
            <dt>fingerprint</dt>
            <dd>
              <code
                className="cast-facts-trunc"
                data-testid="voice-fingerprint"
                title={facts.fingerprint}
              >
                {facts.fingerprint.slice(0, 12)}…
              </code>
            </dd>
          </div>
        ) : null}
        {facts?.consent_provenance ? (
          <div>
            <dt>consent</dt>
            <dd data-testid="voice-consent">
              <code className="cast-facts-trunc" title={facts.consent_provenance}>
                {facts.consent_provenance}
              </code>
              {p.canCurate ? (
                <button
                  type="button"
                  className="cast-copy"
                  data-testid="consent-copy"
                  onClick={copyConsent}
                >
                  {consentCopied ? "copied ✓" : "copy"}
                </button>
              ) : null}
            </dd>
          </div>
        ) : null}
        {firstLine ? (
          <div data-testid="cast-facts-first-line">
            <dt>first line</dt>
            <dd>
              <code className="cast-facts-trunc" title={firstLine}>
                {firstLine.slice(0, 13)}…
              </code>
            </dd>
          </div>
        ) : null}
        {facts ? (
          <div>
            <dt>gain</dt>
            <dd>
              {p.canCurate ? (
                <VoiceGainControl key={facts.id} voiceId={facts.id} gain={facts.gain} />
              ) : (
                <span data-testid="voice-gain">{facts.gain}×</span>
              )}
            </dd>
          </div>
        ) : null}
      </dl>

      {p.voicesUnavailable ? (
        <p className="muted" data-testid="voices-unavailable">
          Voice work is not configured on this appliance (the speech endpoints are unwired) — the
          roster shows the script's characters, but takes cannot be generated here.
        </p>
      ) : p.revisions.length === 0 ? (
        <p className="muted" data-testid="no-voice">
          No voice for {character.name} yet — pick a brief and generate a take.
        </p>
      ) : null}

      {p.voicesUnavailable ? null : (
        <label className="stack">
          <span>
            Voice brief <span className="muted">(the next run carries it)</span>
          </span>
          <textarea
            data-testid="voice-brief"
            value={brief}
            onChange={(e) => setBrief(e.target.value)}
            rows={2}
          />
        </label>
      )}

      {/* The import's placeholder rows (ADR-0135): the row the source names
          only as an identifier is named here — the lines the row holds, a
          name, and the speaker-link that resolves it (an existing row of
          that exact name when one exists, a new confirmed character when it
          does not). The picker's selection follows the name the row then
          carries. */}
      {isUnnamedSpeaker(character, p.messages) && p.canCurate ? (
        <NameSpeakerBlock
          character={character}
          messages={p.messages}
          profileId={p.profileId}
          onNamed={p.onNamed}
        />
      ) : null}

      {current ? (
        <div className="cast-take" data-testid="cast-take">
          <p className="muted">
            <span className="muted">current take</span> · v{current.revision}
            {current.approved_by ? ` · by ${current.approved_by}` : ""}
          </p>
          <TakePlayer
            voiceId={current.id}
            auditionId={referenceAudition?.id ?? ""}
            label={`Current take for ${character.name}`}
          />
        </div>
      ) : null}

      {p.voicesUnavailable || !p.canCurate ? null : (
        <div className="cast-actions" data-testid="cast-actions">
          <PillButton
            variant="coral"
            testid="generate-auditions"
            disabled={!canRun || create.isPending}
            onClick={() => create.mutate({ character: character.name, brief })}
          >
            {runBusy
              ? working?.state === "draft"
                ? "Generating take…"
                : "Review the pending take below"
              : current
                ? "↻ Regenerate voice"
                : "Generate voice"}
          </PillButton>
          <PillButton
            variant="outline"
            testid="more-takes"
            disabled={!canRun || create.isPending}
            onClick={() => create.mutate({ character: character.name, brief, count: 3 })}
          >
            3 more takes
          </PillButton>
          {current ? (
            <PillButton
              variant="outline"
              testid="hear-lines"
              onClick={() => previewInput.current?.focus()}
            >
              Hear lines
            </PillButton>
          ) : null}
          {undoAvailable ? (
            <PillButton
              variant="outline"
              testid="keep-old-take"
              disabled={undo.isPending}
              onClick={() => undo.mutate()}
            >
              {undo.isPending ? "Keeping old take…" : "Keep old take (undo)"}
            </PillButton>
          ) : null}
          {/* The take work's other escape hatch (ADR-0101): with it on, this
              character's lines whose take is not yet approved speak through
              the narrator voice instead of going silent. It is the
              profile's one dial — the header's switch is the same control at
              the screen's level, so the two stay in agreement. */}
          <Switch
            className="narrator"
            label="use narrator voice"
            checked={p.narratorFallback}
            onChange={(enabled) => p.onNarratorChange(enabled)}
            testid="cast-narrator-toggle"
          />
        </div>
      )}

      {create.isError ? <ErrorState error={create.error} action="Generating the take" /> : null}
      {undo.isError ? <ErrorState error={undo.error} action="Keeping the old take" /> : null}

      {current && p.canCurate ? (
        <HearLinesPanel
          inputRef={previewInput}
          voiceId={current.id}
          characterName={character.name}
        />
      ) : null}

      <div className="cast-sample-lines" data-testid="cast-sample-lines">
        <h3>Sample lines</h3>
        {lines.length === 0 ? (
          <p className="muted">No lines are assigned to {character.name} yet.</p>
        ) : (
          <ul>
            {lines.slice(0, 3).map((m) => (
              <li key={m.id}>
                {m.speech_text || m.original_text || m.normalized_text || "(no text)"}
              </li>
            ))}
          </ul>
        )}
      </div>

      {working ? (
        <CastAuditionPanel
          voice={working}
          detail={
            working.id === current?.id
              ? p.currentDetail && p.currentDetail.id === working.id
                ? p.currentDetail
                : undefined
              : p.workingDetail && p.workingDetail.id === working.id
                ? p.workingDetail
                : undefined
          }
          detailLoading={
            working.id === current?.id ? p.currentDetailLoading : p.workingDetailLoading
          }
          detailError={working.id === current?.id ? p.currentDetailError : p.workingDetailError}
          canCurate={p.canCurate}
          canOperate={p.canCurate}
          autoCast={p.autoCast}
          onRefresh={refresh}
        />
      ) : null}
    </aside>
  );
}

function badgeClassOf(state: string): string {
  if (state === "approved") return "published";
  if (state === "retired" || state === "invalid") return "archived";
  if (state === "auditioning") return "review";
  return "draft";
}

/** "Name this speaker" (ADR-0135): the import's placeholder row — the
 *  source named its speaker only as an identifier, and the import minted a
 *  proposed row for it. The block shows the lines the row holds, takes a
 *  name, and runs the speaker-link with that name: an existing non-retired
 *  row of that exact name when one exists, a new confirmed character when
 *  it does not. The placeholder is retired either way, its lines repointed,
 *  and the picker's selection follows the name the row now carries. */
function NameSpeakerBlock({
  character,
  messages,
  profileId,
  onNamed,
}: {
  character: Character;
  messages: Message[];
  profileId: string;
  onNamed?: (name: string) => void;
}) {
  const [name, setName] = useState("");
  const [result, setResult] = useState<string | null>(null);
  const queryClient = useQueryClient();
  const link = useMutation({
    mutationFn: () => catalogApi.linkSpeaker(profileId, character.id, { name }),
    onSuccess: (res) => {
      setResult(
        res.created
          ? `named as “${res.target_name}” — a new character row`
          : `linked to the existing “${res.target_name}”`,
      );
      // The link is a roster write in its own right (docs/spec/04): the
      // placeholder retires, its lines repoint and the target's aliases
      // grow — the screen's roster and line reads catch up, and the
      // selection follows the name the row now carries.
      void queryClient.invalidateQueries({ queryKey: ["characters"] });
      if (res.target_name) onNamed?.(res.target_name);
    },
    onError: (error) =>
      setResult(error instanceof Error ? `not named: ${error.message}` : "not named"),
  });
  const lines = messages.filter((m) => m.speaker_id === character.id);
  return (
    <div className="stack cast-name-speaker" data-testid="speaker-name">
      <h3>Name this speaker</h3>
      <p className="muted">
        The source names this speaker only as <code>{character.name}</code> — its {lines.length}{" "}
        line{lines.length === 1 ? "" : "s"}:
      </p>
      <ul className="cast-name-speaker-lines">
        {lines.slice(0, 3).map((m) => (
          <li key={m.id}>{m.speech_text || m.original_text || m.normalized_text || "(no text)"}</li>
        ))}
      </ul>
      <label className="stack">
        <span>Name</span>
        <input
          data-testid="speaker-name-input"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. the lighthouse keeper"
          disabled={link.isPending}
        />
      </label>
      <PillButton
        variant="outline"
        testid="speaker-name-save"
        disabled={name.trim() === "" || link.isPending}
        onClick={() => link.mutate()}
      >
        {link.isPending ? "Naming…" : "Name this speaker"}
      </PillButton>
      {result !== null ? (
        <p className="muted" role="status" data-testid="speaker-name-result">
          {result}
        </p>
      ) : null}
    </div>
  );
}

/** One audition clip as an audio element (its SHA-256 is the ETag). */
function TakePlayer({
  voiceId,
  auditionId,
  label,
}: {
  voiceId: string;
  auditionId: string;
  label: string;
}) {
  const [src, setSrc] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [bars] = useState(() =>
    Array.from({ length: 24 }, (_, i) => 0.35 + 0.5 * Math.abs(Math.sin(i * 0.7))),
  );

  useEffect(() => {
    let live = true;
    let url: string | null = null;
    if (!auditionId) return;
    voicesApi
      .auditionAudio(voiceId, auditionId)
      .then((res) => {
        if (!live) return;
        url = URL.createObjectURL(res.blob);
        setSrc(url);
      })
      .catch((err) => {
        if (live) setError(err);
      });
    return () => {
      live = false;
      if (url) URL.revokeObjectURL(url);
    };
  }, [voiceId, auditionId]);

  if (error) return <ErrorState error={error} action="Loading the current take" />;
  if (!src) return <Loading label="Loading the current take…" />;
  return (
    <div className="cast-take-player" data-testid="cast-take-player">
      <Waveform values={bars} label={label} testid="cast-take-waveform" />
      <audio controls data-testid="cast-take-audio" src={src} />
    </div>
  );
}

/** "Hear lines": the preview player (docs/spec/07) — a sample line, the
 *  cache decision the server made, the text twin beside the audio. */
function HearLinesPanel({
  inputRef,
  voiceId,
  characterName,
}: {
  inputRef: React.RefObject<HTMLInputElement | null>;
  voiceId: string;
  characterName: string;
}) {
  const [line, setLine] = useState("The road ahead was long, but the light held.");
  const [preview, setPreview] = useState<{ url: string; cache: "HIT" | "MISS" | null } | null>(
    null,
  );
  const previewMu = useMutation({
    mutationFn: () => voicesApi.previewVoice(voiceId, line),
    onSuccess: (res) => {
      if (preview) URL.revokeObjectURL(preview.url);
      setPreview({ url: URL.createObjectURL(res.blob), cache: res.cache });
    },
  });

  return (
    <div className="stack" data-testid="preview-panel">
      <label>
        <span>
          Preview line <span className="muted">(a sample, not game text)</span>
        </span>
        <input
          ref={inputRef}
          data-testid="preview-line"
          value={line}
          onChange={(e) => setLine(e.target.value)}
        />
      </label>
      <PillButton
        variant="outline"
        testid="preview-button"
        onClick={() => previewMu.mutate()}
        disabled={line.trim() === "" || previewMu.isPending}
      >
        {previewMu.isPending ? "Synthesising…" : `Hear ${characterName}`}
      </PillButton>
      {previewMu.isError ? (
        <ErrorState error={previewMu.error} action="Previewing the voice" />
      ) : null}
      {preview ? (
        <div className="stack">
          <audio controls autoPlay data-testid="preview-player" src={preview.url} />
          <p>
            <span className="muted">cache</span>{" "}
            <span
              className={`badge ${preview.cache === "HIT" ? "badge-published" : "badge-draft"}`}
              data-testid="preview-cache"
            >
              {preview.cache ?? "unknown"}
            </span>
          </p>
        </div>
      ) : null}
    </div>
  );
}
