import { Link } from 'react-router'
import { useQuery } from '../api/hooks'
import type { Dashboard } from '../api/types'
import { Alert, date, ErrorAlert, JsonDetails, LoadingState, PageHeader, RunList, StatusBadge, TechnicalDetails } from '../components'

export function DashboardPage() {
  const query = useQuery<Dashboard>('/dashboard', 15000)
  if (query.loading) return <LoadingState />
  if (query.error) return <><PageHeader title="Overview" /><ErrorAlert error={query.error} /><button onClick={query.reload}>Refresh</button></>
  const data = query.data!
  const registered = data.storage.disks.filter(d => d.registered)
  const latestFailure = data.recentRuns.find(r => ['FAILED', 'INTERRUPTED'].includes(r.status))
  const needsAttention = data.repositories.some(r => r.health !== 'HEALTHY') || data.sources.some(s => s.status === 'RECOVERY_REQUIRED') || !!data.storage.enumerationError || !data.storage.platformAvailable
  const steps = [
    { text: 'Source configured', done: data.sources.length > 0, href: '/sources' },
    { text: 'Connection test succeeded in recent history', done: data.recentRuns.some(r => r.kind === 'CONNECTION_TEST' && r.status === 'SUCCEEDED'), href: '/sources' },
    { text: 'First collection published', done: data.sources.some(s => s.lastPublishedAt), href: '/sources' },
    { text: 'Backup disk registered', done: registered.length > 0, href: '/disks' },
    { text: 'Backup storage prepared', done: data.repositories.some(r => r.established), href: '/repositories' },
    { text: 'First durable backup completed', done: !!data.lastSuccessfulBackupAt, href: '/repositories' },
  ]
  const next = steps.find(s => !s.done)
  return <><PageHeader title="Overview" description="Your sources, local copies and backup disks — in one place."><button className="secondary" onClick={query.reload}>Refresh</button><Link className="button" to={next?.href ?? '/repositories'}>{!data.sources.length ? 'Add a source' : 'Manage backups'}</Link></PageHeader>
    <section className={`overview-status ${needsAttention ? 'attention' : ''}`}><div><p className="eyebrow">At a glance</p><h2>{needsAttention ? 'Some items need attention' : !data.lastSuccessfulBackupAt ? 'Ready to set up your first backup' : 'Backup history is available'}</h2><p>{data.lastSuccessfulBackupAt ? 'Last durable backup completed ' + date(data.lastSuccessfulBackupAt) + '. Review each disk below; this does not guarantee every source is current.' : 'No completed durable backup is recorded yet. A collected local copy is not a USB backup.'}</p></div><StatusBadge value={data.applicationStatus} /></section>
    {data.storage.enumerationError && <Alert tone="danger">Disk discovery failed: {data.storage.enumerationError}</Alert>}
    {!data.storage.platformAvailable && <Alert>Media support is unavailable. {data.storage.platformReason}</Alert>}
    <section className="metrics" aria-label="System summary"><div><span>Last successful backup</span><strong>{data.lastSuccessfulBackupAt ? date(data.lastSuccessfulBackupAt) : 'Not yet completed'}</strong><Link to="/repositories">View backup storage →</Link></div><div><span>Sources</span><strong>{data.sources.length} configured</strong><Link to="/sources">Manage sources →</Link></div><div><span>Backup disks</span><strong>{registered.filter(d => d.observed).length} of {registered.length} connected</strong><Link to="/disks">View disks →</Link></div></section>
    {!data.lastSuccessfulBackupAt && <section className="panel"><h2>ROTUP setup</h2><p className="muted">Complete these steps to protect your first local collection.</p><ol className="checklist">{steps.map(step => <li key={step.text}><span aria-label={step.done ? 'Complete' : 'Not confirmed'} className={step.done ? 'step done' : 'step'}>{step.done ? '✓' : '○'}</span><span>{step.text}</span><Link to={step.href}>{step.done ? 'View' : 'Continue'} →</Link></li>)}</ol><p className="muted small">Connection evidence is limited to the latest {data.recentRunLimit} runs. An unconfirmed step does not prove it never happened.</p></section>}
    <div className="columns"><section className="panel"><h2>Source health</h2>{!data.sources.length && <p className="muted">No sources configured.</p>}{data.sources.map(s => <div className="list-row" key={s.id}><Link to={`/sources/${s.id}`}>{s.name}</Link><StatusBadge value={s.enabled ? s.status : 'DISABLED'} /></div>)}</section><section className="panel"><h2>Backup storage</h2>{!data.repositories.length && <p className="muted">Prepare a registered disk to store backups.</p>}{data.repositories.map(r => <div className="list-row" key={r.id}><div><Link to={`/repositories/${r.id}`}>{r.diskName || 'Backup storage'}</Link><small>{r.connected ? 'Disk connected' : 'Disk not connected'}</small></div><StatusBadge value={r.health} /></div>)}</section></div>
    {latestFailure && <Alert tone="warning"><strong>Most recent failed or interrupted run</strong><p>{latestFailure.error?.userMessage || latestFailure.summary}</p><Link to={`/runs/${latestFailure.id}`}>Review recorded result →</Link><p className="small">Historical result; a later operation may have resolved it.</p></Alert>}
    <section className="panel"><div className="section-heading"><h2>Recent activity</h2><Link to="/runs">View history →</Link></div><RunList runs={data.recentRuns.slice(0, 6)} sources={Object.fromEntries(data.sources.map(s => [s.id, s.name]))} /></section>
    <TechnicalDetails><JsonDetails value={{ applicationStatus: data.applicationStatus, databaseStatus: data.databaseStatus, lastSuccessfulBackupAt: data.lastSuccessfulBackupAt }} /></TechnicalDetails>
  </>
}
