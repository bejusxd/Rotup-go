import { createContext, useContext, useEffect, useRef, useState, type ButtonHTMLAttributes, type ReactNode } from 'react'
import { Link } from 'react-router'
import { RequestError } from './api/client'
import type { Run, System, Timestamp } from './api/types'

export const SystemContext = createContext<System | null>(null)
export const useSystem = () => useContext(SystemContext)!

const labels: Record<string, string> = {
  STABLE: 'Ready', RECONCILING: 'Updating local copy', RECOVERY_REQUIRED: 'Needs attention',
  AVAILABLE_UNMOUNTED: 'Disk ready', ABSENT: 'Not connected', MOUNTED: 'Mounted', MOUNTED_VERIFIED: 'Mounted',
  UNKNOWN: 'Unknown', SUSPECT: 'Needs verification', HEALTHY: 'Healthy', UNINITIALIZED: 'Not prepared',
  SUCCEEDED: 'Completed', FAILED: 'Failed', INTERRUPTED: 'Interrupted', CANCELLED: 'Cancelled',
  RUNNING: 'In progress', QUEUED: 'Waiting', REQUESTED: 'Requested',
  CONNECTION_TEST: 'Connection test', COLLECTION: 'Data collection', BACKUP_LATEST: 'Backup',
  REPOSITORY_INIT: 'Prepare backup storage', REPOSITORY_INITIALIZE: 'Prepare backup storage',
  REPOSITORY_ADOPT: 'Connect existing storage', REPOSITORY_RECOVERY: 'Storage verification',
  REPOSITORY_RECOVER: 'Storage verification', DEMO: 'Demo (not a backup)',
  DISABLED: 'Disabled', UNREGISTERED: 'Not registered', UNSUPPORTED: 'Not supported',
  MOUNTED_EXTERNALLY: 'Mounted outside ROTUP', AMBIGUOUS: 'Identity conflict',
  DISCONNECTED: 'Not connected', DETECTED: 'Detected', READY: 'Mounted and verified', SAFE_TO_REMOVE: 'Safe to remove',
  ERROR: 'Needs attention', UPDATING: 'Updating local copy', PENDING: 'Waiting', PARTIAL: 'Incomplete',
  SUCCEEDED_WITH_WARNING: 'Completed with warnings', DISK_SETUP: 'Prepare backup storage', VERIFY: 'Storage verification',
}
export const label = (value: string) => labels[value] ?? value.replaceAll('_', ' ').toLowerCase().replace(/^./, c => c.toUpperCase())
export const date = (value: Timestamp) => value ? new Date(value).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' }) : 'Not recorded'
export const bytes = (value: number) => value > 0 ? new Intl.NumberFormat(undefined, { style: 'unit', unit: 'gigabyte', maximumFractionDigits: 1 }).format(value / 1e9) : 'Not available'

export function StatusBadge({ value }: { value: string }) {
  const good = ['STABLE', 'AVAILABLE_UNMOUNTED', 'SUCCEEDED', 'HEALTHY', 'running', 'ok'].includes(value)
  const bad = ['FAILED', 'RECOVERY_REQUIRED', 'SUSPECT', 'AMBIGUOUS', 'INTERRUPTED', 'UNAVAILABLE', 'ERROR', 'PARTIAL'].includes(value)
  return <span className={`badge ${good ? 'good' : bad ? 'bad' : 'neutral'}`}><span aria-hidden="true" className="dot" />{label(value)}</span>
}
export function PageHeader({ title, description, children }: { title: string; description?: string; children?: ReactNode }) {
  useEffect(() => { document.title = `${title} · ROTUP` }, [title])
  return <header className="page-header"><div><p className="eyebrow">Backup operations</p><h1>{title}</h1>{description && <p className="muted">{description}</p>}</div><div className="actions">{children}</div></header>
}
export function Alert({ children, tone = 'warning' }: { children: ReactNode; tone?: 'warning' | 'danger' | 'info' }) {
  return <div className={`alert ${tone}`} role={tone === 'danger' ? 'alert' : 'status'}>{children}</div>
}
export function TechnicalDetails({ children, title = 'Technical details' }: { children: ReactNode; title?: string }) {
  const dev = useContext(SystemContext)?.devMode ?? false
  return <details className="technical" open={dev}><summary>{title}</summary><div>{children}</div></details>
}
export function ErrorAlert({ error }: { error: unknown }) {
  const detail = error instanceof RequestError ? error.detail : null
  return <Alert tone="danger"><strong>{detail?.userMessage ?? 'Something went wrong while loading this page.'}</strong>
    <TechnicalDetails><p>{detail?.message ?? 'An unexpected client error occurred.'}</p>{detail && <code>{detail.class} / {detail.code}</code>}</TechnicalDetails>
  </Alert>
}
export function LoadingState() { return <p className="loading" role="status">Loading ROTUP data…</p> }
export function EmptyState({ title, children }: { title: string; children?: ReactNode }) {
  return <div className="empty"><h2>{title}</h2>{children}</div>
}
export function ActionButton({ busy, children, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { busy?: boolean }) {
  return <button {...props} disabled={props.disabled || busy} aria-busy={busy}>{busy ? 'Working…' : children}</button>
}
export function ConfirmDialog({ title, children, onConfirm, disabled, actionLabel }: { title: string; children: ReactNode; onConfirm: () => Promise<void>; disabled?: boolean; actionLabel: string }) {
  const ref = useRef<HTMLDialogElement>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  return <><button disabled={disabled} onClick={() => { setError(undefined); ref.current?.showModal() }}>{actionLabel}</button>
    <dialog ref={ref} aria-label={title} onCancel={event => { if (busy) event.preventDefault() }}><h2>{title}</h2>{children}{!!error && <ErrorAlert error={error} />}
      <div className="actions"><button className="secondary" disabled={busy} onClick={() => ref.current?.close()}>Cancel</button><ActionButton busy={busy} onClick={async () => {
        setBusy(true); setError(undefined)
        try { await onConfirm(); ref.current?.close() } catch (error) { setError(error) } finally { setBusy(false) }
      }}>{actionLabel}</ActionButton></div>
    </dialog></>
}
export function RunList({ runs, sources = {} }: { runs: Run[]; sources?: Record<string, string> }) {
  if (!runs.length) return <EmptyState title="No activity recorded"><p>Your connection tests, collections and backups will appear here.</p></EmptyState>
  return <div className="table-scroll"><table><thead><tr><th>Operation</th><th>Result</th><th>Source</th><th>Started / requested</th><th>Duration</th></tr></thead><tbody>
    {runs.map(run => <tr key={run.id}><td><Link to={`/runs/${run.id}`}>{label(run.kind)}</Link></td><td><StatusBadge value={run.status} /></td><td>{sources[run.sourceId] || (run.sourceId ? <Link to={`/sources/${run.sourceId}`}>View source</Link> : '—')}</td><td>{date(run.startedAt ?? run.requestedAt)}</td><td>{run.terminal ? `${(run.durationMs / 1000).toFixed(1)} s` : 'In progress'}</td></tr>)}
  </tbody></table></div>
}
export function JsonDetails({ value }: { value: unknown }) { return <pre>{JSON.stringify(value, null, 2)}</pre> }
