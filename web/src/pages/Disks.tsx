import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { mutate } from "../api/client";
import { useQuery } from "../api/hooks";
import type { Disk, DisksResponse, Repository } from "../api/types";
import {
  ActionButton,
  Alert,
  bytes,
  ConfirmDialog,
  date,
  diskErrorExplanation,
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
        <button
          className="secondary"
          disabled={query.refreshing}
          onClick={query.reload}
        >
          {query.refreshing ? "Refreshing…" : "Refresh discovery"}
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
              Register only a disk you recognise as yours to use for backups.
              ROTUP tracks it by filesystem identity, never by a device name,
              and never formats or erases anything.
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
// A filesystem ROTUP will not register must say why in words the operator can
// act on. The refusal itself stays with the backend; this only explains the
// state the disk list already reports.
function registrationRefusal(disk: Disk) {
  if (disk.ambiguous)
    return "More than one connected filesystem reports this identity, so ROTUP cannot tell them apart. Disconnect the others and refresh.";
  if (!disk.supported)
    return `ROTUP cannot use ${disk.filesystemType || "this filesystem"} for backup storage. Use a supported filesystem, such as ext4.`;
  if (!disk.filesystemUuid)
    return "This filesystem has no UUID. ROTUP identifies a backup disk by its filesystem UUID and will not track one it cannot name.";
  if (disk.errorCode) return diskErrorExplanation(disk.errorCode);
  return "";
}
function RegisterDisk({ disk }: { disk: Disk }) {
  const [name, setName] = useState(disk.name || "");
  const navigate = useNavigate();
  const refusal = registrationRefusal(disk);
  return (
    <div className="registration">
      <h3>
        {disk.name || "Unnamed filesystem"} ·{" "}
        {disk.filesystemType || "Unknown filesystem"}
      </h3>
      <p>{bytes(disk.sizeBytes)}</p>
      {refusal ? (
        <Alert>
          <strong>This filesystem cannot be registered.</strong>
          <p>{refusal}</p>
          <TechnicalDetails>
            <p>
              Filesystem UUID: <code>{disk.filesystemUuid || "missing"}</code>
            </p>
            {!!disk.errorCode && <code>{disk.errorCode}</code>}
          </TechnicalDetails>
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
      {!refusal && (
        <TechnicalDetails>
          <p>
            Filesystem UUID: <code>{disk.filesystemUuid}</code>
          </p>
          <JsonDetails value={disk} />
        </TechnicalDetails>
      )}
    </div>
  );
}
export function DiskPage() {
  const { id } = useParams();
  const query = useQuery<{ disk: Disk; enumerationError: string }>(
    `/disks/${encodeURIComponent(id!)}`,
    10000,
  );
  // Backup storage already prepared on this disk is what the operator wants
  // next; sending them through the preparation form only to be told it exists
  // is a detour.
  const repositories = useQuery<{ repositories: Repository[] }>(
    "/repositories",
  );
  if (query.loading) return <LoadingState />;
  if (query.error) return <ErrorAlert error={query.error} />;
  const d = query.data!.disk;
  const storage = repositories.data?.repositories.find(
    (r) => r.diskId === d.id,
  );
  return (
    <>
      <PageHeader
        title={d.name}
        description={
          d.observed
            ? "This disk is connected now."
            : "This disk is not connected. Connect it before a backup."
        }
      >
        {storage ? (
          <Link className="button" to={`/repositories/${storage.id}`}>
            Open backup storage
          </Link>
        ) : (
          <Link
            className="button"
            to={`/repositories/new?disk=${encodeURIComponent(d.id)}`}
          >
            Prepare for backups
          </Link>
        )}
      </PageHeader>
      {query.data!.enumerationError && (
        <Alert tone="danger">
          Disk discovery failed: {query.data!.enumerationError}
        </Alert>
      )}
      {!!d.errorCode && (
        <Alert>
          <strong>This disk needs attention.</strong>
          <p>{diskErrorExplanation(d.errorCode)}</p>
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
          <dt>Last connected</dt>
          <dd>{date(d.lastConnectedAt)}</dd>
          <dt>Backup storage</dt>
          <dd>
            {repositories.loading ? (
              "Checking…"
            ) : storage ? (
              <Link to={`/repositories/${storage.id}`}>
                {storage.established ? "Prepared" : "Preparation incomplete"}
              </Link>
            ) : (
              "Not prepared yet"
            )}
          </dd>
        </dl>
        <p className="muted small">
          Capacity is observed while connected. “Last connected” is the last
          connection transition, not a continuous presence guarantee.
        </p>
      </section>
      <DiskSettings key={d.id} disk={d} onSaved={query.reload} />
      <TechnicalDetails>
        <p>
          Permanent identity (filesystem UUID): <code>{d.filesystemUuid}</code>
        </p>
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
