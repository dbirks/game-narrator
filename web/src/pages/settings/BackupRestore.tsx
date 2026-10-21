import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { apiStatus } from "../../api/client";
import {
  downloadBackup,
  fetchRestoreStatus,
  uploadRestore,
  type RestoreScheduled,
  type RestoreStatus,
} from "../../api/settings";
import { Card } from "../../components/kit";

/**
 * The backup/restore pair (ADR-0090, ADR-0142 restyle): the backup download
 * (a point-in-time SQLite snapshot) and the restore — the upload validates
 * server-side (format, schema marker, integrity, forward-migrations only),
 * takes an automatic pre-restore copy, and the appliance restarts to apply
 * it. The file's hash and size are shown before the confirm button, so the
 * operator reviews what the restart will install. The post-restart status
 * is this card's banner: none / pending / applied / failed, with the
 * recorded evidence — a restore is never silent.
 */
export function BackupRestoreSection() {
  const statusQuery = useQuery({
    queryKey: ["restore-status"],
    queryFn: ({ signal }) => fetchRestoreStatus(signal),
    retry: false,
  });
  const [file, setFile] = useState<File | null>(null);
  const [fileSha, setFileSha] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [backupError, setBackupError] = useState<string | null>(null);
  const [restoreError, setRestoreError] = useState<string | null>(null);
  const [scheduled, setScheduled] = useState<RestoreScheduled | null>(null);

  // The operator reviews the file's own hash and size before the confirm
  // button — the 202 then repeats the hash the server staged, so a file that
  // changed in flight is visible as a mismatch.
  useEffect(() => {
    setScheduled(null);
    if (!file) {
      setFileSha(null);
      return;
    }
    let live = true;
    void file.arrayBuffer().then((bytes) =>
      crypto.subtle.digest("SHA-256", bytes).then((digest) => {
        if (!live) return;
        setFileSha(
          Array.from(new Uint8Array(digest))
            .map((b) => b.toString(16).padStart(2, "0"))
            .join(""),
        );
      }),
    );
    return () => {
      live = false;
    };
  }, [file]);

  const onBackup = async () => {
    setBusy(true);
    setBackupError(null);
    try {
      await downloadBackup();
    } catch (error) {
      setBackupError(
        error instanceof Error ? error.message : `the backup answered ${apiStatus(error) ?? "?"}`,
      );
    } finally {
      setBusy(false);
    }
  };

  const onRestore = async () => {
    if (!file || busy) return;
    setBusy(true);
    setRestoreError(null);
    try {
      const answer = await uploadRestore(file);
      setScheduled(answer);
      void statusQuery.refetch();
    } catch (error) {
      setRestoreError(
        error instanceof Error ? error.message : `the restore answered ${apiStatus(error) ?? "?"}`,
      );
    } finally {
      setBusy(false);
    }
  };

  const status = statusQuery.data;

  return (
    <Card title="Backup and restore" testid="settings-card-backup">
      <RestoreBanner status={status} refetching={statusQuery.isFetching} />
      <p className="muted">
        The backup is a point-in-time SQLite snapshot of the appliance's database. A restore
        replaces the appliance's catalogue with a backup file: the server validates it first (SQLite
        format, the Game Narrator schema marker, integrity, and this build's migrations — a backup
        newer than the running binary is refused), copies the live database aside as the rollback
        artifact, and the appliance restarts to apply it.
      </p>
      <div className="row" data-testid="backup-restore-controls">
        <button
          type="button"
          onClick={() => void onBackup()}
          disabled={busy}
          data-testid="backup-download"
        >
          {busy ? "Preparing the download…" : "Download database backup"}
        </button>
        <label htmlFor="restore-file" className="muted">
          Restore from a backup
        </label>
        <input
          id="restore-file"
          type="file"
          accept=".sqlite,.sqlite3,.db"
          data-testid="restore-file"
          onChange={(event) => setFile(event.target.files?.[0] ?? null)}
        />
        <button
          type="button"
          onClick={() => void onRestore()}
          disabled={!file || !fileSha || busy}
          data-testid="restore-upload"
        >
          {busy ? "Scheduling the restore…" : "Schedule restore and restart"}
        </button>
      </div>
      {file && fileSha && !scheduled && (
        <p className="muted" role="status" data-testid="restore-review">
          Selected <code>{file.name}</code> — {file.size} bytes, SHA-256 {fileSha.slice(0, 16)}…
          Scheduling takes the automatic pre-restore copy of the live database and restarts the
          appliance; the restore is applied by the restarted process.
        </p>
      )}
      {backupError && (
        <p role="alert" className="error" data-testid="backup-error">
          {backupError}
        </p>
      )}
      {restoreError && (
        <p role="alert" className="error" data-testid="restore-error">
          {restoreError}
        </p>
      )}
      {scheduled && (
        <p role="status" className="note" data-testid="restore-scheduled">
          Restore scheduled — staged SHA-256 {scheduled.staged_sha256.slice(0, 16)}… (
          {scheduled.staged_bytes} bytes, schema {scheduled.backup_schema_version}), pre-restore
          copy {scheduled.pre_restore_backup}. {scheduled.note}
        </p>
      )}
    </Card>
  );
}

// The post-restart banner: the recorded outcome of the last restore. It is
// read from the data directory, so the process that just applied the swap is
// the one that reports it — and a plan the restart never applied is reported
// as pending, with its evidence, not as success.
function RestoreBanner({
  status,
  refetching,
}: {
  status: RestoreStatus | undefined;
  refetching: boolean;
}) {
  if (!status || status.status === "none") return null;
  const plan = status.plan;
  return (
    <div
      data-testid={`restore-banner-${status.status}`}
      role={status.status === "failed" ? "alert" : "status"}
    >
      <strong>
        {status.status === "pending" && "A restore is waiting for the restart"}
        {status.status === "applied" && `Restore applied ${status.at ?? ""}`.trim()}
        {status.status === "failed" && "The last restore failed"}
      </strong>{" "}
      {plan && plan.staged_sha256 && (
        <span className="muted">
          (backup SHA-256 {plan.staged_sha256.slice(0, 16)}…,
          {plan.backup_schema_version !== undefined ? ` schema ${plan.backup_schema_version}` : ""}
          {plan.pre_restore_backup ? `, pre-restore copy ${plan.pre_restore_backup}` : ""})
        </span>
      )}
      {status.status === "applied" &&
        status.schema_version_after != null &&
        status.schema_version_after > 0 && (
          <span className="muted">
            {" "}
            — schema {status.schema_version_before || "?"} → {status.schema_version_after}
            {status.migrations_applied?.length
              ? `, migrations: ${status.migrations_applied.join(", ")}`
              : ""}
          </span>
        )}
      {status.status === "failed" && status.error && <p className="error">{status.error}</p>}
      {status.status === "failed" && status.recovered_from_pre_restore && (
        <p className="muted">
          The appliance recovered from the pre-restore copy and is running on the pre-restore state;
          the rollback artifact is {plan?.pre_restore_backup ?? "the pre-restore copy"}.
        </p>
      )}
      {refetching && status.status === "pending" && <span className="muted"> (checking…)</span>}
    </div>
  );
}
