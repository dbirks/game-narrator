import type { Settings } from "../../api/settings";
import { Card } from "../../components/kit";

/**
 * `/settings` "Audio" (ADR-0109 restyle, ADR-0074): the read-only half of
 * the audio surface — the sink in force, the device it resolves to and the
 * variables that control them. The runtime-mutable half (the device
 * selection, the master volume, the test tone) is the "Audio output" card
 * below; this one is what the environment and the build fix.
 */
export function AudioInfoSection({ data }: { data: Settings }) {
  const audio = data.audio;
  return (
    <Card title="Audio" testid="settings-card-audio">
      <dl className="grid-defs" data-testid="settings-audio">
        <dt>Sink</dt>
        <dd>
          {audio.sink}
          {audio.note ? ` — ${audio.note}` : ""}
        </dd>
        <dt>Device</dt>
        <dd>{audio.device || "default"}</dd>
        <dt>Variables</dt>
        <dd>{audio.env.join(", ")}</dd>
      </dl>
    </Card>
  );
}
