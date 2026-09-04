import {
  createContext,
  useContext,
  useEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type ReactNode,
} from "react";
import { Link } from "react-router";
import { RequestError } from "./api/client";
import type { Run, System, Timestamp } from "./api/types";

export const SystemContext = createContext<System | null>(null);
export const useSystem = () => useContext(SystemContext)!;

const labels: Record<string, string> = {
  EMPTY: "Not collected yet",
  STABLE: "Ready",
  RECONCILING: "Updating local copy",
  RECOVERY_REQUIRED: "Needs attention",
  AVAILABLE_UNMOUNTED: "Disk ready",
  ABSENT: "Not connected",
  MOUNTED: "Mounted",
  MOUNTED_VERIFIED: "Mounted",
  UNKNOWN: "Unknown",
  SUSPECT: "Needs verification",
  HEALTHY: "Healthy",
  UNINITIALIZED: "Not prepared",
  SUCCEEDED: "Completed",
  FAILED: "Failed",
  INTERRUPTED: "Interrupted",
  CANCELLED: "Cancelled",
  RUNNING: "In progress",
  QUEUED: "Waiting",
  REQUESTED: "Requested",
  CONNECTION_TEST: "Connection test",
  COLLECTION: "Data collection",
  BACKUP_LATEST: "Backup",
  REPOSITORY_INIT: "Prepare backup storage",
  REPOSITORY_INITIALIZE: "Prepare backup storage",
  REPOSITORY_ADOPT: "Connect existing storage",
  REPOSITORY_RECOVERY: "Storage verification",
  REPOSITORY_RECOVER: "Storage verification",
  DEMO: "Demo (not a backup)",
  DISABLED: "Disabled",
  UNREGISTERED: "Not registered",
  UNSUPPORTED: "Not supported",
  MOUNTED_EXTERNALLY: "Mounted outside ROTUP",
  AMBIGUOUS: "Identity conflict",
  DISCONNECTED: "Not connected",
  DETECTED: "Detected",
  READY: "Mounted and verified",
  SAFE_TO_REMOVE: "Safe to remove",
  ERROR: "Needs attention",
  UPDATING: "Updating local copy",
  PENDING: "Waiting",
  PARTIAL: "Incomplete",
  SUCCEEDED_WITH_WARNING: "Completed with warnings",
  DISK_SETUP: "Prepare backup storage",
  VERIFY: "Storage verification",
};
export const label = (value: string) =>
  labels[value] ??
  value
    .replaceAll("_", " ")
    .toLowerCase()
    .replace(/^./, (c) => c.toUpperCase());
export const date = (value: Timestamp) => {
  if (!value) return "Not recorded";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "Not recorded";
  return parsed.toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  });
};
// Disk sizes come from the platform in bytes and span megabytes to terabytes.
// Reporting every one of them in gigabytes turned a 374 MB volume into
// "0.4 GB", which an operator cannot use to tell two disks apart.
const sizeUnits = [
  "byte",
  "kilobyte",
  "megabyte",
  "gigabyte",
  "terabyte",
  "petabyte",
] as const;
export function bytes(value: number) {
  if (!Number.isFinite(value) || value <= 0) return "Not available";
  let scaled = value;
  let unit = 0;
  while (scaled >= 1000 && unit < sizeUnits.length - 1) {
    scaled /= 1000;
    unit += 1;
  }
  return new Intl.NumberFormat(undefined, {
    style: "unit",
    unit: sizeUnits[unit],
    unitDisplay: "short",
    maximumFractionDigits: unit === 0 || scaled >= 100 ? 0 : 1,
  }).format(scaled);
}
// A backup can run for hours. Seconds with one decimal is unreadable past a
// minute, so switch units instead of printing "7203.4 s".
export function duration(ms: number) {
  if (!Number.isFinite(ms) || ms < 0) return "Not recorded";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) return `${(ms / 1000).toFixed(1)} s`;
  if (seconds < 3600)
    return `${Math.floor(seconds / 60)} min ${seconds % 60} s`;
  return `${Math.floor(seconds / 3600)} h ${Math.floor((seconds % 3600) / 60)} min`;
}

export function StatusBadge({ value }: { value: string }) {
  const good = [
    "STABLE",
    "AVAILABLE_UNMOUNTED",
    "SUCCEEDED",
    "SUCCEEDED_WITH_WARNING",
    "HEALTHY",
    "READY",
    "SAFE_TO_REMOVE",
    "running",
    "ok",
  ].includes(value);
  const bad = [
    "FAILED",
    "RECOVERY_REQUIRED",
    "SUSPECT",
    "AMBIGUOUS",
    "INTERRUPTED",
    "UNAVAILABLE",
    "ERROR",
    "PARTIAL",
  ].includes(value);
  return (
    <span className={`badge ${good ? "good" : bad ? "bad" : "neutral"}`}>
      <span aria-hidden="true" className="dot" />
      {label(value)}
    </span>
  );
}
export function PageHeader({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children?: ReactNode;
}) {
  useEffect(() => {
    document.title = `${title} · ROTUP`;
  }, [title]);
  return (
    <header className="page-header">
      <div>
        <p className="eyebrow">Backup operations</p>
        <h1>{title}</h1>
        {description && <p className="muted">{description}</p>}
      </div>
      <div className="actions">{children}</div>
    </header>
  );
}
export function Alert({
  children,
  tone = "warning",
}: {
  children: ReactNode;
  tone?: "warning" | "danger" | "info";
}) {
  return (
    <div
      className={`alert ${tone}`}
      role={tone === "danger" ? "alert" : "status"}
    >
      {children}
    </div>
  );
}
export function TechnicalDetails({
  children,
  title = "Technical details",
}: {
  children: ReactNode;
  title?: string;
}) {
  const dev = useContext(SystemContext)?.devMode ?? false;
  return (
    <details className="technical" open={dev}>
      <summary>{title}</summary>
      <div>{children}</div>
    </details>
  );
}
export function ErrorAlert({ error }: { error: unknown }) {
  const detail = error instanceof RequestError ? error.detail : null;
  return (
    <Alert tone="danger">
      <strong>
        {detail?.userMessage ?? "Something went wrong while loading this page."}
      </strong>
      <TechnicalDetails>
        <p>{detail?.message ?? "An unexpected client error occurred."}</p>
        {detail && (
          <code>
            {detail.class} / {detail.code}
          </code>
        )}
      </TechnicalDetails>
    </Alert>
  );
}
export function LoadingState() {
  return (
    <p className="loading" role="status">
      Loading ROTUP data…
    </p>
  );
}
export function EmptyState({
  title,
  children,
}: {
  title: string;
  children?: ReactNode;
}) {
  return (
    <div className="empty">
      <h2>{title}</h2>
      {children}
    </div>
  );
}
export function ActionButton({
  busy,
  children,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { busy?: boolean }) {
  return (
    <button {...props} disabled={props.disabled || busy} aria-busy={busy}>
      {busy ? "Working…" : children}
    </button>
  );
}
export function ConfirmDialog({
  title,
  children,
  onConfirm,
  disabled,
  actionLabel,
}: {
  title: string;
  children: ReactNode;
  onConfirm: () => Promise<void>;
  disabled?: boolean;
  actionLabel: string;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  return (
    <>
      <button
        disabled={disabled}
        onClick={() => {
          setError(undefined);
          ref.current?.showModal();
        }}
      >
        {actionLabel}
      </button>
      <dialog
        ref={ref}
        aria-label={title}
        onCancel={(event) => {
          if (busy) event.preventDefault();
        }}
      >
        <h2>{title}</h2>
        {children}
        {!!error && <ErrorAlert error={error} />}
        <div className="actions">
          <button
            className="secondary"
            disabled={busy}
            onClick={() => ref.current?.close()}
          >
            Cancel
          </button>
          <ActionButton
            busy={busy}
            onClick={async () => {
              setBusy(true);
              setError(undefined);
              try {
                await onConfirm();
                ref.current?.close();
              } catch (error) {
                setError(error);
              } finally {
                setBusy(false);
              }
            }}
          >
            {actionLabel}
          </ActionButton>
        </div>
      </dialog>
    </>
  );
}
export function RunList({
  runs,
  sources = {},
  emptyTitle = "No activity recorded",
  emptyBody = "Your connection tests, collections and backups will appear here.",
}: {
  runs: Run[];
  sources?: Record<string, string>;
  emptyTitle?: string;
  emptyBody?: string;
}) {
  if (!runs.length)
    return (
      <EmptyState title={emptyTitle}>
        <p>{emptyBody}</p>
      </EmptyState>
    );
  return (
    <div className="table-scroll">
      <table>
        <thead>
          <tr>
            <th>Operation</th>
            <th>Result</th>
            <th>Source / storage</th>
            <th>Started / requested</th>
            <th>Duration</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((run) => (
            <tr key={run.id}>
              <td>
                <Link to={`/runs/${run.id}`}>{label(run.kind)}</Link>
                {run.summary && (
                  <small className="run-summary">{run.summary}</small>
                )}
              </td>
              <td>
                <StatusBadge value={run.status} />
              </td>
              <td>
                {run.sourceId ? (
                  <Link to={`/sources/${run.sourceId}`}>
                    {sources[run.sourceId] || "View source"}
                  </Link>
                ) : run.repositoryId ? (
                  <Link to={`/repositories/${run.repositoryId}`}>
                    Backup storage
                  </Link>
                ) : (
                  "—"
                )}
              </td>
              <td>
                {date(run.startedAt ?? run.requestedAt)}
                {run.finishedAt && (
                  <small>Finished {date(run.finishedAt)}</small>
                )}
              </td>
              <td>{run.terminal ? duration(run.durationMs) : "In progress"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
export function JsonDetails({ value }: { value: unknown }) {
  return <pre>{JSON.stringify(value, null, 2)}</pre>;
}
