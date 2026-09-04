import { useQuery } from "../api/hooks";
import type { DebugStatus } from "../api/types";
import {
  bytes,
  date,
  EmptyState,
  ErrorAlert,
  JsonDetails,
  LoadingState,
  PageHeader,
  StatusBadge,
  useSystem,
} from "../components";

export function DebugPage() {
  if (!useSystem().devMode)
    return (
      <EmptyState title="Page not found">
        <p>Development diagnostics are disabled.</p>
      </EmptyState>
    );
  return <DebugStatusPage />;
}
function DebugStatusPage() {
  const query = useQuery<DebugStatus>("/debug/status", 10000);
  return (
    <>
      <PageHeader
        title="Diagnostics"
        description="Allowlisted backend state. No passwords, secret contents or credential locators."
      >
        <button disabled={query.refreshing} onClick={query.reload}>
          {query.refreshing ? "Refreshing…" : "Refresh"}
        </button>
      </PageHeader>
      {query.loading ? (
        <LoadingState />
      ) : query.error ? (
        <ErrorAlert error={query.error} />
      ) : (
        <>
          <section className="panel">
            <h2>Environment</h2>
            <dl>
              <dt>Platform</dt>
              <dd>{query.data!.platform}</dd>
              <dt>Transfer engine</dt>
              <dd>
                {query.data!.transferEngine.name}{" "}
                {query.data!.transferEngine.version || "(version unknown)"} ·{" "}
                {query.data!.transferEngine.available
                  ? "available"
                  : `unavailable: ${query.data!.transferEngine.detail || "no detail recorded"}`}
              </dd>
              <dt>Backup engine</dt>
              <dd>
                {query.data!.backupEngine.name}{" "}
                {query.data!.backupEngine.version || "(version unknown)"} ·{" "}
                {query.data!.backupEngine.available
                  ? "available"
                  : `unavailable: ${query.data!.backupEngine.detail || "no detail recorded"}`}
              </dd>
              <dt>Media enumeration</dt>
              <dd>
                {query.data!.status.storage.platformAvailable
                  ? `available, observed ${date(query.data!.status.storage.generatedAt)}`
                  : `unavailable: ${query.data!.status.storage.platformReason || "no reason recorded"}`}
                {query.data!.status.storage.enumerationError &&
                  ` · last error: ${query.data!.status.storage.enumerationError}`}
              </dd>
            </dl>
          </section>
          <section className="panel table-scroll">
            <h2>Resolved media</h2>
            <p className="muted small">
              Device paths and mount points are ephemeral observations of this
              moment. Persistent identity is the filesystem UUID alone.
            </p>
            <table>
              <thead>
                <tr>
                  <th>Filesystem UUID</th>
                  <th>State</th>
                  <th>Device now</th>
                  <th>Mount now</th>
                  <th>Size</th>
                </tr>
              </thead>
              <tbody>
                {query.data!.status.storage.disks.map((d, i) => (
                  <tr key={`${d.id}-${d.filesystemUuid}-${i}`}>
                    <td>
                      <code>{d.filesystemUuid || "none"}</code>
                      {d.registered && <small>registered as {d.name}</small>}
                    </td>
                    <td>
                      <StatusBadge value={d.status} />
                      {!!d.errorCode && <small>{d.errorCode}</small>}
                      {d.busy && <small>lease held</small>}
                    </td>
                    <td>
                      <code>{d.devicePath || "—"}</code>
                    </td>
                    <td>
                      <code>{d.mountPoint || "—"}</code>
                      {d.externalMounts?.map((m) => (
                        <small key={m}>outside ROTUP: {m}</small>
                      ))}
                    </td>
                    <td>{bytes(d.sizeBytes)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
          <section className="panel">
            <h2>Raw allowlisted response</h2>
            <JsonDetails value={query.data} />
          </section>
        </>
      )}
    </>
  );
}
