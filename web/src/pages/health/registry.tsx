import { useState } from "react";

import { playTestTone, type AudioDevicesView } from "../../api/audio";
import { ApiError } from "../../api/client";
import { GatedError, gatedJson } from "../../api/diagnostics";
import { useGated } from "./gated";

/**
 * `src/pages/health/registry.tsx` — the voice registry and the speaker
 * test, moved verbatim from the pre-redesign /diagnostics page (ADR-0111
 * §1): the single-node registry's per-voice registration states, and the
 * consented test tone on the selected output.
 */

const loadVoices = (signal: AbortSignal) =>
  gatedJson<{
    voices: {
      id: string;
      character: string;
      revision: number;
      state: string;
      registrations?: {
        instance_id: string;
        registered_name: string;
        state: string;
        err?: string;
        last_verified_at?: string;
      }[];
    }[];
  }>("/api/v1/voices", undefined, signal);

const loadAudio = (signal: AbortSignal) =>
  gatedJson<AudioDevicesView>("/api/v1/audio/devices", undefined, signal);

export function VoiceRegistrySection() {
  const voices = useGated(loadVoices, true);

  return (
    <section className="panel" data-testid="voice-registry">
      <h2>Voice registry</h2>
      <p className="muted" data-testid="voice-registry-single-node">
        This build is a single node: there is no peer replication to verify. The registry below is
        this appliance&apos;s voice registry, with each voice&apos;s registration state on the
        provider it was registered with.
      </p>
      {voices.state.kind === "loading" ? (
        <p className="muted">Reading the voice registry…</p>
      ) : voices.state.kind === "unauthenticated" ? (
        <p className="muted" data-testid="voice-registry-sign-in">
          The voice registry needs a signed-in session (viewer role or higher).{" "}
          <a href="/login">Sign in</a> to see it.
        </p>
      ) : voices.state.kind === "forbidden" ? (
        <p className="muted" data-testid="voice-registry-role">
          Reading the voice registry needs at least the viewer role.
        </p>
      ) : voices.state.kind === "error" ? (
        <div role="alert" className="error" data-testid="voice-registry-error">
          <p>
            The registry read answered HTTP {voices.state.status}: {voices.state.detail}
          </p>
          <button type="button" onClick={voices.refetch}>
            Try again
          </button>
        </div>
      ) : (
        <VoiceRegistryFacts voices={voices.state.body.voices} />
      )}
    </section>
  );
}

function VoiceRegistryFacts({
  voices,
}: {
  voices: Awaited<ReturnType<typeof loadVoices>>["voices"];
}) {
  if (voices.length === 0) {
    return (
      <p className="muted" data-testid="voice-registry-empty">
        No voices exist yet — the registry is empty, which is the honest state before the first
        audition.
      </p>
    );
  }
  return (
    <div className="stack">
      <p data-testid="voice-registry-count">
        {voices.length} voice{voices.length === 1 ? "" : "s"} in the registry.
      </p>
      <table>
        <thead>
          <tr>
            <th>Character</th>
            <th>Revision</th>
            <th>State</th>
            <th>Registrations</th>
          </tr>
        </thead>
        <tbody>
          {voices.map((voice) => (
            <tr key={voice.id} data-testid="voice-row">
              <td>{voice.character}</td>
              <td>r{voice.revision}</td>
              <td>
                <span
                  className={`badge ${voice.state === "approved" ? "badge-succeeded" : "badge-plain"}`}
                >
                  {voice.state}
                </span>
              </td>
              <td>
                {(voice.registrations ?? []).length === 0
                  ? "not registered"
                  : (voice.registrations ?? [])
                      .map(
                        (registration) =>
                          `${registration.state}${registration.err ? ` (${registration.err})` : ""}`,
                      )
                      .join(" · ")}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function SpeakerTestSection() {
  const audio = useGated(loadAudio, true);
  const [tone, setTone] = useState<Awaited<ReturnType<typeof playTestTone>> | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<GatedError | null>(null);

  const play = () => {
    setBusy(true);
    setError(null);
    setTone(null);
    playTestTone()
      .then((response) => {
        setTone(response);
      })
      .catch((caught) => {
        setError(
          caught instanceof ApiError
            ? new GatedError(caught.status, `${caught.title} — ${caught.detail}`)
            : new GatedError(0, String(caught)),
        );
      })
      .finally(() => {
        setBusy(false);
      });
  };

  return (
    <section className="panel" data-testid="speaker-test">
      <h2>Speaker test</h2>
      <p className="muted">
        Plays the deterministic test tone (1.0 s / 440 Hz) on the selected output. Playback is a
        consented action (docs/spec/13): the tone only plays from this button.
      </p>
      {audio.state.kind === "loading" ? (
        <p className="muted">Enumerating the audio outputs…</p>
      ) : audio.state.kind === "unauthenticated" ? (
        <p className="muted" data-testid="speaker-test-sign-in">
          The speaker test needs a signed-in session (operator role or higher).{" "}
          <a href="/login">Sign in</a> to play the tone.
        </p>
      ) : audio.state.kind === "forbidden" ? (
        <p className="muted" data-testid="speaker-test-role">
          Playing the tone needs the operator role or higher; this session does not hold it.
        </p>
      ) : audio.state.kind === "error" ? (
        <div role="alert" className="error" data-testid="speaker-test-error">
          <p>
            The audio read answered HTTP {audio.state.status}: {audio.state.detail}
          </p>
          <button type="button" onClick={audio.refetch}>
            Try again
          </button>
        </div>
      ) : audio.state.body.devices.length === 0 ? (
        <p className="muted" data-testid="speaker-test-none">
          No audio outputs found — the tone cannot play on this machine.
        </p>
      ) : (
        <button type="button" onClick={play} disabled={busy} data-testid="speaker-test-play">
          {busy ? "Playing…" : "Play the test tone"}
        </button>
      )}
      {error ? (
        <div role="alert" className="error" data-testid="speaker-test-failed">
          <p>
            The test tone failed (HTTP {error.status}): {error.message}
          </p>
        </div>
      ) : null}
      {tone ? (
        <p data-testid="speaker-test-result">
          Played on <strong>{tone.device}</strong> via sink <code>{tone.sink}</code> for{" "}
          {tone.duration_ms} ms.
          {tone.evidence_file ? ` Recorded evidence: ${tone.evidence_file}.` : ""}
          {tone.note ? ` ${tone.note}` : ""}
        </p>
      ) : null}
    </section>
  );
}
