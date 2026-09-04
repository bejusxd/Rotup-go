import { useEffect, useState } from 'react'
import { Link, NavLink, Outlet, Route, Routes, useLocation } from 'react-router'
import { loadSystem } from './api/client'
import type { System } from './api/types'
import { Alert, EmptyState, ErrorAlert, LoadingState, SystemContext, useSystem } from './components'
import { DashboardPage } from './pages/Dashboard'
import { SourceEditPage, SourceNewPage, SourcePage, SourcesPage } from './pages/Sources'
import { DiskPage, DisksPage } from './pages/Disks'
import { RunPage, RunsPage } from './pages/Runs'
import { RepositoriesPage, RepositoryPage, RepositorySetupPage } from './pages/Repositories'
import { DebugPage } from './pages/Debug'

function AppShell() {
  const system = useSystem()
  const location = useLocation()
  useEffect(() => { document.getElementById('main')?.focus(); window.scrollTo(0, 0) }, [location.pathname])
  return <div className="app-shell"><a href="#main" className="skip-link">Skip to content</a>
    <aside className="sidebar"><Link className="brand" to="/"><span className="brand-mark" aria-hidden="true">R</span><span>ROTUP<small>Local backup control</small></span></Link>
      <nav aria-label="Main navigation">{[['/', 'Overview'], ['/sources', 'Sources'], ['/disks', 'Disks'], ['/repositories', 'Backup storage'], ['/runs', 'History'], ...(system.devMode ? [['/debug', 'Diagnostics']] : [])].map(([to, name]) => <NavLink key={to} to={to} end={to === '/'}>{name}</NavLink>)}</nav>
      <div className="sidebar-footer"><span className="dot" /> Single host · {system.version}<p>{system.devMode ? 'Development diagnostics on' : 'Local administration'}</p></div>
    </aside><div className="workspace"><div className="topbar"><span>Local staging → Rotated disks</span><span>{system.devMode ? 'DEVELOPMENT' : 'ROTUP'}</span></div>
      <main id="main" tabIndex={-1}>{system.devMode && <Alert tone="info">Development mode · extra diagnostics visible. All backup safety checks remain active.</Alert>}<Outlet /></main>
      <footer>ROTUP · Manual backup operations. No scheduler is enabled.</footer>
    </div></div>
}
export default function App() {
  const [system, setSystem] = useState<System>()
  const [error, setError] = useState<unknown>()
  const [retry, setRetry] = useState(0)
  useEffect(() => { let active = true; setError(undefined); void loadSystem().then(v => { if (active) setSystem(v) }).catch(e => { if (active) setError(e) }); return () => { active = false } }, [retry])
  if (error) return <main><PageBoot /><ErrorAlert error={error} /><button onClick={() => setRetry(v => v + 1)}>Try again</button></main>
  if (!system) return <main><PageBoot /><LoadingState /></main>
  return <SystemContext.Provider value={system}><Routes><Route element={<AppShell />}><Route index element={<DashboardPage />} />
    <Route path="sources" element={<SourcesPage />} /><Route path="sources/new" element={<SourceNewPage />} /><Route path="sources/:id" element={<SourcePage />} /><Route path="sources/:id/edit" element={<SourceEditPage />} />
    <Route path="disks" element={<DisksPage />} /><Route path="disks/:id" element={<DiskPage />} />
    <Route path="repositories" element={<RepositoriesPage />} /><Route path="repositories/new" element={<RepositorySetupPage />} /><Route path="repositories/:id" element={<RepositoryPage />} />
    <Route path="runs" element={<RunsPage />} /><Route path="runs/:id" element={<RunPage />} /><Route path="debug" element={<DebugPage />} />
    <Route path="*" element={<EmptyState title="Page not found"><Link to="/">Return to overview</Link></EmptyState>} />
  </Route></Routes></SystemContext.Provider>
}
function PageBoot() { return <h1>ROTUP</h1> }
