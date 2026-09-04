import { useEffect, useState } from "react";
import { Link, useParams } from "react-router";
import { RequestError } from "../api/client";
import { useQuery } from "../api/hooks";
import type { Run, RunDetail, Source } from "../api/types";
import {
  Alert,
  date,
  duration,
  ErrorAlert,
  JsonDetails,
  label,
  LoadingState,
  PageHeader,
  RunList,
  StatusBadge,
  TechnicalDetails,
} from "../components";

export function RunsPage() {
  const sourceQuery = useQuery<{ sources: Source[] }>("/sources", 15000);
  const query = useQuery<{ runs: Run[]; limit: number }>("/runs", 5000);
  return (
    <>
      <PageHeader
        title="History"
        description="Recorded results, including failed and interrupted operations."
      >
        <button
          className="secondary"
          disabled={query.refreshing}
          onClick={query.reload}
        >
          {query.refreshing ? "Refreshing…" : "Refresh"}
        </button>
      </PageHeader>
      {!!sourceQuery.error && <ErrorAlert error={sourceQuery.error} />}
      {query.loading ? (
        <LoadingState />
      ) : query.error ? (
        <ErrorAlert error={query.error} />
      ) : (
        <section className="panel">
          <p className="small muted">
            Latest {query.data!.limit} runs. This is a bounded view, not a
            deletion of older audit records.
          </p>
          <RunList
            runs={query.data!.runs}
            sources={Object.fromEntries(
              (sourceQuery.data?.sources ?? []).map((s) => [s.id, s.name]),
            )}
          />
        </section>
      )}
    </>
  );
}
export function RunPage() {
  const { id } = useParams();
  const [live, setLive] = useState(true);
  // A finished run never changes again, so stop asking for it.
  const query = useQuery<RunDetail>(
    `/runs/${encodeURIComponent(id!)}`,
    live ? 3000 : 0,
  );
  const terminal = query.data?.run.terminal ?? false;
  useEffect(() => {
    if (terminal) setLive(false);
  }, [terminal]);
  if (query.loading) return <LoadingState />;
  if (query.error) return <ErrorAlert error={query.error} />;
  const { run, source, collection, backup } = query.data!;
  const nextStep = recommendedNextStep(query.data!);
  return (
    <>
      <PageHeader title={label(run.kind)} description={date(run.requestedAt)}>
        <Link className="button secondary" to="/runs">
          All history
        </Link>
        <StatusBadge value={run.status} />
      </PageHeader>
      {!run.terminal && (
        <Alert tone="info">
          This operation is in progress. You can leave this page; ROTUP
          continues independently.
        </Alert>
      )}
      {run.error && <ErrorAlert error={new RequestError(run.error, 0)} />}
      {run.kind === "DEMO" && (
        <Alert>
          This is a demonstration record, not a collection or backup.
        </Alert>
      )}
      {run.kind === "COLLECTION" && (
        <Alert tone="info">
          Collection updates the local copy only. It does not create a backup on
          a rotated disk.
        </Alert>
      )}
      {run.repositoryId && (
        <p>
          <Link to={`/repositories/${run.repositoryId}`}>
            View associated backup storage →
          </Link>
        </p>
      )}
      <section className="panel">
        <h2>Recorded result</h2>
        {/* A failure already states itself above; repeating the internal
            summary verbatim here only reads as a second, rawer error. */}
        {!run.error && (
          <p>
            {run.summary ||
              (run.terminal
                ? "No additional summary was recorded."
                : "Waiting for the operation result.")}
          </p>
        )}
        <dl>
          <dt>Source</dt>
          <dd>
            {source ? (
              <Link to={`/sources/${source.id}`}>{source.name}</Link>
            ) : (
              "Not a source-specific operation"
            )}
          </dd>
          {backup && (
            <>
              <dt>Backup storage</dt>
              <dd>
                <Link to={`/repositories/${backup.repositoryId}`}>
                  View associated storage
                </Link>
              </dd>
              <dt>Durable success</dt>
              <dd>{date(backup.durableSuccessAt)}</dd>
            </>
          )}
          <dt>Started</dt>
          <dd>{date(run.startedAt)}</dd>
          <dt>Finished</dt>
          <dd>{date(run.finishedAt)}</dd>
          <dt>Duration</dt>
          <dd>{run.terminal ? duration(run.durationMs) : "In progress"}</dd>
        </dl>
        {backup && !backup.durableSuccessAt && (
          <Alert>
            No durable backup success is recorded for this run. A snapshot
            identifier alone does not mean the backup completed safely.
          </Alert>
        )}
        {collection && (
          <p>
            Copied {collection.filesCopied} of {collection.filesScanned} scanned
            files into the local copy.{" "}
            {collection.filesDeletedOrArchived > 0
              ? `${collection.filesDeletedOrArchived} local files that were replaced or removed at the source were archived, not deleted.`
              : "No local files had to be archived."}
          </p>
        )}
        {collection?.warnings.map((warning, i) => (
          <Alert key={i}>{warning}</Alert>
        ))}
        {nextStep && (
          <p>
            <Link to={nextStep.href}>{nextStep.text} →</Link>
          </p>
        )}
      </section>
      <TechnicalDetails>
        {collection && (
          <p>
            Published local copy version {collection.stateVersion} ·{" "}
            {collection.bytesCopied} of {collection.bytesScanned} bytes copied ·{" "}
            {collection.engineErrors} engine errors
          </p>
        )}
        <p>
          Run <code>{run.id}</code> · trigger <code>{run.trigger}</code> ·
          internal status <code>{run.status}</code>
        </p>
        <JsonDetails value={query.data} />
      </TechnicalDetails>
    </>
  );
}
// Only outcomes whose follow-up is unambiguous get a suggestion. A failure
// whose cause ROTUP has not established must not be given a confident remedy.
function recommendedNextStep(detail: RunDetail) {
  const { run, source } = detail;
  if (!run.terminal) return null;
  if (run.kind === "COLLECTION" && run.status === "SUCCEEDED")
    return { text: "Back this local copy up to a disk", href: "/repositories" };
  if (run.kind === "CONNECTION_TEST" && run.status === "SUCCEEDED" && source)
    return {
      text: "Collect this source",
      href: `/sources/${source.id}`,
    };
  if (
    run.kind === "CONNECTION_TEST" &&
    run.status !== "SUCCEEDED" &&
    source &&
    run.error?.class === "AUTHENTICATION"
  )
    return {
      text: "Check the username and saved password",
      href: `/sources/${source.id}`,
    };
  return null;
}
