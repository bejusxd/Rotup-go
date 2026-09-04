import { Link } from "react-router";
import { useQuery } from "../api/hooks";
import type { Dashboard } from "../api/types";
import {
  Alert,
  date,
  ErrorAlert,
  JsonDetails,
  label,
  LoadingState,
  PageHeader,
  RunList,
  StatusBadge,
  TechnicalDetails,
  useSystem,
} from "../components";

export function DashboardPage() {
  const system = useSystem();
  const query = useQuery<Dashboard>("/dashboard", 15000);
  if (query.loading) return <LoadingState />;
  if (query.error)
    return (
      <>
        <PageHeader title="Overview" />
        <ErrorAlert error={query.error} />
        <button onClick={query.reload}>Refresh</button>
      </>
    );
  const data = query.data!;
  const registered = data.storage.disks.filter((d) => d.registered);
  const latestFailure = data.recentRuns.find((r) =>
    ["FAILED", "INTERRUPTED", "PARTIAL"].includes(r.status),
  );
  const needsAttention =
    data.applicationStatus !== "running" ||
    !system.transferEngine.available ||
    !system.backupEngine.available ||
    registered.some((d) => d.status === "ERROR" || d.ambiguous) ||
    data.repositories.some((r) => r.health !== "HEALTHY") ||
    data.sources.some((s) => s.status === "RECOVERY_REQUIRED") ||
    !!data.storage.enumerationError ||
    !data.storage.platformAvailable;
  // Each unfinished step points at the page that performs it and names the
  // action, so "what do I do next" never means "work out which subsystem owns
  // this". The evidence itself is still whatever the backend persisted.
  const source = data.sources[0];
  const storage =
    data.repositories.find((r) => r.established) ?? data.repositories[0];
  const sourceHref = source ? `/sources/${source.id}` : "/sources/new";
  const storageDiskConnected = registered.some(
    (d) => d.id === storage?.diskId && d.observed,
  );
  const steps = [
    {
      text: "Source configured",
      done: data.sources.length > 0,
      href: sourceHref,
      action: "Add a source",
    },
    {
      text: "Connection test succeeded in recent history",
      done: data.recentRuns.some(
        (r) => r.kind === "CONNECTION_TEST" && r.status === "SUCCEEDED",
      ),
      href: sourceHref,
      action: "Test the connection",
    },
    {
      text: "First collection published",
      done: data.sources.some((s) => s.lastPublishedAt),
      href: sourceHref,
      action: "Collect this source",
    },
    {
      text: "Backup disk registered",
      done: registered.length > 0,
      href: "/disks",
      action: "Register a backup disk",
    },
    {
      text: "Backup storage prepared",
      done: data.repositories.some((r) => r.established),
      href: registered.length
        ? `/repositories/new?disk=${encodeURIComponent(registered[0].id)}`
        : "/disks",
      action: "Prepare backup storage",
    },
    {
      text: "First durable backup completed",
      done: !!data.lastSuccessfulBackupAt,
      // A backup cannot start while its disk is away, so say so instead of
      // sending the operator to a page whose only button is disabled.
      href: storage
        ? storageDiskConnected
          ? `/repositories/${storage.id}`
          : `/disks/${storage.diskId}`
        : "/repositories",
      action:
        storage && !storageDiskConnected ? "Connect the disk" : "Back up now",
    },
  ];
  const next = steps.find((s) => !s.done);
  return (
    <>
      <PageHeader
        title="Overview"
        description="Your sources, local copies and backup disks — in one place."
      >
        <button
          className="secondary"
          disabled={query.refreshing}
          onClick={query.reload}
        >
          {query.refreshing ? "Refreshing…" : "Refresh"}
        </button>
        <Link className="button" to={next?.href ?? "/repositories"}>
          {next ? next.action : "Back up again"}
        </Link>
      </PageHeader>
      <section
        className={`overview-status ${needsAttention ? "attention" : ""}`}
      >
        <div>
          <p className="eyebrow">At a glance</p>
          <h2>
            {needsAttention
              ? "Some items need attention"
              : !data.lastSuccessfulBackupAt
                ? "No backup to a disk has completed yet"
                : "No current problem detected"}
          </h2>
          <p>
            {data.lastSuccessfulBackupAt
              ? "Last completed backup: " +
                date(data.lastSuccessfulBackupAt) +
                ". Review each disk below; this does not guarantee every source is current."
              : "No completed backup to a disk is recorded yet. A collected local copy stays on this machine and is not a backup."}
            {latestFailure && !needsAttention
              ? " The most recent recorded failure is shown below."
              : ""}
          </p>
        </div>
        <StatusBadge value={data.applicationStatus} />
      </section>
      {(!system.transferEngine.available || !system.backupEngine.available) && (
        <Alert tone="danger">
          <strong>A required tool is unavailable.</strong>
          <p>
            {!system.transferEngine.available &&
              "Collecting a source needs the transfer tool. "}
            {!system.backupEngine.available &&
              "Backing up to a disk needs the snapshot tool. "}
            Check the tool configuration before starting an operation.
          </p>
          <TechnicalDetails>
            <p>
              {system.transferEngine.name}:{" "}
              {system.transferEngine.available
                ? `available ${system.transferEngine.version}`
                : system.transferEngine.detail ||
                  "unavailable, no detail recorded"}
            </p>
            <p>
              {system.backupEngine.name}:{" "}
              {system.backupEngine.available
                ? `available ${system.backupEngine.version}`
                : system.backupEngine.detail ||
                  "unavailable, no detail recorded"}
            </p>
          </TechnicalDetails>
        </Alert>
      )}
      {data.storage.enumerationError && (
        <Alert tone="danger">
          Disk discovery failed: {data.storage.enumerationError}
        </Alert>
      )}
      {!data.storage.platformAvailable && (
        <Alert>
          Media support is unavailable. {data.storage.platformReason}
        </Alert>
      )}
      {latestFailure && (
        <Alert tone="warning">
          <strong>
            {label(latestFailure.kind)} did not complete ·{" "}
            {date(latestFailure.finishedAt ?? latestFailure.requestedAt)}
          </strong>
          <p>
            {latestFailure.error?.userMessage ||
              latestFailure.summary ||
              "No result detail was recorded."}
          </p>
          <Link to={`/runs/${latestFailure.id}`}>Review recorded result →</Link>
          <p className="small">
            This is the latest recorded failure, not a live check; a later
            operation may already have resolved it.
          </p>
        </Alert>
      )}
      <section className="metrics" aria-label="System summary">
        <div>
          <span>Last successful backup</span>
          <strong>
            {data.lastSuccessfulBackupAt
              ? date(data.lastSuccessfulBackupAt)
              : "Not yet completed"}
          </strong>
          <Link to="/repositories">View backup storage →</Link>
        </div>
        <div>
          <span>Sources</span>
          <strong>{data.sources.length} configured</strong>
          <Link to="/sources">Manage sources →</Link>
        </div>
        <div>
          <span>Backup disks</span>
          <strong>
            {registered.filter((d) => d.observed).length} of {registered.length}{" "}
            connected
          </strong>
          <Link to="/disks">View disks →</Link>
        </div>
      </section>
      {!data.lastSuccessfulBackupAt && (
        <section className="panel">
          <h2>ROTUP setup</h2>
          <p className="muted">
            Complete these steps to protect your first local collection.
          </p>
          <ol className="checklist">
            {steps.map((step) => (
              <li key={step.text}>
                <span
                  aria-label={step.done ? "Complete" : "Not confirmed"}
                  className={step.done ? "step done" : "step"}
                >
                  {step.done ? "✓" : "○"}
                </span>
                <span>{step.text}</span>
                <Link to={step.href}>{step.done ? "View" : step.action} →</Link>
              </li>
            ))}
          </ol>
          <p className="muted small">
            Connection evidence is limited to the latest {data.recentRunLimit}{" "}
            runs. An unconfirmed step does not prove it never happened.
          </p>
        </section>
      )}
      <div className="columns">
        <section className="panel">
          <h2>Source health</h2>
          {!data.sources.length && (
            <p className="muted">No sources configured.</p>
          )}
          {data.sources.map((s) => (
            <div className="list-row" key={s.id}>
              <Link to={`/sources/${s.id}`}>{s.name}</Link>
              <StatusBadge value={s.enabled ? s.status : "DISABLED"} />
            </div>
          ))}
        </section>
        <section className="panel">
          <h2>Backup storage</h2>
          {!data.repositories.length && (
            <p className="muted">Prepare a registered disk to store backups.</p>
          )}
          {data.repositories.map((r) => (
            <div className="list-row" key={r.id}>
              <div>
                <Link to={`/repositories/${r.id}`}>
                  {r.diskName || "Backup storage"}
                </Link>
                <small>
                  {r.connected ? "Disk connected" : "Disk not connected"}
                </small>
              </div>
              <StatusBadge value={r.health} />
            </div>
          ))}
        </section>
      </div>
      <section className="panel">
        <div className="section-heading">
          <h2>Recent activity</h2>
          <Link to="/runs">View history →</Link>
        </div>
        <RunList
          runs={data.recentRuns.slice(0, 6)}
          sources={Object.fromEntries(data.sources.map((s) => [s.id, s.name]))}
        />
      </section>
      <TechnicalDetails>
        <JsonDetails
          value={{
            applicationStatus: data.applicationStatus,
            databaseStatus: data.databaseStatus,
            transferEngine: system.transferEngine,
            backupEngine: system.backupEngine,
            lastSuccessfulBackupAt: data.lastSuccessfulBackupAt,
          }}
        />
      </TechnicalDetails>
    </>
  );
}
