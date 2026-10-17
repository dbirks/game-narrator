import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import * as TabsPrimitive from "@radix-ui/react-tabs";
import { useEffect, useState, type ChangeEvent, type FormEvent } from "react";
import { Link, useParams } from "react-router";

import {
  ApiError,
  catalogApi,
  jobsApi,
  portabilityApi,
  refreshProfileEtag,
  type ColumnMapping,
  type Edition,
  type Job,
  type JobDetail,
  type Profile,
  type RightsStatus,
} from "../api/client";
import { useAuth } from "../auth/auth-context";
import { Modal } from "../components/Modal";
import { EmptyState, ErrorState, Loading, ProblemList } from "../components/states";

/**
 * `/games/:id/overview` — docs/spec/09: "edition/coverage report; source
 * provenance, imported text count, missing actors, unresolved errors, version
 * history". This milestone renders what the catalog API can actually answer: the
 * editions, every revision of each with its lifecycle state, the import that
 * fills a draft, the gate that refuses a publication, and the export that carries
 * it away. The coverage report proper (regions, OCR fixtures, voice approval)
 * arrives with those subsystems.
 *
 * The lifecycle rules are the server's, not the page's: `next_states` on each
 * revision is what the transition table allows (ADR-0015), publication is a
 * separate call from a status write (docs/spec/08), and every write carries the
 * revision&apos;s ETag. A button that would be refused is not shown; a write that
 * is refused is explained with the server&apos;s words.
 */
export function GameOverviewPage() {
  const { id = "" } = useParams();
  const queryClient = useQueryClient();
  const { atLeast } = useAuth();
  const [creatingEdition, setCreatingEdition] = useState(false);

  const game = useQuery({
    queryKey: ["game", id],
    queryFn: ({ signal }) => catalogApi.getGame(id, signal),
  });

  const editions = useQuery({
    queryKey: ["editions", id],
    queryFn: ({ signal }) => catalogApi.listEditions(id, signal),
    enabled: !game.isError,
  });

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["game", id] });
    void queryClient.invalidateQueries({ queryKey: ["editions", id] });
    void queryClient.invalidateQueries({ queryKey: ["profiles"] });
  };

  return (
    <main>
      <header className="page-header">
        <h1 data-testid="game-title">{game.data?.title ?? id}</h1>
        {atLeast("curator") ? (
          <button type="button" onClick={() => setCreatingEdition(true)} data-testid="new-edition">
            New edition
          </button>
        ) : null}
      </header>
      <p className="muted">
        <Link to="/library">Library</Link>
        {game.data ? (
          <>
            {" · "}
            <code>{game.data.slug}</code>
            {game.data.platform ? ` · ${game.data.platform}` : ""}
          </>
        ) : null}
      </p>

      {game.isPending || editions.isPending ? <Loading label="Loading the title…" /> : null}
      {game.isError ? (
        <ErrorState
          error={game.error}
          action="Cannot load this game"
          onRetry={() => void game.refetch()}
        />
      ) : null}
      {game.isError && editions.isError ? (
        <ErrorState error={editions.error} action="Cannot load its editions" />
      ) : null}

      {editions.data && editions.data.editions.length === 0 ? (
        <EmptyState title="No editions yet">
          <p>
            An edition binds a title to a region, language and disc revision. Dialogue revisions and
            imports hang off one.
          </p>
        </EmptyState>
      ) : null}

      {editions.data && editions.data.editions.length > 0 ? (
        <TabsPrimitive.Root className="tabs" defaultValue={editions.data.editions[0].id}>
          <TabsPrimitive.List className="tabs-list" aria-label="Editions">
            {editions.data.editions.map((edition) => (
              <TabsPrimitive.Trigger
                key={edition.id}
                value={edition.id}
                className="tab"
                data-testid={`edition-tab-${edition.id}`}
              >
                {editionLabel(edition)}
              </TabsPrimitive.Trigger>
            ))}
          </TabsPrimitive.List>
          {editions.data.editions.map((edition) => (
            <TabsPrimitive.Content key={edition.id} value={edition.id} className="tab-panel">
              <EditionPanel
                gameId={id}
                edition={edition}
                canWrite={atLeast("curator")}
                onChanged={refresh}
              />
            </TabsPrimitive.Content>
          ))}
        </TabsPrimitive.Root>
      ) : null}

      {creatingEdition ? (
        <NewEditionDialog
          gameId={id}
          onClose={() => setCreatingEdition(false)}
          onCreated={() => {
            setCreatingEdition(false);
            refresh();
          }}
        />
      ) : null}
    </main>
  );
}

function editionLabel(edition: Edition): string {
  const parts = [
    edition.region || "region-unset",
    edition.language || "lang-unset",
    edition.revision || "rev-unset",
  ];
  return parts.join(" / ");
}

/** One edition: its revision history (ADR-0015) and the actions each state allows. */
function EditionPanel({
  gameId,
  edition,
  canWrite,
  onChanged,
}: {
  gameId: string;
  edition: Edition;
  canWrite: boolean;
  onChanged: () => void;
}) {
  const profiles = useQuery({
    queryKey: ["profiles", edition.id],
    queryFn: ({ signal }) => catalogApi.listProfiles(edition.id, signal),
  });
  const [error, setError] = useState<unknown>(null);

  const createDraft = useMutation({
    mutationFn: () => catalogApi.createDraft(edition.id),
    onSuccess: onChanged,
    onError: setError,
  });

  if (profiles.isPending) return <Loading label="Loading revisions…" />;
  if (profiles.isError) return <ErrorState error={profiles.error} action="Cannot load revisions" />;

  return (
    <section className="panel" data-testid={`edition-panel-${edition.id}`}>
      <dl className="facts">
        <div>
          <dt>Region / language / revision</dt>
          <dd>
            {edition.region || "—"} / {edition.language || "—"} / {edition.revision || "—"}
          </dd>
        </div>
        <div>
          <dt>Source identity</dt>
          <dd>
            <code>{edition.source_identity || "not observed"}</code>
          </dd>
        </div>
        {edition.locale ? (
          <div>
            <dt>Locale</dt>
            <dd>{edition.locale}</dd>
          </div>
        ) : null}
      </dl>

      {error ? <ErrorState error={error} action="Cannot change this edition" /> : null}

      {canWrite ? (
        <p>
          <button
            type="button"
            onClick={() => createDraft.mutate()}
            disabled={createDraft.isPending}
            data-testid="new-draft"
          >
            {createDraft.isPending ? "Creating…" : "New draft"}
          </button>{" "}
          <span className="muted">A new draft lineage for this edition.</span>
        </p>
      ) : null}
      {createDraft.isError ? (
        <ErrorState error={createDraft.error} action="Cannot create the draft" />
      ) : null}

      {profiles.data.profiles.length === 0 ? (
        <EmptyState title="No revisions yet">
          <p>Revisions hold the dialogue. Start a draft, then import a script into it.</p>
        </EmptyState>
      ) : (
        <ul className="revisions" data-testid="revision-list">
          {[...profiles.data.profiles].reverse().map((profile) => (
            <RevisionRow
              key={profile.id}
              gameId={gameId}
              profile={profile}
              canWrite={canWrite}
              onChanged={onChanged}
            />
          ))}
        </ul>
      )}
    </section>
  );
}

/**
 * A revision row. `next_states` drives the buttons: the transition table lives in
 * internal/games (ADR-0015) and the DTO repeats its answer, so the UI cannot
 * offer a move the store would refuse — and `to: published` is never a status
 * write, which is why Publish is its own action rather than an entry here.
 */
function RevisionRow({
  gameId,
  profile,
  canWrite,
  onChanged,
}: {
  gameId: string;
  profile: Profile;
  canWrite: boolean;
  onChanged: () => void;
}) {
  const [error, setError] = useState<unknown>(null);
  const [refused, setRefused] = useState<string[] | null>(null);
  const [importJob, setImportJob] = useState<string | null>(null);

  const move = useMutation({
    mutationFn: async (to: string) => {
      await refreshProfileEtag(profile.id);
      return catalogApi.setStatus(profile.id, to as Profile["status"]);
    },
    onSuccess: onChanged,
    onError: setError,
  });

  const publish = useMutation({
    mutationFn: async () => {
      await refreshProfileEtag(profile.id);
      return catalogApi.publish(profile.id);
    },
    onSuccess: () => {
      setRefused(null);
      onChanged();
    },
    onError: (caught) => {
      if (caught instanceof ApiError && caught.problems.length > 0) {
        setRefused(caught.problems);
        return;
      }
      setError(caught);
    },
  });

  const derive = useMutation({
    mutationFn: () => catalogApi.createRevision(profile.id),
    onSuccess: onChanged,
    onError: setError,
  });

  const immutable = profile.status === "published" || profile.status === "archived";

  return (
    <li className="revision" data-testid={`revision-${profile.revision_num}`}>
      <div className="revision-head">
        <strong>Revision {profile.revision_num}</strong>
        <StatusBadge status={profile.status} />
        {profile.parent_revision ? (
          <span className="badge badge-plain">from r{profile.parent_revision}</span>
        ) : null}
        {profile.published_at ? (
          <span className="muted">published {profile.published_at}</span>
        ) : null}
      </div>

      <p className="revision-links">
        <Link
          to={`/games/${gameId}/dialogue?profile=${encodeURIComponent(profile.id)}`}
          data-testid={`dialogue-${profile.revision_num}`}
        >
          Dialogue
        </Link>
        <a
          href={portabilityApi.exportUrl(profile.id)}
          data-testid={`export-${profile.revision_num}`}
        >
          Export
        </a>
        <span className="muted">
          digest{" "}
          <code>
            {profile.manifest_digest ? profile.manifest_digest.slice(0, 12) : "not computed"}
          </code>
        </span>
      </p>

      {canWrite ? (
        <div className="actions">
          {profile.next_states
            .filter((state) => state !== "published")
            .map((state) => (
              <button
                key={state}
                type="button"
                onClick={() => move.mutate(state)}
                disabled={move.isPending}
                data-testid={`status-${state}`}
              >
                Move to {state}
              </button>
            ))}
          {profile.next_states.includes("published") ? (
            <button
              type="button"
              onClick={() => publish.mutate()}
              disabled={publish.isPending}
              data-testid="publish"
            >
              {publish.isPending ? "Publishing…" : "Publish"}
            </button>
          ) : null}
          {immutable ? (
            <button
              type="button"
              onClick={() => derive.mutate()}
              disabled={derive.isPending}
              data-testid="new-revision"
            >
              {derive.isPending ? "Deriving…" : "New revision from this"}
            </button>
          ) : null}
        </div>
      ) : null}

      {!immutable ? (
        <ImportForm
          profileId={profile.id}
          onAccepted={setImportJob}
          onError={setError}
          jobId={importJob}
          onFinished={() => onChanged()}
        />
      ) : null}

      <ProblemList problems={refused ?? []} />
      {move.isError ? <ErrorState error={move.error} action="Cannot move this revision" /> : null}
      {publish.isError ? (
        <ErrorState error={publish.error} action="Cannot publish this revision" />
      ) : null}
      {derive.isError ? (
        <ErrorState error={derive.error} action="Cannot derive a revision" />
      ) : null}
      {error && !move.isError && !publish.isError && !derive.isError ? (
        <ErrorState error={error} action="Cannot change this revision" />
      ) : null}
    </li>
  );
}

export function StatusBadge({ status }: { status: Profile["status"] }) {
  return (
    <span className={`badge badge-${status}`} data-testid={`badge-${status}`}>
      {status}
    </span>
  );
}

/**
 * The upload form for one draft. The API answers 202 with a job (docs/spec/08
 * "staged source/profiles"), so the row's life after the submit belongs to the
 * queue: this polls GET /api/v1/jobs/{id} until a terminal state and then shows
 * the importer&apos;s own report, including the rows it skipped.
 */
function ImportForm({
  profileId,
  onAccepted,
  onError,
  jobId,
  onFinished,
}: {
  profileId: string;
  onAccepted: (jobId: string) => void;
  onError: (error: unknown) => void;
  jobId: string | null;
  onFinished: () => void;
}) {
  const [rights, setRights] = useState<RightsStatus>("user_supplied");
  const [fileName, setFileName] = useState("");
  // docs/spec/04: a file whose headers or keys do not match the adapter's
  // defaults is mapped by configuration, per upload. Blank = the defaults.
  const [columns, setColumns] = useState<ColumnMapping>({});
  const setColumn = (name: keyof ColumnMapping) => (event: ChangeEvent<HTMLInputElement>) =>
    setColumns((prev) => ({ ...prev, [name]: event.target.value }));
  const upload = useMutation({
    mutationFn: (file: File) => portabilityApi.importFile(profileId, file, { rights, columns }),
    onSuccess: (accepted) => onAccepted(accepted.job_id),
    onError,
  });

  const job = useQuery({
    queryKey: ["job", jobId],
    queryFn: ({ signal }) => jobsApi.get(jobId as string, signal),
    enabled: jobId !== null,
    refetchInterval: (state) => (isTerminal(state.state.data?.state) ? false : 500),
  });

  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    const input = event.currentTarget.querySelector<HTMLInputElement>("input[type=file]");
    const file = input?.files?.[0];
    if (!file) {
      onError(new Error("choose a script file first"));
      return;
    }
    upload.mutate(file);
  };

  const report = jobsApi.importReport(job.data);

  return (
    <div className="import" data-testid={`import-${profileId}`}>
      <form onSubmit={onSubmit} className="import-form">
        <label>
          Script file
          <input
            type="file"
            accept=".csv,.tsv,.json,.jsonl,.txt,text/plain,text/csv,application/json"
            required
            onChange={(event) => setFileName(event.target.files?.[0]?.name ?? "")}
            data-testid="import-file"
          />
        </label>
        <label>
          Rights position
          <select
            value={rights}
            onChange={(event) => setRights(event.target.value as RightsStatus)}
            data-testid="import-rights"
          >
            {/* docs/spec/14: `unknown` is never accepted on an upload, so it is
                not offered — the server refuses it with 422 for the same reason. */}
            <option value="user_supplied">user_supplied</option>
            <option value="licensed">licensed</option>
          </select>
        </label>
        <details className="import-mapping" data-testid="import-mapping">
          <summary>Column mapping (files whose headers differ)</summary>
          <p className="muted">
            Name the source column or key each field comes from; blank keeps the adapter default
            (Text, ID, Speaker, Kind, Scene, Page).
          </p>
          <div className="import-mapping-grid">
            {(
              [
                ["text", "Text"],
                ["id", "ID"],
                ["speaker", "Speaker"],
                ["kind", "Kind"],
                ["scene", "Scene"],
                ["page", "Page"],
              ] as [keyof ColumnMapping, string][]
            ).map(([name, label]) => (
              <label key={name}>
                {label}
                <input
                  type="text"
                  value={columns[name] ?? ""}
                  onChange={setColumn(name)}
                  data-testid={`import-col-${name}`}
                />
              </label>
            ))}
          </div>
        </details>
        <button type="submit" disabled={upload.isPending} data-testid="import-submit">
          {upload.isPending ? "Uploading…" : "Import into this revision"}
        </button>
      </form>
      {upload.isError ? (
        <ErrorState error={upload.error} action="The upload was not accepted" />
      ) : null}
      {fileName && !upload.isPending && jobId === null ? <p className="muted">{fileName}</p> : null}

      {job.data ? <JobProgress job={job.data} onSettled={onFinished} /> : null}
      {job.isError ? <ErrorState error={job.error} action="Cannot follow the import job" /> : null}
      {report ? (
        <dl className="facts import-report" data-testid="import-report">
          <div>
            <dt>Adapter</dt>
            <dd>
              {report.adapter} · {report.media_type}
            </dd>
          </div>
          <div>
            <dt>Rows</dt>
            <dd>{report.stats.rows}</dd>
          </div>
          <div>
            <dt>Messages inserted</dt>
            <dd data-testid="import-inserted">{report.messages_inserted}</dd>
          </div>
          <div>
            <dt>Skipped</dt>
            <dd>{report.stats.skipped}</dd>
          </div>
          {report.warnings.length > 0 ? (
            <div>
              <dt>Warnings</dt>
              <dd>
                {report.warnings.map((warning) => (
                  <span key={`${warning.line}-${warning.msg}`}>
                    line {warning.line}: {warning.msg}{" "}
                  </span>
                ))}
                {report.warnings_truncated ? <em>(truncated)</em> : null}
              </dd>
            </div>
          ) : null}
        </dl>
      ) : null}
    </div>
  );
}

function isTerminal(state: Job["state"] | string | undefined): boolean {
  return state === "succeeded" || state === "failed" || state === "cancelled";
}

export function JobProgress({ job, onSettled }: { job: JobDetail; onSettled?: () => void }) {
  const settled = isTerminal(job.state);
  // A succeeded import moved the revision's contents and its row_version, so the
  // parent's reads must not keep answering from the pre-import cache. In an
  // effect, not during render: this component re-renders on every poll.
  useEffect(() => {
    if (settled && onSettled) onSettled();
  }, [settled, job.state, onSettled]);

  return (
    <p
      className={`job job-${job.state}`}
      data-testid="job-progress"
      aria-live={settled ? "off" : "polite"}
      role={job.state === "failed" ? "alert" : undefined}
    >
      <span className="badge">{job.state}</span> {job.kind}
      {job.state === "failed" ? ` — ${describeFailure(job)}` : ""}
      {job.state === "requires_input"
        ? " — the job needs a human answer before it can continue"
        : ""}
    </p>
  );
}

function describeFailure(job: JobDetail): string {
  const result = job.result as { error?: string; message?: string } | undefined;
  return result?.error ?? result?.message ?? "see the job resource for the finding";
}

/**
 * The edition dialog. (game, region, language, revision) is unique in the store,
 * so re-declaring a disc is a 409 to resolve rather than a second row — the form
 * says so instead of letting the user guess which of the four fields collided.
 */
function NewEditionDialog({
  gameId,
  onClose,
  onCreated,
}: {
  gameId: string;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [region, setRegion] = useState("");
  const [language, setLanguage] = useState("");
  const [revision, setRevision] = useState("");
  const [locale, setLocale] = useState("");
  const [identity, setIdentity] = useState("");

  const create = useMutation({
    mutationFn: () =>
      catalogApi.createEdition(gameId, {
        region,
        language,
        revision,
        locale: locale || undefined,
        source_identity: identity || undefined,
      }),
    onSuccess: onCreated,
  });

  return (
    <Modal open onOpenChange={(open) => !open && onClose()} title="New edition">
      <p className="muted">
        Region, language and disc revision together identify the thing you actually own. Leave a
        field empty to say it was never observed.
      </p>
      {create.isError ? (
        <ErrorState error={create.error} action="Cannot create the edition" />
      ) : null}
      <form
        onSubmit={(event) => {
          event.preventDefault();
          create.mutate();
        }}
        className="stack"
        data-testid="new-edition-form"
      >
        <label>
          Region
          <input
            value={region}
            onChange={(event) => setRegion(event.target.value)}
            maxLength={32}
            data-testid="edition-region"
          />
        </label>
        <label>
          Language
          <input
            value={language}
            onChange={(event) => setLanguage(event.target.value)}
            maxLength={32}
            data-testid="edition-language"
          />
        </label>
        <label>
          Disc revision
          <input
            value={revision}
            onChange={(event) => setRevision(event.target.value)}
            maxLength={64}
            data-testid="edition-revision"
          />
        </label>
        <label>
          Locale
          <input
            value={locale}
            onChange={(event) => setLocale(event.target.value)}
            maxLength={32}
            placeholder="en-US"
            data-testid="edition-locale"
          />
        </label>
        <label>
          Source identity (SHA-256 of the image)
          <input
            value={identity}
            onChange={(event) => setIdentity(event.target.value)}
            data-testid="edition-identity"
          />
        </label>
        <button type="submit" disabled={create.isPending} data-testid="edition-submit">
          {create.isPending ? "Creating…" : "Create edition"}
        </button>
      </form>
    </Modal>
  );
}
