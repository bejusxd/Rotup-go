import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { mutate } from "../api/client";
import { useQuery } from "../api/hooks";
import type { Disk, DisksResponse } from "../api/types";
import {
  ActionButton,
  Alert,
  bytes,
  ConfirmDialog,
  date,
  EmptyState,
  ErrorAlert,
  JsonDetails,
  LoadingState,
  PageHeader,
  StatusBadge,
  TechnicalDetails,
} from "../components";

export function DisksPage() {
  const query = useQuery<DisksResponse>("/disks", 10000);
  return (
    <>
      <PageHeader
        title="Disks"
        description="Registered backup disks stay unmounted while idle."
      >
        <button className="secondary" onClick={query.reload}>
          Refresh discovery
        </button>
      </PageHeader>
      {query.loading ? (
        <LoadingState />
      ) : query.error ? (
        <ErrorAlert error={query.error} />
      ) : (
        <>
          {!query.data!.platformAvailable && (
            <Alert>
              Media support is unavailable. {query.data!.platformReason}
            </Alert>
          )}
          {query.data!.enumerationError && (
            <Alert tone="danger">
              Disk discovery failed: {query.data!.enumerationError}. Presence
              cannot be confirmed.
            </Alert>
          )}
          <section className="panel">
            <h2>Registered disks</h2>
            {!query.data!.disks.some((d) => d.registered) ? (
              <EmptyState title="No disks registered">
                <p>
                  Connect a supported filesystem, then register it below.
                  Registration does not format the disk.
                </p>
              </EmptyState>
            ) : (
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th>Disk</th>
                      <th>Status</th>
                      <th>Capacity</th>
                      <th>Filesystem</th>
                      <th>Last connected</th>
                    </tr>
                  </thead>
                  <tbody>
                    {query
                      .data!.disks.filter((d) => d.registered)
                      .map((d) => (
                        <tr key={d.id}>
                          <td>
                            <Link to={`/disks/${d.id}`}>{d.name}</Link>
                            {!d.enabled && <small>Disabled</small>}
                          </td>
                          <td>
                            <StatusBadge value={d.status} />
                          </td>
                          <td>{bytes(d.sizeBytes)}</td>
                          <td>{d.filesystemType}</td>
                          <td>{date(d.lastConnectedAt)}</td>
                        </tr>
                      ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
          <section className="panel">
            <h2>Available to register</h2>
            <p className="muted">
              Select by filesystem identity, never by a device name. No
              formatting is performed.
            </p>
            {query
              .data!.disks.filter((d) => !d.registered)
              .map((d, i) => (
                <RegisterDisk key={`${d.filesystemUuid}-${i}`} disk={d} />
              ))}
            {!query.data!.disks.some((d) => !d.registered) && (
              <p>No unregistered filesystems detected.</p>
            )}
          </section>
        </>
      )}
    </>
  );
}
function RegisterDisk({ disk }: { disk: Disk }) {
  const [name, setName] = useState(disk.name || "");
  const navigate = useNavigate();
  return (
    <div className="registration">
      <h3>
        {disk.name || "Unnamed filesystem"} ·{" "}
        {disk.filesystemType || "Unknown filesystem"}
      </h3>
      <p>{bytes(disk.sizeBytes)}</p>
      <p className="small">
        Filesystem UUID: <code>{disk.filesystemUuid || "Missing"}</code>
      </p>
      {!disk.supported || disk.ambiguous || !disk.filesystemUuid ? (
        <Alert>
          This filesystem cannot be registered safely. {disk.errorCode}
        </Alert>
      ) : (
        <>
          <label>
            Friendly disk name
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. Backup disk A"
            />
          </label>
          <ConfirmDialog
            title="Register this backup disk?"
            actionLabel="Register disk"
            disabled={!name.trim()}
            onConfirm={async () => {
              const result = await mutate<{ id: string }>("/disks/register", {
                name,
                filesystemUuid: disk.filesystemUuid,
              });
              navigate(`/disks/${result.id}`);
            }}
          >
            <p>
              Register <strong>{name}</strong> with filesystem UUID{" "}
              <code>{disk.filesystemUuid}</code>?
            </p>
            <p>
              The backend will freshly check that this identity is unique and
              supported. Existing files are not formatted or erased.
            </p>
          </ConfirmDialog>
        </>
      )}
      <TechnicalDetails>
        <JsonDetails value={disk} />
      </TechnicalDetails>
    </div>
  );
}
export function DiskPage() {
  const { id } = useParams();
  const query = useQuery<{ disk: Disk; enumerationError: string }>(
    `/disks/${encodeURIComponent(id!)}`,
    10000,
  );
  if (query.loading) return <LoadingState />;
  if (query.error) return <ErrorAlert error={query.error} />;
  const d = query.data!.disk;
  return (
    <>
      <PageHeader
        title={d.name}
        description="The filesystem UUID is this disk’s permanent identity."
      >
        <Link
          className="button"
          to={`/repositories/new?disk=${encodeURIComponent(d.id)}`}
        >
          Manage backup storage
        </Link>
      </PageHeader>
      {query.data!.enumerationError && (
        <Alert tone="danger">
          Disk discovery failed: {query.data!.enumerationError}
        </Alert>
      )}
      {!!d.errorCode && (
        <Alert>
          This disk needs attention.{" "}
          <TechnicalDetails>
            <code>{d.errorCode}</code>
          </TechnicalDetails>
        </Alert>
      )}
      <section className="panel">
        <dl>
          <dt>Status</dt>
          <dd>
            <StatusBadge value={d.status} />
          </dd>
          <dt>Capacity</dt>
          <dd>{bytes(d.sizeBytes)}</dd>
          <dt>Filesystem</dt>
          <dd>{d.filesystemType}</dd>
          <dt>Currently detected</dt>
          <dd>{d.observed ? "Yes" : "No"}</dd>
          <dt>Last connected</dt>
          <dd>{date(d.lastConnectedAt)}</dd>
        </dl>
        <p className="muted small">
          Capacity is observed while connected. “Last connected” is the last
          connection transition, not a continuous presence guarantee.
        </p>
      </section>
      <DiskSettings key={d.id} disk={d} onSaved={query.reload} />
      <TechnicalDetails>
        <JsonDetails value={d} />
        <h3>Manual mount diagnostics</h3>
        <p>
          Normal backups manage mounting automatically. Keep disks unmounted
          when idle.
        </p>
        <div className="actions">
          <ConfirmDialog
            title="Mount for diagnostics?"
            actionLabel="Mount disk"
            disabled={d.busy}
            onConfirm={async () => {
              await mutate(`/disks/${d.id}/mount`);
              query.reload();
            }}
          >
            <p>
              ROTUP will re-resolve and verify the registered filesystem UUID
              before and after mounting. Remember to safely unmount when
              finished.
            </p>
          </ConfirmDialog>
          <ConfirmDialog
            title="Sync and unmount?"
            actionLabel="Safely unmount"
            disabled={d.busy}
            onConfirm={async () => {
              await mutate(`/disks/${d.id}/unmount`);
              query.reload();
            }}
          >
            <p>
              Flush pending writes and verify that the mount is gone before
              removing the disk.
            </p>
          </ConfirmDialog>
        </div>
      </TechnicalDetails>
    </>
  );
}
function DiskSettings({ disk, onSaved }: { disk: Disk; onSaved: () => void }) {
  const [name, setName] = useState(disk.name);
  const [enabled, setEnabled] = useState(disk.enabled);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  return (
    <form
      className="panel form-width"
      onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        setError(undefined);
        try {
          await mutate(`/disks/${disk.id}`, { name, enabled }, "PUT");
          onSaved();
        } catch (error) {
          setError(error);
        } finally {
          setBusy(false);
        }
      }}
    >
      <h2>Disk settings</h2>
      {!!error && <ErrorAlert error={error} />}
      <fieldset disabled={busy}>
        <label>
          Friendly name
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
        </label>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          Disk enabled for ROTUP
        </label>
        <ActionButton busy={busy}>Save disk settings</ActionButton>
      </fieldset>
    </form>
  );
}
