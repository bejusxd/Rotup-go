import { useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router'
import { mutate } from '../api/client'
import { useQuery } from '../api/hooks'
import type { DisksResponse, Repository, RepositoryDetail } from '../api/types'
import { ActionButton, Alert, ConfirmDialog, date, EmptyState, ErrorAlert, JsonDetails, LoadingState, PageHeader, RunList, StatusBadge, TechnicalDetails, useSystem } from '../components'

export function RepositoriesPage() {
  const query = useQuery<{ repositories: Repository[] }>('/repositories', 10000)
  return <><PageHeader title="Backup storage" description="Each rotated disk has its own encrypted restic repository."><Link className="button" to="/repositories/new">Prepare a disk</Link></PageHeader>
    {query.loading ? <LoadingState /> : query.error ? <ErrorAlert error={query.error} /> : !query.data!.repositories.length ? <EmptyState title="No backup storage prepared"><p>Register a disk, then explicitly initialize new storage or connect an existing ROTUP repository.</p><Link className="button" to="/disks">View disks</Link></EmptyState> : <section className="panel table-scroll"><table><thead><tr><th>Storage</th><th>Health</th><th>Disk</th><th>Last successful backup</th></tr></thead><tbody>{query.data!.repositories.map(r => <tr key={r.id}><td><Link to={`/repositories/${r.id}`}>{r.diskName || 'Backup storage'}</Link>{!r.established && <small>Setup not completed</small>}</td><td><StatusBadge value={r.health} /></td><td>{r.connected ? 'Connected' : 'Not connected'}</td><td>{date(r.lastSuccessfulBackupAt)}</td></tr>)}</tbody></table></section>}
  </>
}

export function RepositoryPage() {
  const { id } = useParams()
  const query = useQuery<RepositoryDetail>(`/repositories/${encodeURIComponent(id!)}`, 5000)
  const navigate = useNavigate()
  const [selected, setSelected] = useState<string[]>([])
  if (query.loading) return <LoadingState />
  if (query.error) return <ErrorAlert error={query.error} />
  const { repository: r, sources, history, historyLimit } = query.data!
  return <><PageHeader title={r.diskName || 'Backup storage'} description="Encrypted backup storage on this registered disk."><Link className="button secondary" to={`/disks/${r.diskId}`}>View disk</Link><StatusBadge value={r.health} /></PageHeader>
    {!r.connected && <Alert>Connect this disk before starting a backup or verification. Idle disks normally remain unmounted.</Alert>}
    {r.reason && <Alert>This storage needs attention. <TechnicalDetails><p>{r.reason}</p></TechnicalDetails></Alert>}
    {r.busy && <Alert tone="info">An operation is already using this storage.</Alert>}
    <section className="panel"><h2>Storage status</h2><dl><dt>Prepared</dt><dd>{r.established ? date(r.establishedAt) : 'Not yet established'}</dd><dt>Last successful backup</dt><dd>{date(r.lastSuccessfulBackupAt)}</dd><dt>Last successful verification</dt><dd>{query.data!.lastVerification ? <Link to={`/runs/${query.data!.lastVerification!.id}`}>{date(query.data!.lastVerification!.finishedAt)}</Link> : 'Not available in recent verification history'}</dd><dt>Encryption password</dt><dd>{r.hasPassword ? 'Saved (not displayed)' : 'Not available'}</dd></dl></section>
    {!r.established ? <section className="panel"><h2>Finish storage preparation</h2><p>ROTUP has not yet established the repository identity. Preparation remains an explicit operation.</p><Link className="button" to={`/repositories/new?disk=${r.diskId}`}>Review preparation</Link></section> : <section className="panel"><h2>Back up local copies</h2><p>Choose sources to back up. ROTUP captures the latest stable local copies when it acquires its staging lock; this does not collect newer SMB files first.</p>
      {sources.length ? sources.map(({ source, eligible, reason }) => <label className="checkbox" key={source.id}><input type="checkbox" disabled={!eligible || r.busy} checked={selected.includes(source.id)} onChange={e => setSelected(e.target.checked ? [...selected, source.id] : selected.filter(id => id !== source.id))} /><span>{source.name}<small className="block">{eligible ? `Local copy version ${source.stateVersion} · ${date(source.lastPublishedAt)}` : reason}</small></span></label>) : <p>No sources are available. <Link to="/sources">Configure a source first.</Link></p>}
      <ConfirmDialog title="Back up the selected sources?" actionLabel="Back up now" disabled={!selected.length || r.busy || !r.connected} onConfirm={async () => {
        const result = await mutate<{ runId: string }>(`/repositories/${r.id}/backup`, { sourceIds: selected }); navigate(`/runs/${result.runId}`)
      }}><p>Target: <strong>{r.diskName}</strong></p><p>Sources: {sources.filter(s => selected.includes(s.source.id)).map(s => s.source.name).join(', ')}</p><p>The backend rechecks eligibility and identity, then mounts, backs up, verifies, syncs and unmounts. Keep the disk connected until the recorded run finishes.</p></ConfirmDialog>
    </section>}
    <section className="panel"><h2>Verification and recovery</h2><p>Use this explicit operation when storage is suspect or a run was interrupted. It checks the repository and resolves recorded interrupted backups. It does not automatically unlock restic or prune data.</p><ConfirmDialog title="Verify this backup storage?" actionLabel="Verify and recover" disabled={r.busy || !r.connected || !r.established} onConfirm={async () => { const result = await mutate<{ runId: string }>(`/repositories/${r.id}/recover`); navigate(`/runs/${result.runId}`) }}><p>ROTUP will use its existing recovery procedure for <strong>{r.diskName}</strong>. Keep the disk connected until it completes.</p></ConfirmDialog></section>
    <section className="panel"><h2>Backup history</h2><p className="small muted">Latest {historyLimit} backup runs for this storage.</p><RunList runs={history.map(row => row.run)} /></section>
    <TechnicalDetails><JsonDetails value={r} /></TechnicalDetails>
  </>
}

export function RepositorySetupPage() {
  const [params] = useSearchParams()
  const disks = useQuery<DisksResponse>('/disks')
  const repositories = useQuery<{ repositories: Repository[] }>('/repositories')
  const [diskId, setDiskId] = useState(params.get('disk') || '')
  const [mode, setMode] = useState('INITIALIZE')
  const [password, setPassword] = useState('')
  const [escrowAcknowledged, setEscrow] = useState(false)
  const [reviewed, setReviewed] = useState(false)
  const [error, setError] = useState<unknown>()
  const system = useSystem()
  const navigate = useNavigate()
  const existing = repositories.data?.repositories.find(r => r.diskId === diskId)
  const selectedDisk = disks.data?.disks.find(d => d.id === diskId && d.registered)
  if (disks.loading || repositories.loading) return <LoadingState />
  if (disks.error || repositories.error) return <ErrorAlert error={disks.error || repositories.error} />
  return <><PageHeader title="Prepare disk for backups" description="An explicit setup operation. No disk is formatted or automatically initialized." />
    {!!error && <ErrorAlert error={error} />}
    {disks.data!.enumerationError && <Alert tone="danger">Disk discovery failed: {disks.data!.enumerationError}</Alert>}
    {!disks.data!.disks.some(d => d.registered) ? <EmptyState title="Register a backup disk first"><Link className="button" to="/disks">View disks</Link></EmptyState> : <section className="panel form-width"><form onSubmit={e => { e.preventDefault(); setReviewed(true) }} onChange={() => setReviewed(false)}>
      <label>Backup disk<select required value={diskId} onChange={e => { setDiskId(e.target.value); setPassword(''); setEscrow(false) }}><option value="">Choose a registered disk</option>{disks.data!.disks.filter(d => d.registered).map(d => <option key={d.id} value={d.id}>{d.name}{d.observed ? '' : ' (not connected)'}</option>)}</select></label>
      {existing?.established ? <Alert tone="info">This disk already has prepared backup storage. <Link to={`/repositories/${existing.id}`}>Open storage</Link></Alert> : <>
        <label>Preparation method<select value={mode} onChange={e => { setMode(e.target.value); setPassword(''); setEscrow(false) }}><option value="INITIALIZE">Initialize new backup storage</option><option value="ADOPT">Connect an existing ROTUP repository</option></select></label>
        <p className="muted">{mode === 'INITIALIZE' ? 'Initialization is only allowed in an empty ROTUP repository directory. It will not overwrite an existing repository.' : 'Use the existing repository password. ROTUP verifies and records its identity; it does not replace it.'}</p>
        <label>{mode === 'INITIALIZE' ? 'New repository password' : 'Existing repository password'}<input type="password" required minLength={mode === 'INITIALIZE' ? system.minimumRepositoryPasswordLength : undefined} autoComplete={mode === 'INITIALIZE' ? 'new-password' : 'current-password'} value={password} onChange={e => setPassword(e.target.value)} /></label>
        <label className="checkbox"><input type="checkbox" required checked={escrowAcknowledged} onChange={e => setEscrow(e.target.checked)} /><span>I have saved this repository password securely outside ROTUP. I understand backups cannot be restored without it.</span></label>
        <ActionButton disabled={!selectedDisk?.observed}>Review preparation</ActionButton>
        {!selectedDisk?.observed && <p className="muted small">Choose a connected disk to continue.</p>}
      </>}
    </form>{reviewed && !existing?.established && <div className="form-actions"><ConfirmDialog title="Confirm backup storage preparation" actionLabel="Prepare backup storage" onConfirm={async () => {
      setError(undefined)
      try { const result = await mutate<{ runId: string }>('/repositories', { diskId, mode, password, escrowAcknowledged }); navigate(`/runs/${result.runId}`) }
      catch (error) { setError(error) }
      finally { setPassword(''); setReviewed(false) }
    }}><p>Disk: <strong>{selectedDisk?.name}</strong></p><p>Filesystem UUID: <code>{selectedDisk?.filesystemUuid}</code></p><p>{mode === 'INITIALIZE' ? 'Initialize new storage' : 'Connect an existing repository'}. The backend performs all identity, ownership and escrow checks.</p></ConfirmDialog></div>}</section>}
  </>
}
