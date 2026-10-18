import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router";
import { useSearchParams } from "react-router";

import { api, catalogApi, type Edition, type Profile } from "../api/client";
import {
  voicesApi,
  type VoiceAudition,
  type VoiceRegistration,
  type VoiceRevision,
} from "../api/voices";
import { useAuth } from "../auth/auth-context";
import { EmptyState, ErrorState, Loading } from "../components/states";
/**
 * `/games/:id/voices` — the voice setup workflow (docs/spec/07, docs/spec/09,
 * docs/spec/14). Characters of the game's current profile, the editable voice
 * brief, the VoiceDesign auditions side by side, the consent-gated approval,
 * the per-instance registration state, and a preview line that shows the
 * speech-cache decision the server made (HIT/MISS).
 *
 * Every audio control has its text equivalent beside it (docs/spec/09
 * accessibility): the transcript each clip speaks, the cache decision, the
 * registration state. The lifecycle rules are the server's: an approval the
 * server would refuse (without consent, or a second time) is the server's
 * problem document, rendered in the server's words.
 */

const DEFAULT_BRIEF =
  "A clear, warm narrator voice with a steady cadence, suited to original synthetic dialogue.";

export function VoicesPage() {
  const { id: gameId = "" } = useParams();
  const { atLeast } = useAuth();
  const [params, setParams] = useSearchParams();
  const profileId = params.get("profile") ?? "";
  const character = params.get("character") ?? "";
  const queryClient = useQueryClient();

  // The script belongs to one revision of one edition; with no ?profile= pick
  // the newest, the same way the dialogue page does, so a link from the
  // overview lands on the same revision.
  const fallback = useDefaultProfile(gameId, profileId === "");
  useEffect(() => {
    if (profileId === "" && fallback) {
      const next = new URLSearchParams(params);
      next.set("profile", fallback);
      setParams(next, { replace: true });
    }
  }, [profileId, fallback, params, setParams]);

  const characters = useQuery({
    queryKey: ["characters", profileId],
    queryFn: ({ signal }) =>
      api.get("/api/v1/profiles/{id}/characters", { params: { id: profileId }, signal }),
    enabled: profileId !== "",
  });
  const roster =
    (characters.data as { characters: { id: string; name: string; status: string }[] } | undefined)
      ?.characters ?? [];

  const voices = useQuery({
    queryKey: ["voices", character],
    queryFn: ({ signal }) => voicesApi.listVoices(character || undefined, signal),
    enabled: character !== "",
  });
  // The newest revision is the one the page works on.
  const current = useMemo<VoiceRevision | null>(() => {
    const list = [...(voices.data?.voices ?? [])].sort((a, b) => b.revision - a.revision);
    return list[0] ?? null;
  }, [voices.data]);

  const detail = useQuery({
    queryKey: ["voice-detail", current?.id],
    queryFn: ({ signal }) => voicesApi.getVoice(current?.id as string, signal),
    enabled: current?.id !== undefined && current?.id !== "",
  });

  const setCharacter = (slug: string) => {
    const next = new URLSearchParams(params);
    if (slug) next.set("character", slug);
    else next.delete("character");
    setParams(next, { replace: true });
  };

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["voices"] });
    void queryClient.invalidateQueries({ queryKey: ["voice-detail"] });
  };

  const [brief, setBrief] = useState(DEFAULT_BRIEF);
  const [selected, setSelected] = useState<string | null>(null);
  const [consent, setConsent] = useState("");
  const [previewLine, setPreviewLine] = useState("The road ahead was long, but the light held.");
  const [preview, setPreview] = useState<{ url: string; cache: "HIT" | "MISS" | null } | null>(
    null,
  );

  useEffect(() => {
    if (current?.design_instruction) setBrief(current.design_instruction);
    setSelected(null);
    setConsent("");
  }, [current?.id, current?.design_instruction]);

  const auditions = detail.data?.auditions ?? [];
  const registrations = detail.data?.registrations ?? [];

  const canGenerate =
    character !== "" && (!current || current.state === "draft" || current.state === "auditioning");

  // The design attempt: one brief, one click. A character with no voice yet
  // gets its draft here; an auditioning revision gets a *new* attempt (the
  // domain's rule: a revision's content freezes at approval, so a changed
  // brief means a new revision, not an edit).
  const create = useMutation({
    mutationFn: () => voicesApi.createAuditions({ character, brief }),
    onSuccess: (res) => void pollVoice(res.voice_id, refresh),
  });

  return (
    <main>
      <header className="page-header">
        <h1>Voices</h1>
        <p className="muted">
          <Link to={`/games/${gameId}/overview`} data-testid="voices-back">
            Overview
          </Link>
          {character ? ` · ${character}` : ""}
        </p>
      </header>

      {characters.isError ? (
        <ErrorState error={characters.error} action="Loading the roster" />
      ) : characters.isLoading ? (
        <Loading label="Loading characters…" />
      ) : roster.length === 0 ? (
        <EmptyState title="No characters yet">
          <p className="muted">
            Import a script with a speaker column and the roster appears here.
          </p>
        </EmptyState>
      ) : (
        <div className="stack" data-testid="voice-characters">
          {roster.map((c: { id: string; name: string; status: string }) => (
            <label key={c.id} className="voice-character">
              <input
                type="radio"
                name="voice-character"
                checked={character === c.name}
                onChange={() => setCharacter(c.name)}
              />{" "}
              {c.name}
              <span className="badge badge-draft">{c.status}</span>
            </label>
          ))}
        </div>
      )}

      {character && voices.isError ? (
        <ErrorState error={voices.error} action="Loading voices" onRetry={refresh} />
      ) : null}

      {character && canGenerate && atLeast("curator") ? (
        <div className="stack" data-testid="voice-setup">
          {character && !current && !voices.isLoading && !characters.isLoading ? (
            <p className="muted" data-testid="no-voice">
              No voice for {character} yet — pick a brief and generate auditions.
            </p>
          ) : null}
          <label className="stack">
            Voice brief
            <textarea
              data-testid="voice-brief"
              value={brief}
              onChange={(e) => setBrief(e.target.value)}
              rows={2}
              disabled={current?.state === "draft"}
            />
          </label>
          <button
            type="button"
            data-testid="generate-auditions"
            onClick={() => create.mutate()}
            disabled={create.isPending || brief.trim() === ""}
          >
            {create.isPending || current?.state === "draft"
              ? "Generating auditions…"
              : "Generate 3 auditions"}
          </button>
          {create.isError ? (
            <ErrorState error={create.error} action="Generating auditions" />
          ) : null}
        </div>
      ) : null}

      {current ? (
        <VoicePanel
          key={current.id}
          voice={current}
          selected={selected}
          setSelected={setSelected}
          consent={consent}
          setConsent={setConsent}
          previewLine={previewLine}
          setPreviewLine={setPreviewLine}
          preview={preview}
          setPreview={setPreview}
          auditions={auditions}
          registrations={registrations}
          detailLoading={detail.isLoading}
          detailError={detail.error}
          canCurate={atLeast("curator")}
          canOperate={atLeast("operator")}
          refresh={refresh}
        />
      ) : null}
    </main>
  );
}

interface PanelProps {
  voice: VoiceRevision;
  selected: string | null;
  setSelected: (v: string | null) => void;
  consent: string;
  setConsent: (v: string) => void;
  previewLine: string;
  setPreviewLine: (v: string) => void;
  preview: { url: string; cache: "HIT" | "MISS" | null } | null;
  setPreview: (v: { url: string; cache: "HIT" | "MISS" | null } | null) => void;
  auditions: VoiceAudition[];
  registrations: VoiceRegistration[];
  detailLoading: boolean;
  detailError: unknown;
  canCurate: boolean;
  canOperate: boolean;
  refresh: () => void;
}

function VoicePanel(p: PanelProps) {
  const { voice } = p;
  const auditioning = voice.state === "auditioning";
  const consentMissing = p.consent.trim() === "";

  const approve = useMutation({
    mutationFn: () =>
      voicesApi.approveVoice(voice.id, { audition_id: p.selected as string, consent: p.consent }),
    onSuccess: () => {
      p.setConsent("");
      p.setSelected(null);
      p.refresh();
    },
  });
  const register = useMutation({
    mutationFn: () => voicesApi.registerVoice(voice.id),
    onSuccess: p.refresh,
  });
  const previewMu = useMutation({
    mutationFn: () => voicesApi.previewVoice(voice.id, p.previewLine),
    onSuccess: (res) => {
      if (p.preview) URL.revokeObjectURL(p.preview.url);
      p.setPreview({ url: URL.createObjectURL(res.blob), cache: res.cache });
    },
  });

  return (
    <section className="stack">
      <div className="facts" data-testid="voice-state">
        <div>
          <span className="muted">revision</span> v{voice.revision}
        </div>
        <div>
          <span className="muted">state</span>{" "}
          <span
            className={`badge badge-${badgeClass(voice.state)}`}
            data-testid="voice-state-badge"
          >
            {voice.state}
          </span>
        </div>
        {voice.ref_transcript ? (
          <div data-testid="voice-reference-line">
            <span className="muted">reference line</span> {voice.ref_transcript}
          </div>
        ) : null}
        {voice.fingerprint ? (
          <div>
            <span className="muted">fingerprint</span>{" "}
            <code data-testid="voice-fingerprint">{voice.fingerprint.slice(0, 12)}…</code>
          </div>
        ) : null}
        {voice.consent_provenance ? (
          <div data-testid="voice-consent">
            <span className="muted">consent</span> {voice.consent_provenance}
          </div>
        ) : null}
      </div>

      {p.detailLoading ? <Loading label="Loading audition clips…" /> : null}
      {p.detailError ? <ErrorState error={p.detailError} action="Loading audition clips" /> : null}

      {auditioning && p.auditions.length > 0 ? (
        <div className="cards" data-testid="auditions">
          {p.auditions.map((a, i) => (
            <AuditionCard
              key={a.id}
              audition={a}
              index={i}
              voiceId={voice.id}
              selected={p.selected === a.id}
              onSelect={() => p.setSelected(a.id)}
            />
          ))}
        </div>
      ) : null}

      {approve.isError ? <ErrorState error={approve.error} action="Approving the voice" /> : null}

      {auditioning && p.canCurate ? (
        <div className="stack" data-testid="approve-panel">
          <label>
            Consent record <span className="muted">(required — docs/spec/14)</span>
            <input
              data-testid="consent-input"
              value={p.consent}
              onChange={(e) => p.setConsent(e.target.value)}
              placeholder="original_synthetic_design"
            />
          </label>
          <button
            type="button"
            data-testid="approve-voice"
            onClick={() => approve.mutate()}
            disabled={consentMissing || p.selected === null}
          >
            {approve.isPending ? "Approving…" : "Approve selected audition"}
          </button>
          {consentMissing ? (
            <p className="muted" data-testid="consent-required-note">
              A meaningful consent record is required to approve.
            </p>
          ) : null}
        </div>
      ) : null}

      <div className="stack" data-testid="registrations">
        <h3>Registration</h3>
        {p.registrations.length === 0 ? (
          <p className="muted">Not registered on any instance yet.</p>
        ) : (
          p.registrations.map((r) => (
            <div key={r.instance_id} className="facts">
              <div>
                <span className="muted">instance</span> {r.instance_id}
              </div>
              <div>
                <span className="muted">name</span>{" "}
                <code data-testid="registration-name">{r.registered_name || "—"}</code>
              </div>
              <div>
                <span className="muted">state</span>{" "}
                <span
                  className={`badge badge-${r.state === "ready" ? "published" : r.state === "invalid" ? "archived" : "draft"}`}
                  data-testid={`registration-state-${r.instance_id}`}
                >
                  {r.state}
                </span>
                {r.err ? <div className="muted">{r.err}</div> : null}
              </div>
            </div>
          ))
        )}
        {voice.state === "approved" && p.canOperate ? (
          <button type="button" data-testid="register-voice" onClick={() => register.mutate()}>
            {register.isPending ? "Registering…" : "Register on this instance"}
          </button>
        ) : null}
        {register.isError ? (
          <ErrorState error={register.error} action="Registering the voice" />
        ) : null}
      </div>

      {voice.state === "approved" && p.canCurate ? (
        <div className="stack" data-testid="preview-panel">
          <label>
            Preview line
            <input
              data-testid="preview-line"
              value={p.previewLine}
              onChange={(e) => p.setPreviewLine(e.target.value)}
            />
          </label>
          <button
            type="button"
            data-testid="preview-button"
            onClick={() => previewMu.mutate()}
            disabled={p.previewLine.trim() === "" || previewMu.isPending}
          >
            {previewMu.isPending ? "Synthesising…" : "Preview"}
          </button>
          {previewMu.isError ? (
            <ErrorState error={previewMu.error} action="Previewing the voice" />
          ) : null}
          {p.preview ? (
            <div className="stack">
              <audio controls autoPlay data-testid="preview-player" src={p.preview.url} />
              <p>
                <span className="muted">cache</span>{" "}
                <span
                  className={`badge ${p.preview.cache === "HIT" ? "badge-published" : "badge-draft"}`}
                  data-testid="preview-cache"
                >
                  {p.preview.cache ?? "unknown"}
                </span>
              </p>
            </div>
          ) : null}
        </div>
      ) : null}
    </section>
  );
}

/** Poll one revision until it leaves the draft state (the audition job lands
 *  the clips), then hand the refresh off. Bounded: a job that never lands is a
 *  server fault, not a page that spins forever. */
async function pollVoice(voiceId: string, done: () => void, tries = 24) {
  for (let i = 0; i < tries; i++) {
    try {
      const d = await voicesApi.getVoice(voiceId);
      if (d.state !== "draft") {
        done();
        return;
      }
    } catch {
      // The draft row may lag the 202 by a tick; the poll retries rather than
      // treating a 404 as a failure.
    }
    await new Promise((r) => setTimeout(r, 750));
  }
  done();
}

function badgeClass(state: string): string {
  if (state === "approved" || state === "ready") return "published";
  if (state === "invalid" || state === "retired") return "archived";
  if (state === "auditioning") return "review";
  return "draft";
}

function AuditionCard({
  audition,
  index,
  voiceId,
  selected,
  onSelect,
}: {
  audition: VoiceAudition;
  index: number;
  voiceId: string;
  selected: boolean;
  onSelect: () => void;
}) {
  const [src, setSrc] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<unknown>(null);

  useEffect(() => {
    let live = true;
    let url: string | null = null;
    voicesApi
      .auditionAudio(voiceId, audition.id)
      .then((res) => {
        if (!live) return;
        url = URL.createObjectURL(res.blob);
        setSrc(url);
      })
      .catch((err) => {
        if (live) setLoadError(err);
      });
    return () => {
      live = false;
      if (url) URL.revokeObjectURL(url);
    };
  }, [voiceId, audition.id]);

  return (
    <div className="card" data-testid={`audition-card-${index}`}>
      <p className="muted">
        candidate {index + 1} · <code>{audition.model_id}</code>
        {audition.latency_ms ? ` · ${audition.latency_ms} ms` : ""}
      </p>
      {loadError ? (
        <ErrorState error={loadError} action="Loading clip" />
      ) : src ? (
        <audio controls data-testid={`audition-player-${index}`} src={src} />
      ) : (
        <Loading label="Loading clip…" />
      )}
      <p data-testid={`audition-transcript-${index}`}>{audition.transcript}</p>
      {/* ASR QA verdict (FR-10, ADR 0061): the failed state is the flag —
          the badge shows the WER and the recognised transcript as the review
          evidence, and the backend refuses approval until a human has looked. */}
      {audition.qa_state === "failed" ? (
        <p className="muted">
          <span className="badge badge-failed" data-testid={`qa-flag-${index}`}>
            QA flagged · wer {audition.qa_wer?.toFixed(2) ?? "—"}
          </span>{" "}
          {audition.qa_model ? (
            <code data-testid={`qa-model-${index}`}>{audition.qa_model}</code>
          ) : null}
        </p>
      ) : audition.qa_state === "passed" ? (
        <p className="muted">
          <span className="badge badge-published" data-testid={`qa-pass-${index}`}>
            QA passed · wer {audition.qa_wer?.toFixed(2) ?? "—"}
          </span>
        </p>
      ) : (
        <p className="muted">
          <span className="badge badge-draft" data-testid={`qa-notrun-${index}`}>
            QA not run
          </span>
        </p>
      )}
      {audition.qa_state === "failed" && audition.qa_transcript ? (
        <p className="muted" data-testid={`qa-transcript-${index}`}>
          <span className="muted">ASR heard</span> “{audition.qa_transcript}”
        </p>
      ) : null}
      <button
        type="button"
        onClick={onSelect}
        data-testid={`audition-approve-${index}`}
        aria-pressed={selected}
      >
        {selected ? "Selected" : "Select this clip"}
      </button>
    </div>
  );
}

// The page's own default-profile resolver: the newest revision of the
// first edition, the same rule the dialogue page applies.
function useDefaultProfile(gameId: string, enabled: boolean): string | null {
  const editions = useQuery({
    queryKey: ["editions", gameId],
    queryFn: ({ signal }) => catalogApi.listEditions(gameId, signal),
    enabled,
  });
  const editionId = (editions.data as { editions: Edition[] } | undefined)?.editions?.[0]?.id;
  const profiles = useQuery({
    queryKey: ["profiles", editionId],
    queryFn: ({ signal }) => catalogApi.listProfiles(editionId as string, signal),
    enabled: editionId !== undefined,
  });
  return useMemo(() => {
    const list = (profiles.data as { profiles: Profile[] } | undefined)?.profiles ?? [];
    if (list.length === 0) return null;
    const sorted = [...list].sort((a, b) =>
      a.revision_num > b.revision_num ? -1 : a.revision_num < b.revision_num ? 1 : 0,
    );
    return sorted[0]?.id ?? null;
  }, [profiles.data]);
}
