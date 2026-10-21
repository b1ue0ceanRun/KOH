'use client'

import { useCallback, useEffect, useRef, useState } from 'react'
import { adminFetch, resolveRuntimeAPI } from '../../lib/api'

// CYBER-KING arenas: an arena is one match whose hills are scored every tick
// by reading each hill's KingFile for a player_flag. Agents talk to the
// /koh/v1 API with the per-player token listed at the bottom of this page.

type Flag = { id: number; flag: string; score: string; path: string; enabled: boolean; solves: number }
type Hill = {
  hill_code: string; os: string; base_score: string; healthcheck_cmd: string
  current_king: string | null; streak: number
  machine_id: string | null; machine_image: string | null; machine_ip: string | null; machine_status: string | null
  flags: Flag[]
}
type Arena = {
  arena_code: string; description: string; entrypoints: string[]; status: string
  tick_seconds: number; next_tick_at: string | null; started_at: string | null; ended_at: string | null
  hills: Hill[]
}
type LiveTarget = { id: string; arena_ip: string; image_name: string; status: string }
type AgentCreds = { player_name: string; api_token: string; player_flag: string; status: string }

const STATUS_BADGE: Record<string, string> = {
  pending: 'badge-dim', running: 'badge-green', settling: 'badge-amber', finished: 'badge-purple',
}

const post = (path: string, body?: unknown, method = 'POST') =>
  adminFetch(path, { method, body: body === undefined ? undefined : JSON.stringify(body) })

function NewArenaForm({ onDone }: { onDone: (msg: string, ok?: boolean) => void }) {
  const [code, setCode] = useState('')
  const [desc, setDesc] = useState('')
  const [eps, setEps] = useState('')
  const [tick, setTick] = useState('300')
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      await post('/admin/koh/arenas', {
        arena_code: code.trim(), description: desc,
        entrypoints: eps.split(',').map(s => s.trim()).filter(Boolean),
        tick_seconds: Number(tick) || 300,
      })
      setCode(''); setDesc(''); setEps('')
      onDone(`Arena ${code} created (pending)`)
    } catch (ex: any) { onDone(ex.message, false) }
  }
  return (
    <form onSubmit={submit} className="section-body" style={{ display: 'grid', gridTemplateColumns: '1fr 2fr 2fr 110px auto', gap: 10, alignItems: 'end' }}>
      <div><label className="form-label">Arena code</label><input className="form-input sm" value={code} onChange={e => setCode(e.target.value)} placeholder="arena-live" required /></div>
      <div><label className="form-label">Description</label><input className="form-input sm" value={desc} onChange={e => setDesc(e.target.value)} placeholder="夜间友谊赛" /></div>
      <div><label className="form-label">Entrypoints (comma-separated host:port)</label><input className="form-input sm" value={eps} onChange={e => setEps(e.target.value)} placeholder="10.40.0.11:443, 10.40.0.12:8080" /></div>
      <div><label className="form-label">Tick (s)</label><input className="form-input sm" type="number" min={10} value={tick} onChange={e => setTick(e.target.value)} /></div>
      <button className="btn btn-primary btn-sm" type="submit">Create</button>
    </form>
  )
}

function NewHillForm({ arena, targets, onDone }: { arena: string; targets: LiveTarget[]; onDone: (msg: string, ok?: boolean) => void }) {
  const [code, setCode] = useState('')
  const [machine, setMachine] = useState('')
  const [os, setOs] = useState('linux')
  const [base, setBase] = useState('1.0000')
  const [health, setHealth] = useState('')
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      await post(`/admin/koh/arenas/${arena}/hills`, {
        hill_code: code.trim(), machine_id: machine || undefined, os, base_score: base, healthcheck_cmd: health,
      })
      setCode('')
      onDone(`Hill ${code} added`)
    } catch (ex: any) { onDone(ex.message, false) }
  }
  return (
    <form onSubmit={submit} style={{ display: 'grid', gridTemplateColumns: '1.2fr 2fr 100px 100px 2fr auto', gap: 8, alignItems: 'end', marginTop: 10 }}>
      <div><label className="form-label">Hill code</label><input className="form-input sm" value={code} onChange={e => setCode(e.target.value)} placeholder={`${arena}-01`} required /></div>
      <div>
        <label className="form-label">Bound target</label>
        <select className="form-input sm" value={machine} onChange={e => setMachine(e.target.value)}>
          <option value="">(none yet)</option>
          {targets.map(t => <option key={t.id} value={t.id}>{t.image_name} · {t.arena_ip} · {t.status}</option>)}
        </select>
      </div>
      <div><label className="form-label">OS</label>
        <select className="form-input sm" value={os} onChange={e => setOs(e.target.value)}><option>linux</option><option>windows</option></select>
      </div>
      <div><label className="form-label">Base score</label><input className="form-input sm" value={base} onChange={e => setBase(e.target.value)} /></div>
      <div><label className="form-label">Health check cmd (optional)</label><input className="form-input sm" value={health} onChange={e => setHealth(e.target.value)} placeholder="curl -sf http://127.0.0.1/ >/dev/null" /></div>
      <button className="btn btn-sm" type="submit">Add hill</button>
    </form>
  )
}

function FlagsCell({ hill, onDone }: { hill: Hill; onDone: (msg: string, ok?: boolean) => void }) {
  const [open, setOpen] = useState(false)
  const [value, setValue] = useState('')
  const [score, setScore] = useState('1.0000')
  const [path, setPath] = useState('')
  const [plant, setPlant] = useState(true)
  const add = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      const r = await post(`/admin/koh/hills/${hill.hill_code}/flags`, { flag: value, score, path, plant: plant && !!path })
      setValue('')
      onDone(`Flag added to ${hill.hill_code}: ${r.flag}`)
    } catch (ex: any) { onDone(ex.message, false) }
  }
  return (
    <div>
      <button className="btn btn-xs" onClick={() => setOpen(!open)}>{hill.flags.length} flag{hill.flags.length === 1 ? '' : 's'} {open ? '▴' : '▾'}</button>
      {open && (
        <div style={{ marginTop: 8, display: 'flex', flexDirection: 'column', gap: 6 }}>
          {hill.flags.map(f => (
            <div key={f.id} style={{ display: 'flex', gap: 6, alignItems: 'center', fontSize: '0.75rem' }}>
              <code style={{ opacity: f.enabled ? 1 : 0.4 }}>{f.flag}</code>
              <span className="badge badge-cyan">{f.score}</span>
              <span style={{ color: 'var(--txt-dim)' }}>{f.solves} solve{f.solves === 1 ? '' : 's'}{f.path ? ` · ${f.path}` : ''}</span>
              <button className="btn btn-xs" onClick={() => post(`/admin/koh/flags/${f.id}`, { enabled: !f.enabled }, 'PUT')
                .then(() => onDone(f.enabled ? 'Flag disabled' : 'Flag enabled')).catch(ex => onDone(ex.message, false))}>
                {f.enabled ? 'Disable' : 'Enable'}
              </button>
              <button className="btn btn-xs btn-danger" onClick={() => confirm('Delete this flag?') &&
                post(`/admin/koh/flags/${f.id}`, undefined, 'DELETE').then(() => onDone('Flag deleted')).catch(ex => onDone(ex.message, false))}>
                ✕
              </button>
            </div>
          ))}
          <form onSubmit={add} style={{ display: 'flex', gap: 6, flexWrap: 'wrap', alignItems: 'center' }}>
            <input className="form-input sm" style={{ width: 170 }} value={value} onChange={e => setValue(e.target.value)} placeholder="flag value (blank = random)" />
            <input className="form-input sm" style={{ width: 80 }} value={score} onChange={e => setScore(e.target.value)} title="fixed score" />
            <input className="form-input sm" style={{ width: 170 }} value={path} onChange={e => setPath(e.target.value)} placeholder="/var/www/flag.txt" />
            <label style={{ fontSize: '0.72rem', color: 'var(--txt-dim)' }}>
              <input type="checkbox" checked={plant} onChange={e => setPlant(e.target.checked)} /> plant in box
            </label>
            <button className="btn btn-xs btn-cyan" type="submit">Add flag</button>
          </form>
        </div>
      )}
    </div>
  )
}

function ArenaCard({ a, targets, onDone }: { a: Arena; targets: LiveTarget[]; onDone: (msg: string, ok?: boolean) => void }) {
  const run = (label: string, fn: () => Promise<unknown>, ask?: string) => {
    if (ask && !confirm(ask)) return
    fn().then(() => onDone(label)).catch(ex => onDone(ex.message, false))
  }
  return (
    <div className="section">
      <div className="section-head">
        <span className="section-title">
          {a.arena_code} <span className={`badge ${STATUS_BADGE[a.status] || 'badge-dim'}`}>{a.status.toUpperCase()}</span>
        </span>
        <div className="action-row">
          {a.status === 'pending' && (
            <button className="btn btn-xs btn-primary" onClick={() => run(`${a.arena_code} started`,
              () => post(`/admin/koh/arenas/${a.arena_code}/start`),
              `Start ${a.arena_code}? Hills reset to unowned; the first tick lands in ${a.tick_seconds}s.`)}>Start</button>
          )}
          {a.status === 'running' && (
            <button className="btn btn-xs btn-danger" onClick={() => run(`${a.arena_code} finishing`,
              () => post(`/admin/koh/arenas/${a.arena_code}/finish`),
              `Finish ${a.arena_code}? No more ticks or flag submissions; final standings are kept.`)}>Finish</button>
          )}
          {(a.status === 'pending' || a.status === 'finished') && (
            <button className="btn btn-xs btn-danger" onClick={() => run(`${a.arena_code} deleted`,
              () => post(`/admin/koh/arenas/${a.arena_code}`, undefined, 'DELETE'),
              `Delete ${a.arena_code} and ALL its hills, history and scores?`)}>Delete</button>
          )}
        </div>
      </div>
      <div className="section-body">
        <div className="section-sub">
          {a.description || 'No description'} · tick {a.tick_seconds}s · entrypoints {a.entrypoints.join(', ') || '-'}
          {a.next_tick_at && <> · next tick {new Date(a.next_tick_at).toLocaleTimeString()}</>}
        </div>
        <div className="table-scroll" style={{ marginTop: 10 }}>
          <table className="ck-table">
            <thead><tr><th>Hill</th><th>Target</th><th>OS</th><th>Base</th><th>King</th><th>Streak</th><th>Flags</th></tr></thead>
            <tbody>
              {a.hills.map(h => (
                <tr key={h.hill_code}>
                  <td><code>{h.hill_code}</code></td>
                  <td style={{ fontSize: '0.78rem' }}>
                    {h.machine_id
                      ? <>{h.machine_image} <code>{h.machine_ip}</code> <span className={`badge ${h.machine_status === 'active' ? 'badge-green' : 'badge-red'}`}>{h.machine_status}</span></>
                      : <span className="badge badge-red">unbound</span>}
                  </td>
                  <td>{h.os}</td>
                  <td>{h.base_score}</td>
                  <td>{h.current_king ? <span style={{ color: 'var(--txt-bright)', fontWeight: 600 }}>{h.current_king}</span> : <span style={{ color: 'var(--txt-dim)' }}>unowned</span>}</td>
                  <td>{h.streak || '-'}</td>
                  <td><FlagsCell hill={h} onDone={onDone} /></td>
                </tr>
              ))}
              {a.hills.length === 0 && <tr><td colSpan={7} style={{ textAlign: 'center', color: 'var(--txt-dim)', padding: 16 }}>No hills yet. Spin a target, then bind it below.</td></tr>}
            </tbody>
          </table>
        </div>
        {a.status !== 'finished' && <NewHillForm arena={a.arena_code} targets={targets} onDone={onDone} />}
      </div>
    </div>
  )
}

function AgentCredentials({ onDone }: { onDone: (msg: string, ok?: boolean) => void }) {
  const [list, setList] = useState<AgentCreds[]>([])
  const [shown, setShown] = useState<string | null>(null)
  const [baseURL, setBaseURL] = useState('<api-url>/koh/v1')
  const load = useCallback(() => { adminFetch('/admin/koh/players').then(setList).catch(() => {}) }, [])
  useEffect(() => { load() }, [load])
  // window-derived, so only after mount (keeps the prerendered HTML stable)
  useEffect(() => { setBaseURL(`${resolveRuntimeAPI()}/koh/v1`) }, [])
  const rotate = (p: string, what: { token?: boolean; flag?: boolean }, ask: string) => {
    if (!confirm(ask)) return
    post(`/admin/koh/players/${p}/rotate`, what).then(() => { onDone(`Rotated credentials for ${p}`); load() }).catch(ex => onDone(ex.message, false))
  }
  return (
    <div className="section">
      <div className="section-head">
        <span className="section-title">AGENT CREDENTIALS</span>
        <span className="section-sub">Inject into each player&apos;s agent container: <code>KOH_BASE_URL={baseURL}</code> and its <code>KOH_API_TOKEN</code></span>
      </div>
      <div className="table-scroll">
        <table className="ck-table">
          <thead><tr><th>Player</th><th>Status</th><th>KOH_API_TOKEN</th><th>player_flag</th><th>Actions</th></tr></thead>
          <tbody>
            {list.map(p => (
              <tr key={p.player_name}>
                <td style={{ fontWeight: 600 }}>{p.player_name}</td>
                <td><span className={`badge ${p.status === 'active' ? 'badge-green' : 'badge-red'}`}>{p.status}</span></td>
                <td>
                  <code style={{ fontSize: '0.72rem' }}>{shown === p.player_name ? p.api_token : `${p.api_token.slice(0, 8)}…`}</code>{' '}
                  <button className="btn btn-xs" onClick={() => setShown(shown === p.player_name ? null : p.player_name)}>{shown === p.player_name ? 'Hide' : 'Show'}</button>{' '}
                  <button className="btn btn-xs" onClick={() => navigator.clipboard?.writeText(p.api_token).then(() => onDone(`Token for ${p.player_name} copied`))}>Copy</button>
                </td>
                <td><code style={{ fontSize: '0.72rem' }}>{p.player_flag}</code></td>
                <td>
                  <div className="action-row">
                    <button className="btn btn-xs" onClick={() => rotate(p.player_name, { token: true }, `Issue a new API token for ${p.player_name}? Their agent must be given the new one.`)}>New token</button>
                    <button className="btn btn-xs btn-danger" onClick={() => rotate(p.player_name, { flag: true }, `Issue a new player_flag for ${p.player_name}? Hills held with the old flag are lost at the next tick.`)}>New flag</button>
                  </div>
                </td>
              </tr>
            ))}
            {list.length === 0 && <tr><td colSpan={5} style={{ textAlign: 'center', color: 'var(--txt-dim)', padding: 16 }}>No players yet.</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  )
}

export default function ArenasPage() {
  const [arenas, setArenas] = useState<Arena[]>([])
  const [targets, setTargets] = useState<LiveTarget[]>([])
  const [flash, setFlash] = useState<{ msg: string; ok: boolean } | null>(null)
  const timer = useRef<ReturnType<typeof setTimeout>>()

  const load = useCallback(() => {
    adminFetch('/admin/koh/arenas').then(setArenas).catch(() => {})
    adminFetch('/admin/hills').then(d => setTargets(d ?? [])).catch(() => {})
  }, [])
  useEffect(() => {
    load()
    const id = setInterval(load, 15000)
    return () => clearInterval(id)
  }, [load])

  const notify = (msg: string, ok = true) => {
    setFlash({ msg, ok })
    clearTimeout(timer.current)
    timer.current = setTimeout(() => setFlash(null), 5000)
    load()
  }

  const running = arenas.filter(a => a.status === 'running').length
  return (
    <div>
      <div className="page-header">
        <div className="page-title">CYBER-KING ARENAS</div>
        <div className="page-sub">{arenas.length} arenas · {running} running · KingFile /root/king.txt (Linux) or C:\king.txt (Windows)</div>
      </div>
      {flash && <div className={`flash ${flash.ok ? 'flash-ok' : 'flash-err'}`}>{flash.msg}</div>}
      <div className="section">
        <div className="section-head"><span className="section-title">NEW ARENA</span></div>
        <NewArenaForm onDone={notify} />
      </div>
      {arenas.map(a => <ArenaCard key={a.arena_code} a={a} targets={targets} onDone={notify} />)}
      <AgentCredentials onDone={notify} />
    </div>
  )
}
