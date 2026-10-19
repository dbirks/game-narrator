import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router";

import { clipsApi, type Clip } from "../api/clips";
import { useAuth } from "../auth/auth-context";
import { EmptyState, ErrorState, Loading } from "../components/states";

/**
 * `/replay-clips` — the managed replay clip store (ADR-0073). A clip is a
 * recorded replay directory (the layout internal/capture/replay reads),
 * uploaded as a tar or zip and installed under the data directory the
 * session runtime always treats as a replay root. The surface is
 * developer-scoped: a clip is a recording of gameplay frames, the same
 * evidence class as the dev-trace store, so it lives where developer
 * surfaces live (docs/spec/08). The provenance row is the archive's content
 * hash; no host path is shown — the id is the selection key.
 */

export function ReplayClipsPage() {
  const { atLeast } = useAuth();
  const developer = atLeast("developer");
  const client = useQueryClient();
  const [file, setFile] = useState<File | null>(null);
  const [name, setName] = useState("");
  const [lastUpload, setLastUpload] = useState<string | null>(null);

  const list = useQuery({
    queryKey: ["replay-clips", "list"],
    queryFn: ({ signal }) => clipsApi.list(signal),
    enabled: developer,
  });

  const upload = useMutation({
    mutationFn: ({ file, name }: { file: File; name: string }) => clipsApi.upload(file, name),
    onSuccess: (resp) => {
      setLastUpload(`${resp.id}: ${resp.detail}`);
      setFile(null);
      setName("");
      void client.invalidateQueries({ queryKey: ["replay-clips", "list"] });
    },
  });

  const remove = useMutation({
    mutationFn: (id: string) => clipsApi.remove(id),
    onSuccess: () => {
      setLastUpload(null);
      void client.invalidateQueries({ queryKey: ["replay-clips", "list"] });
    },
  });

  if (!developer) {
    return (
      <main>
        <h1 data-testid="replay-clips-title">Replay clips</h1>
        <section className="panel" data-testid="replay-clips-role">
          <p role="alert">
            The clip store is a developer surface: sign in with a developer or administrator account
            to upload, list or delete clips.
          </p>
        </section>
      </main>
    );
  }

  const clips = list.data?.clips ?? [];

  return (
    <main>
      <h1 data-testid="replay-clips-title">Replay clips</h1>
      <section className="panel" data-testid="clips-upload-panel">
        <p className="muted">
          Upload a tar or zip of a replay layout (a <code>timestamps.json</code> manifest plus frame
          images, the layout the replay source reads). The archive lands in the asset store under
          its content hash and the clip installs under the managed store;{" "}
          <Link to="/play">the Play page</Link> then lists it in the frame-source picker.
        </p>
        <form
          className="stack"
          onSubmit={(event) => {
            event.preventDefault();
            if (file) upload.mutate({ file, name });
          }}
          data-testid="clips-upload-form"
        >
          <label>
            Archive
            <input
              type="file"
              accept=".tar,.zip"
              data-testid="clips-file"
              onChange={(event) => {
                const picked = event.target.files?.[0] ?? null;
                setFile(picked);
              }}
            />
          </label>
          <label>
            Clip id
            <input
              type="text"
              value={name}
              placeholder="lowercase slug; optional"
              data-testid="clips-name"
              onChange={(event) => setName(event.target.value)}
            />
            <span className="muted">
              Without a name the id comes from the archive&apos;s top-level directory, then the
              filename.
            </span>
          </label>
          <button type="submit" disabled={!file || upload.isPending} data-testid="clips-upload">
            {upload.isPending ? "Uploading…" : "Upload clip"}
          </button>
        </form>
        {upload.isError ? <ErrorState error={upload.error} action="The upload failed" /> : null}
        {lastUpload ? (
          <p className="muted" role="status" data-testid="clips-upload-result">
            {lastUpload}
          </p>
        ) : null}
      </section>

      <section className="panel" data-testid="clips-list">
        <h2>Installed clips</h2>
        {list.isPending ? <Loading label="Listing clips…" /> : null}
        {list.isError ? (
          <ErrorState
            error={list.error}
            action="Cannot list the clip store"
            onRetry={() => void list.refetch()}
          />
        ) : null}
        {list.data && clips.length === 0 ? (
          <EmptyState title="No clips installed">
            <p>
              Upload an archive above, or start a session with a replay directory from the
              configured allowlist.
            </p>
          </EmptyState>
        ) : null}
        {list.data && clips.length > 0 ? (
          <table className="stats-table" data-testid="clips-table">
            <thead>
              <tr>
                <th>Id</th>
                <th>Name</th>
                <th>Frames</th>
                <th>Format</th>
                <th>Archive</th>
                <th>Installed</th>
                <th>By</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {clips.map((clip) => (
                <ClipRow
                  key={clip.id}
                  clip={clip}
                  onRemove={remove.mutate}
                  removing={remove.isPending}
                />
              ))}
            </tbody>
          </table>
        ) : null}
        {remove.isError ? <ErrorState error={remove.error} action="The delete failed" /> : null}
      </section>
    </main>
  );
}

function ClipRow({
  clip,
  onRemove,
  removing,
}: {
  clip: Clip;
  onRemove: (id: string) => void;
  removing: boolean;
}) {
  return (
    <tr data-testid={`clip-row-${clip.id}`}>
      <td>
        <code>{clip.id}</code>
      </td>
      <td>{clip.name ?? ""}</td>
      <td data-testid={`clip-frames-${clip.id}`}>{clip.missing ? "—" : clip.frames}</td>
      <td>{clip.archive_format}</td>
      <td>
        <span title={clip.archive_sha256} data-testid={`clip-hash-${clip.id}`}>
          {clip.archive_sha256.slice(0, 12)}… · {clip.archive_size} B
        </span>
      </td>
      <td>{new Date(clip.created_at).toLocaleString()}</td>
      <td>{clip.created_by ?? ""}</td>
      <td>
        <button
          type="button"
          disabled={removing}
          onClick={() => onRemove(clip.id)}
          data-testid={`clip-delete-${clip.id}`}
        >
          {clip.missing ? "Remove row" : "Delete"}
        </button>
      </td>
    </tr>
  );
}
