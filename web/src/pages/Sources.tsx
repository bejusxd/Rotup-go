import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { mutate } from "../api/client";
import { useQuery } from "../api/hooks";
import type { Run, Source, SourceDetail, SourceInput } from "../api/types";
import {
  ActionButton,
  Alert,
  ConfirmDialog,
  date,
  EmptyState,
  ErrorAlert,
  JsonDetails,
  label as labelResult,
  LoadingState,
  PageHeader,
  RunList,
  StatusBadge,
  TechnicalDetails,
} from "../components";

export function SourcesPage() {
  const query = useQuery<{ sources: Source[] }>("/sources", 15000);
  return (
    <>
      <PageHeader
        title="Sources"
        description="Network shares that ROTUP collects into a local copy."
      >
        <Link className="button" to="/sources/new">
          Add source
        </Link>
      </PageHeader>
      {query.loading ? (
        <LoadingState />
      ) : query.error ? (
        <ErrorAlert error={query.error} />
      ) : !query.data!.sources.length ? (
        <EmptyState title="Add your first source">
          <p>
            Connect an SMB share, test the connection, then collect its files.
          </p>
          <Link className="button" to="/sources/new">
            Add source
          </Link>
        </EmptyState>
      ) : (
        <section className="panel table-scroll">
          <table>
            <thead>
              <tr>
                <th>Source</th>
                <th>Local copy</th>
                <th>Last published</th>
                <th>Credentials</th>
              </tr>
            </thead>
            <tbody>
              {query.data!.sources.map((s) => (
                <tr key={s.id}>
                  <td>
                    <Link to={`/sources/${s.id}`}>{s.name}</Link>
                    <small>
                      {s.host} / {s.shareName}
                    </small>
                  </td>
                  <td>
                    <StatusBadge value={s.enabled ? s.status : "DISABLED"} />
                    {s.busy && <small>Operation in progress</small>}
                  </td>
                  <td>{date(s.lastPublishedAt)}</td>
                  <td>
                    {s.hasCredential ? "Password saved" : "No saved password"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
    </>
  );
}

export function SourcePage() {
  const { id } = useParams();
  const query = useQuery<SourceDetail>(
    `/sources/${encodeURIComponent(id!)}`,
    5000,
  );
  const navigate = useNavigate();
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  if (query.loading) return <LoadingState />;
  if (query.error) return <ErrorAlert error={query.error} />;
  const { source, runs, revisionBatches, historyLimit } = query.data!;
  const test = runs.find((r) => r.kind === "CONNECTION_TEST");
  const collection = runs.find((r) => r.kind === "COLLECTION");
  const start = async (action: string) => {
    setBusy(true);
    setError(undefined);
    try {
      const result = await mutate<{ runId: string }>(
        `/sources/${source.id}/${action}`,
      );
      navigate(`/runs/${result.runId}`);
    } catch (error) {
      setError(error);
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <PageHeader
        title={source.name}
        description={`${source.host} / ${source.shareName}`}
      >
        <Link className="button secondary" to={`/sources/${source.id}/edit`}>
          Edit settings
        </Link>
        <ActionButton
          busy={busy}
          disabled={source.busy}
          onClick={() => void start("test")}
        >
          Test connection
        </ActionButton>
        <ConfirmDialog
          title="Collect this source?"
          actionLabel="Collect now"
          disabled={busy || source.busy}
          onConfirm={async () => {
            const result = await mutate<{ runId: string }>(
              `/sources/${source.id}/collect`,
            );
            navigate(`/runs/${result.runId}`);
          }}
        >
          <p>
            This updates the local copy on this machine using the saved policy.
            It does not write anything to a backup disk.
          </p>
          <p>
            ROTUP will check the source and staging safety conditions before it
            starts.
          </p>
        </ConfirmDialog>
      </PageHeader>
      {!!error && <ErrorAlert error={error} />}
      {source.busy && (
        <Alert tone="info">
          An operation is already running for this source. This page refreshes
          its status automatically.
        </Alert>
      )}
      {source.status === "RECOVERY_REQUIRED" && (
        <Alert>
          ROTUP cannot currently prove that this local copy is consistent.
          Review the last collection result. Backend recovery checks remain in
          control.
        </Alert>
      )}
      <section className="panel">
        <h2>Local copy</h2>
        <dl>
          <dt>Status</dt>
          <dd>
            <StatusBadge value={source.enabled ? source.status : "DISABLED"} />
          </dd>
          <dt>Last published collection</dt>
          <dd>
            {source.lastCollectionRunId ? (
              <Link to={`/runs/${source.lastCollectionRunId}`}>
                {date(source.lastPublishedAt)}
              </Link>
            ) : (
              "Not recorded"
            )}
          </dd>
          <dt>Last connection test</dt>
          <dd>
            {test ? (
              <Link to={`/runs/${test.id}`}>
                {labelResult(test.status)} ·{" "}
                {date(test.finishedAt ?? test.requestedAt)}
              </Link>
            ) : (
              "Not in recent history"
            )}
          </dd>
          <dt>Last collection attempt</dt>
          <dd>
            {collection ? (
              <Link to={`/runs/${collection.id}`}>
                {labelResult(collection.status)} ·{" "}
                {date(collection.finishedAt ?? collection.requestedAt)}
              </Link>
            ) : (
              "Not in recent history"
            )}
          </dd>
        </dl>
        <p className="muted">{sourceNextStep(source, test, collection)}</p>
      </section>
      <CredentialForm key={source.id} source={source} onSaved={query.reload} />
      <section className="panel">
        <h2>Source activity</h2>
        <p className="muted small">
          Latest {historyLimit} runs for this source. Older activity may exist.
        </p>
        <RunList runs={runs} sources={{ [source.id]: source.name }} />
      </section>
      <TechnicalDetails>
        <p>
          Staging state version <code>{source.stateVersion}</code> · staging
          path <code>{source.stagingRelativePath}</code> · policy{" "}
          <code>{source.policy}</code>
        </p>
        <JsonDetails value={{ source, revisionBatches }} />
        <p>The JSON above contains no saved password or credential locator.</p>
        <a href={`/legacy/sources/${source.id}/staging`}>
          Browse staging in the legacy interface
        </a>
      </TechnicalDetails>
    </>
  );
}

// Derived from what the backend already recorded; it suggests, and never
// implies that ROTUP has verified anything it has not.
function sourceNextStep(source: Source, test?: Run, collection?: Run) {
  if (!source.enabled)
    return "This source is disabled. Enable it in settings before collecting.";
  if (!source.hasCredential)
    return "Save the share password below, then test the connection.";
  if (source.status === "RECOVERY_REQUIRED")
    return "Review the last collection result before collecting again.";
  if (!test || test.status !== "SUCCEEDED")
    return "Test the connection to confirm ROTUP can read this share.";
  if (!source.lastPublishedAt)
    return "Collect this source to create its first local copy.";
  if (collection && collection.status !== "SUCCEEDED")
    return "The last collection attempt did not succeed. Review it, then collect again.";
  return "A local copy is published. Back it up to a registered disk from Backup storage.";
}

export function SourceEditPage() {
  const { id } = useParams();
  const query = useQuery<SourceDetail>(`/sources/${encodeURIComponent(id!)}`);
  if (query.loading) return <LoadingState />;
  if (query.error) return <ErrorAlert error={query.error} />;
  return (
    <>
      <PageHeader
        title="Edit source"
        description="Changes are validated by ROTUP before they are saved."
      />
      <SourceForm key={id} initial={query.data!.source} />
    </>
  );
}
export function SourceNewPage() {
  return (
    <>
      <PageHeader
        title="Add source"
        description="Start with the network share. You can save its password in the next step."
      />
      <SourceForm />
    </>
  );
}

export function SourceForm({ initial }: { initial?: Source }) {
  const navigate = useNavigate();
  const [value, setValue] = useState<SourceInput>(() =>
    initial
      ? sourceInput(initial)
      : {
          name: "",
          host: "",
          shareName: "",
          subdirectory: "",
          username: "",
          stagingRelativePath: `source-${crypto.randomUUID().slice(0, 8)}`,
          enabled: true,
          policy: "archive_then_sync",
          includeRules: [],
          excludeRules: [],
        },
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  function field(key: keyof SourceInput, text: string, required = false) {
    return (
      <label>
        {text}
        <input
          value={String(value[key])}
          required={required}
          onChange={(e) => setValue({ ...value, [key]: e.target.value })}
        />
      </label>
    );
  }
  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const result = await mutate<{ id: string }>(
        initial ? `/sources/${initial.id}` : "/sources",
        // Editing a rules box down to nothing leaves one empty line behind;
        // an empty rule is not a rule and must not reach the collector.
        {
          ...value,
          includeRules: value.includeRules.filter((r) => r.trim()),
          excludeRules: value.excludeRules.filter((r) => r.trim()),
        },
        initial ? "PUT" : "POST",
      );
      navigate(`/sources/${result.id}`);
    } catch (error) {
      setError(error);
    } finally {
      setBusy(false);
    }
  }
  return (
    <form className="panel form-width" onSubmit={submit}>
      {!!error && <ErrorAlert error={error} />}
      <fieldset disabled={busy}>
        {field("name", "Name", true)}
        <div className="form-grid">
          {field("host", "Server / host", true)}
          {field("shareName", "Share", true)}
        </div>
        {field("username", "Username")}
        <p className="muted">
          Passwords are managed separately and never returned to the browser.
        </p>
        <details className="technical">
          <summary>Advanced settings</summary>
          <div>
            {field("subdirectory", "Subdirectory (optional)")}
            {field(
              "stagingRelativePath",
              "Staging destination (relative folder)",
              true,
            )}
            <label>
              Collection policy
              <select
                value={value.policy}
                onChange={(e) => setValue({ ...value, policy: e.target.value })}
              >
                <option value="archive_then_sync">
                  Archive replaced or deleted local files, then update
                </option>
                <option value="copy_only">
                  Copy additions and changes only
                </option>
              </select>
            </label>
            <p className="muted small">
              The policy affects local staging, not the SMB source. The backend
              enforces the supported behavior.
            </p>
            <div className="form-grid">
              {(["includeRules", "excludeRules"] as const).map((key) => (
                <label key={key}>
                  {key === "includeRules"
                    ? "Include rules (one per line)"
                    : "Exclude rules (one per line)"}
                  <textarea
                    rows={4}
                    value={value[key].join("\n")}
                    onChange={(e) =>
                      setValue({ ...value, [key]: e.target.value.split("\n") })
                    }
                  />
                </label>
              ))}
            </div>
            <label className="checkbox">
              <input
                type="checkbox"
                checked={value.enabled}
                onChange={(e) =>
                  setValue({ ...value, enabled: e.target.checked })
                }
              />
              Source enabled
            </label>
          </div>
        </details>
        <div className="actions form-actions">
          <ActionButton busy={busy}>Save source</ActionButton>
          <Link to={initial ? `/sources/${initial.id}` : "/sources"}>
            Cancel
          </Link>
        </div>
      </fieldset>
    </form>
  );
}
function sourceInput(s: Source): SourceInput {
  return {
    name: s.name,
    host: s.host,
    shareName: s.shareName,
    subdirectory: s.subdirectory,
    username: s.username,
    stagingRelativePath: s.stagingRelativePath,
    enabled: s.enabled,
    policy: s.policy,
    includeRules: s.includeRules,
    excludeRules: s.excludeRules,
  };
}
export function CredentialForm({
  source,
  onSaved,
}: {
  source: Source;
  onSaved: () => void;
}) {
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [saved, setSaved] = useState(false);
  return (
    <section className="panel form-width">
      <h2>Connection password</h2>
      <p>
        {source.hasCredential
          ? "A password is saved. Its value cannot be displayed."
          : "No password is saved for this source."}
      </p>
      {saved && <Alert tone="info">Password saved.</Alert>}
      {!!error && <ErrorAlert error={error} />}
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(undefined);
          setSaved(false);
          try {
            await mutate(`/sources/${source.id}/credential`, {
              action: "replace",
              password,
            });
            setSaved(true);
            onSaved();
          } catch (error) {
            setError(error);
          } finally {
            setPassword("");
            setBusy(false);
          }
        }}
      >
        <label>
          New password
          <input
            type="password"
            required
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            disabled={busy}
          />
        </label>
        <div className="actions">
          <ActionButton busy={busy}>
            {source.hasCredential ? "Replace saved password" : "Save password"}
          </ActionButton>
        </div>
      </form>
      {source.hasCredential && (
        <div className="form-actions">
          <ConfirmDialog
            title="Clear the saved password?"
            actionLabel="Clear saved password"
            disabled={busy}
            onConfirm={async () => {
              await mutate(`/sources/${source.id}/credential`, {
                action: "clear",
              });
              onSaved();
            }}
          >
            <p>
              Future connection tests and collections may fail until you save a
              password again.
            </p>
          </ConfirmDialog>
        </div>
      )}
    </section>
  );
}
