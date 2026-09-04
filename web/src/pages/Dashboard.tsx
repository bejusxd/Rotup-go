import { Link } from "react-router";
import { useQuery } from "../api/hooks";
import type { Dashboard, SetupState } from "../api/types";
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
  // Onboarding truth is derived by the daemon from persisted state. The
  // dashboard used to recompute it here, which meant two places could disagree
  // about whether a step was done.
  const setup = useQuery<SetupState>("/setup", 30000);
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
  const next = setup.data?.nextStep ?? null;
  const steps = setup.data?.steps ?? [];
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
      {setup.data?.system?.checked && !setup.data.system.ready && (
        <Alert tone="danger">
          <strong>This host is not fully provisioned.</strong>
          <p>
            Run <code>{setup.data.system.command}</code> on the machine, then
            refresh. <Link to="/setup">See what is missing →</Link>
          </p>
        </Alert>
      )}
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
      {setup.data && !setup.data.complete && (
        <section className="panel">
          <div className="section-heading">
            <h2>ROTUP setup</h2>
            <Link to="/setup">Open setup →</Link>
          </div>
          <p className="muted">
            {steps.filter((step) => step.done).length} of {steps.length} steps
            confirmed from what ROTUP recorded.
          </p>
          <ol className="checklist">
            {steps.map((step) => (
              <li key={step.id}>
                <span
                  aria-label={step.done ? "Confirmed" : "Not confirmed"}
                  className={step.done ? "step done" : "step"}
                >
                  {step.done ? "✓" : "○"}
                </span>
                <span>{step.title}</span>
                {step.done ? (
                  <Link to={step.href}>View →</Link>
                ) : step.blocked ? (
                  <Link to="/setup">Needs system setup →</Link>
                ) : (
                  <Link to={step.href}>{step.action} →</Link>
                )}
              </li>
            ))}
          </ol>
          <p className="muted small">
            Connection evidence is limited to the latest{" "}
            {setup.data.recentRunLimit} runs. An unconfirmed step does not prove
            it never happened.
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
