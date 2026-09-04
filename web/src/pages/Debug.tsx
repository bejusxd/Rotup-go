import { useQuery } from "../api/hooks";
import type { DebugStatus } from "../api/types";
import {
  EmptyState,
  ErrorAlert,
  JsonDetails,
  LoadingState,
  PageHeader,
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
        <section className="panel">
          <p>
            Device paths are ephemeral observations, never persistent identity.
            Run details contain the recorded backup read-set and durability
            stage.
          </p>
          <JsonDetails value={query.data} />
        </section>
      )}
    </>
  );
}
