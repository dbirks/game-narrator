/**
 * `src/pages/cast/CastAuditions.tsx` — the voice-run half of the Cast
 * detail panel (docs/design/plum-peach 2d; carried over from the former
 * Voices page after the ADR-0093 merge): while a run is in flight, the
 * audition clips side by side (each with its transcript text twin and its
 * ASR QA verdict, ADR-0061) and the consent-gated approval (docs/spec/14);
 * once the take is approved (or auto-approved, ADR-0109), the per-instance
 * registration state. The lifecycle rules are the server's: an approval the
 * server would refuse is the server's problem document, rendered in the
 * server's words.
 */

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { setVoiceGain } from "../../api/audio";
import {
  voicesApi,
  type VoiceAudition,
  type VoiceRegistration,
  type VoiceRevision,
} from "../../api/voices";
import { ErrorState, Loading } from "../../components/states";

export interface AuditionPanelProps {
  /** The working revision: the draft/auditioning run in flight, or the
   *  approved current (the panel then shows the registration state). */
  voice: VoiceRevision;
  /** The whole character's revisions (the approval refresh re-derives current). */
  detail: { auditions: VoiceAudition[]; registrations?: VoiceRegistration[] } | undefined;
  detailLoading: boolean;
  detailError: unknown;
  canCurate: boolean;
  canOperate: boolean;
  /** The profile's auto-cast dial (ADR-0109): off at job end is the honest
   *  "toggled after the run" pending case; on means the QA read-back
   *  flagged the take and a human review is the only way through. */
  autoCast: boolean;
  onRefresh: () => void;
}

export function CastAuditionPanel(p: AuditionPanelProps) {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<string | null>(null);
  const [consent, setConsent] = useState("");
  const auditioning = p.voice.state === "auditioning";
  const consentMissing = consent.trim() === "";

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["voices"] });
    void queryClient.invalidateQueries({ queryKey: ["voice-detail"] });
  };

  const approve = useMutation({
    mutationFn: () =>
      voicesApi.approveVoice(p.voice.id, { audition_id: selected as string, consent }),
    onSuccess: () => {
      setConsent("");
      setSelected(null);
      refresh();
    },
  });
  const register = useMutation({
    mutationFn: () => voicesApi.registerVoice(p.voice.id),
    onSuccess: refresh,
  });

  const auditions = p.detail?.auditions ?? [];
  const registrations = p.detail?.registrations ?? [];

  return (
    <div className="stack" data-testid="cast-audition-panel">
      {p.voice.state === "draft" ? (
        <p className="muted" data-testid="take-queued">
          Take queued — the design job is running; the clips land here when it finishes.
        </p>
      ) : null}
      {p.voice.state === "auditioning" && p.autoCast === false ? (
        <p className="muted" data-testid="auto-cast-pending-note">
          Auto-cast is off for this profile, so the take waits for a pick and an approval below.
        </p>
      ) : null}
      {p.voice.state === "auditioning" && p.autoCast === true ? (
        <p className="muted" data-testid="auto-cast-pending-note">
          Auto-cast could not approve this take — the ASR QA read-back flagged it. Review the
          evidence below and approve it yourself, or regenerate.
        </p>
      ) : null}

      {p.detailLoading ? <Loading label="Loading audition clips…" /> : null}
      {p.detailError ? <ErrorState error={p.detailError} action="Loading audition clips" /> : null}

      {auditioning && auditions.length > 0 ? (
        <div className="cards" data-testid="auditions">
          {auditions.map((a, i) => (
            <AuditionCard
              key={a.id}
              audition={a}
              index={i}
              voiceId={p.voice.id}
              selected={selected === a.id}
              onSelect={() => setSelected(a.id)}
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
              value={consent}
              onChange={(e) => setConsent(e.target.value)}
              placeholder="original_synthetic_design"
            />
          </label>
          <button
            type="button"
            data-testid="approve-voice"
            onClick={() => approve.mutate()}
            disabled={consentMissing || selected === null || approve.isPending}
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
        {registrations.length === 0 ? (
          p.detailLoading ? (
            <Loading label="Loading the registration state…" />
          ) : (
            <p className="muted">Not registered on any instance yet.</p>
          )
        ) : (
          registrations.map((r) => (
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
        {p.voice.state === "approved" && p.canOperate ? (
          <button type="button" data-testid="register-voice" onClick={() => register.mutate()}>
            {register.isPending ? "Registering…" : "Register on this instance"}
          </button>
        ) : null}
        {register.isError ? (
          <ErrorState error={register.error} action="Registering the voice" />
        ) : null}
      </div>
    </div>
  );
}

/** The per-voice output gain (docs/spec/07, ADR-0074): one linear factor,
 *  0..10. Saving never re-synthesises — the runtime applies it at playback —
 *  so the control sits on the facts' gain row (its dt carries the "gain"
 *  label): an ink track with the coral thumb and the value read-out, the
 *  "applied at playback" note set on its own line so it cannot overlap the
 *  slider. */
export function VoiceGainControl({ voiceId, gain }: { voiceId: string; gain: number }) {
  const [value, setValue] = useState(gain);
  const [note, setNote] = useState<string | null>(null);
  const save = useMutation({
    mutationFn: (g: number) => setVoiceGain(voiceId, g),
    onSuccess: (res) => setNote(`gain ${res.gain}× saved`),
    onError: (error) => setNote(`not saved: ${error instanceof Error ? error.message : ""}`),
  });

  return (
    <span className="cast-gain" data-testid="voice-gain">
      <input
        data-testid="voice-gain-slider"
        aria-label="voice gain (applied at playback)"
        type="range"
        min={0}
        max={10}
        step={0.1}
        value={value}
        onChange={(event) => {
          setValue(Number(event.target.value));
          setNote(null);
        }}
        onMouseUp={() => save.mutate(value)}
        onKeyUp={() => save.mutate(value)}
        onBlur={() => save.mutate(value)}
      />
      <span className="cast-gain-value" data-testid="voice-gain-value">
        {value.toFixed(1)}×
      </span>
      <span className="muted cast-gain-note" data-testid="voice-gain-note">
        applied at playback; no re-synthesis
      </span>
      {note ? (
        <span role="status" className="muted" data-testid="voice-gain-note-status">
          {note}
        </span>
      ) : null}
    </span>
  );
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
