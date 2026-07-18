import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { apiFetch, apiJSON, del, postJSON, putJSON } from './api.js'
import { useUI } from './ui.jsx'
import { ProxyLogo } from './brand.jsx'
import UpdateBanner from './UpdateBanner.jsx'
import UpdateCheckButton from './UpdateCheckButton.jsx'
import PickerModal from './PickerModal.jsx'
import WebRoutes from './WebRoutes.jsx'

// === Helpers ===============================================================

// Scroll the Apply card (and the live step log inside it) into view. Deferred a
// frame so it measures the card AFTER React has painted whatever just changed
// its height — the log appears mid-apply and grows the card underneath us.
function showApplyLogEl() {
  requestAnimationFrame(() => {
    document.querySelector('#applyCard')?.scrollIntoView({ behavior: 'smooth', block: 'end' })
  })
}

function parsePorts(s) {
  return s.split(',').map(x => x.trim()).filter(Boolean).map(tok => {
    const [p, pr] = tok.split(':')
    return { port: +p, proto: (pr || 'both').trim() }
  })
}
function fmtBytes(n) {
  n = +n || 0
  const u = ['B','KB','MB','GB','TB']; let i = 0
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++ }
  const v = i ? parseFloat(n.toFixed(2)) : Math.round(n)
  return v + ' ' + u[i]
}

// Compact pile of game presets — keep parity with the existing dropdown.
const PRESETS = [
  { group: 'Survival / Crafting', items: [
    { label: 'Satisfactory (7777 + 8888 tcp+udp)', name: 'satisfactory', ports: '7777:both, 8888:both' },
    { label: 'Factorio (34197 udp)', name: 'factorio', ports: '34197:udp' },
    { label: 'Valheim (2456-2458 udp)', name: 'valheim', ports: '2456:udp, 2457:udp, 2458:udp' },
    { label: 'Rust (28015 udp + 28016 tcp rcon)', name: 'rust', ports: '28015:udp, 28016:tcp' },
    { label: 'ARK: Survival Evolved (7777 + 27015 udp)', name: 'ark-evolved', ports: '7777:udp, 27015:udp' },
    { label: 'ARK: Survival Ascended (7777 udp)', name: 'ark-ascended', ports: '7777:udp' },
    { label: 'Palworld (8211 udp)', name: 'palworld', ports: '8211:udp' },
    { label: '7 Days to Die (26900-26902)', name: '7-days-to-die', ports: '26900:both, 26901:udp, 26902:udp' },
    { label: 'Project Zomboid (16261-16262 udp)', name: 'project-zomboid', ports: '16261:udp, 16262:udp' },
    { label: "Don't Starve Together (10999 udp)", name: 'dont-starve', ports: '10999:udp' },
    { label: 'Enshrouded (15636-15637 udp)', name: 'enshrouded', ports: '15636:udp, 15637:udp' },
    { label: 'V Rising (9876-9877 udp)', name: 'v-rising', ports: '9876:udp, 9877:udp' },
  ]},
  { group: 'Sandbox / Building', items: [
    { label: 'Minecraft: Java (25565 tcp)', name: 'minecraft', ports: '25565:tcp' },
    { label: 'Minecraft: Bedrock (19132 udp)', name: 'minecraft-bedrock', ports: '19132:udp' },
    { label: 'Minecraft: BlueMap web map (8100 tcp)', name: 'minecraft-bluemap', ports: '8100:tcp' },
    { label: 'Terraria (7777 tcp+udp)', name: 'terraria', ports: '7777:both' },
    { label: "Garry's Mod (27015 tcp+udp)", name: 'garrys-mod', ports: '27015:both' },
  ]},
  { group: 'Shooters / Source', items: [
    { label: 'Counter-Strike 2 (27015 + 27020 SourceTV)', name: 'cs2', ports: '27015:both, 27020:udp' },
    { label: 'Team Fortress 2 (27015 tcp+udp)', name: 'tf2', ports: '27015:both' },
    { label: 'Left 4 Dead 2 (27015 tcp+udp)', name: 'left4dead2', ports: '27015:both' },
    { label: 'Unturned (27015-27017 udp)', name: 'unturned', ports: '27015:udp, 27016:udp, 27017:udp' },
    // SPT + Fika (Tarkov co-op): 6969/tcp = SPT backend + Fika WebSocket (lobby),
    // 22100/udp = Fika relay/NAT-punch for in-raid P2P. Both needed or remote
    // players reach the lobby but can't run raids together.
    { label: 'SPT + Fika (Tarkov co-op) (6969 tcp + 22100 udp)', name: 'spt-fika', ports: '6969:tcp, 22100:udp' },
  ]},
]

// === Component =============================================================
export default function Admin({ onSignOut, onOpenSetup }) {
  const { ask, toast } = useUI()
  const [data, setData] = useState({ entries: [], setup: null, pending: false, diff: {}, mode: '' })
  const [domains, setDomains] = useState([])
  const [autoDomains, setAutoDomains] = useState(new Set())
  const [cfOk, setCfOk] = useState(false)
  const [cfRecords, setCfRecords] = useState([])
  const [cfMsg, setCfMsg] = useState('Auto-composed from the game + domain (editable). Apply creates/updates the grey-cloud A record in Cloudflare for you — no extra step.')
  const [stats, setStats] = useState({})       // id -> {connections, online, rxBytes, txBytes, rate{rRx,rTx}}
  const statsPrev = useRef({})
  const [pickerOpen, setPickerOpen] = useState(false)
  const [tab, setTab] = useState('games') // 'games' | 'web'

  // Apply job state (polled every 2s while running, idle otherwise).
  const [applyJob, setApplyJob] = useState(null)
  const applyInt = useRef(null)
  const applyWasRunning = useRef(false)
  // Set when the operator clicks Apply; consumed by pollApply once the log has
  // actually rendered. Scoped to a click so a job already running at page load
  // doesn't yank the view somewhere they didn't ask to go.
  const wantApplyScroll = useRef(false)

  // Add-entry form state (controlled inputs).
  const [form, setForm] = useState({ name: '', subdomain: '', ports: '', targetIP: '', service: '', enabled: true, domain: '' })
  const [formErr, setFormErr] = useState('')

  // Domains card local input.
  const [domainInput, setDomainInput] = useState('')
  const [domainErr, setDomainErr] = useState('')

  // === Data loaders =========================================================
  const loadEntries = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/entries')
    if (ok && data) setData(data)
  }, [])

  const loadDomains = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/domains')
    if (ok && data) {
      setDomains(data.domains || [])
      setAutoDomains(new Set(data.auto || []))
    }
  }, [])

  const cfStatus = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/dns/records')
    setCfOk(ok && !!data?.configured)
  }, [])

  const cfLoadRecords = useCallback(async (dom) => {
    if (!cfOk || !dom) { setCfRecords([]); return }
    const { data } = await apiJSON('/api/dns/records?domain=' + encodeURIComponent(dom))
    if (data?.error) { setCfMsg(data.error); setCfRecords([]); return }
    const recs = data?.records || []
    setCfRecords(recs)
    setCfMsg(`${recs.length} existing A record(s) in ${dom}. Apply will create/update this one automatically.`)
  }, [cfOk])

  const pollStats = useCallback(async () => {
    const { ok, data: d } = await apiJSON('/api/stats')
    if (!ok) return
    const now = Date.now()
    const next = {}
    for (const s of (d.stats || [])) {
      const prev = statsPrev.current[s.id]
      let rRx = null, rTx = null
      if (prev) {
        const ds = Math.max((now - prev.t) / 1000, 1)
        rRx = Math.max(0, (s.rxBytes - prev.rx) / ds)
        rTx = Math.max(0, (s.txBytes - prev.tx) / ds)
      }
      next[s.id] = { ...s, rRx, rTx }
      statsPrev.current[s.id] = { rx: s.rxBytes, tx: s.txBytes, t: now }
    }
    setStats(next)
  }, [])

  const pollApply = useCallback(async () => {
    const { ok, data: j } = await apiJSON('/api/apply/status')
    if (!ok) return
    setApplyJob(j)
    if (j?.running) {
      if (!applyInt.current) applyInt.current = setInterval(pollApply, 2000)
    } else if (applyInt.current) {
      clearInterval(applyInt.current); applyInt.current = null
    }
    // The log renders as soon as the job reports running, so this is the first
    // moment there's something to scroll to. Fires once per click.
    if (j?.running && wantApplyScroll.current) {
      wantApplyScroll.current = false
      showApplyLogEl()
    }
    if (applyWasRunning.current && !j?.running) loadEntries()
    applyWasRunning.current = !!j?.running
  }, [loadEntries])

  // === Initial boot =========================================================
  useEffect(() => {
    loadEntries(); loadDomains(); cfStatus(); pollStats(); pollApply()
    const t = setInterval(pollStats, 5000)
    return () => { clearInterval(t); if (applyInt.current) clearInterval(applyInt.current) }
  }, [loadEntries, loadDomains, cfStatus, pollStats, pollApply])

  // Recompose <name>.<domain> when either changes.
  function setFormPair(patch) {
    setForm(prev => {
      const next = { ...prev, ...patch }
      if (next.domain) {
        const safe = (next.name.trim() || 'app').toLowerCase().replace(/[^a-z0-9-]/g, '-')
        next.subdomain = `${safe}.${next.domain}`
      }
      return next
    })
  }

  function applyPreset(value) {
    if (!value) return
    let chosen = null
    for (const g of PRESETS) for (const it of g.items) if (it.ports === value) { chosen = it; break }
    if (!chosen) return
    setForm(prev => {
      const out = { ...prev, ports: chosen.ports }
      if (!prev.name.trim()) out.name = chosen.name
      // If exactly one domain exists, auto-pick it.
      if (!prev.subdomain.trim() && domains.length === 1 && !prev.domain) {
        out.domain = domains[0]
        const safe = chosen.name.toLowerCase().replace(/[^a-z0-9-]/g, '-')
        out.subdomain = `${safe}.${domains[0]}`
      }
      return out
    })
  }

  // === Action handlers ======================================================
  async function toggleEntry(id) {
    await apiFetch(`/api/entries/${id}/toggle`, { method: 'POST' })
    loadEntries()
  }
  async function rebindEntry(id, name) {
    if (!await ask(`Rebind <strong>${escapeHtml(name)}</strong> to its Service's current ClusterIP? Updates the entry's target — then <strong>Apply</strong> rebuilds just this game's gateway (others untouched).`, { ok: 'Rebind' })) return
    const { data: res } = await apiJSON(`/api/entries/${id}/rebind`, { method: 'POST' })
    if (res?.error) toast('✗ ' + res.error, true)
    else toast((res?.rebound ? '✓ ' : '· ') + (res?.message || 'rebound'))
    loadEntries()
  }
  async function deleteEntry(id, name, subdomain) {
    if (!await ask(`Delete entry <strong>${escapeHtml(name)}</strong>? It stays live until you Apply.`, { ok: 'Delete entry' })) return
    let cf = false
    if (cfOk && subdomain && subdomain.includes('.')) {
      cf = await ask(`Also remove the DNS record <strong>${escapeHtml(subdomain)}</strong> from Cloudflare?`,
        { ok: 'Yes, remove from Cloudflare', cancel: 'Keep DNS record' })
    }
    const { data: res } = await apiJSON(`/api/entries/${id}?cf=${cf ? 1 : 0}`, { method: 'DELETE' })
    if (res?.dns) toast(res.dns)
    loadEntries()
  }
  async function addDomain() {
    setDomainErr('')
    const { ok, data } = await postJSON('/api/domains', { domain: domainInput })
    if (!ok) { setDomainErr(data?.error || 'failed'); return }
    setDomainInput('')
    loadDomains()
  }
  async function removeDomain(d) {
    if (!await ask(`Remove domain <strong>${escapeHtml(d)}</strong> from the list? (Does not touch DNS.)`, { ok: 'Remove' })) return
    await del('/api/domains/' + encodeURIComponent(d))
    loadDomains()
  }
  async function submitEntry(ev) {
    ev.preventDefault()
    setFormErr('')
    const body = {
      name: form.name, subdomain: form.subdomain,
      ports: parsePorts(form.ports),
      targetIP: form.targetIP, service: form.service,
      enabled: form.enabled,
    }
    const { ok, data } = await postJSON('/api/entries', body)
    if (!ok) { setFormErr(data?.error || 'failed'); return }
    setForm({ name: '', subdomain: '', ports: '', targetIP: '', service: '', enabled: true, domain: '' })
    loadEntries()
  }
  // One click: Apply starts immediately and the view follows to the log.
  // No confirm step — Apply is the app's whole point, it's idempotent
  // (reconciles to the entry set rather than doing anything one-way), and the
  // button is disabled while a job runs, so a stray double-click is a no-op.
  async function applyNow() {
    const r = await apiFetch('/api/apply', { method: 'POST' })
    if (r.status === 409) { toast('An apply is already in progress.', true); return }
    applyWasRunning.current = true
    // Move now so the click visibly does something — from the sticky bar the
    // Apply card is off-screen, and the first step can take seconds to report.
    showApplyLogEl()
    // ...then again once the log actually exists. It only renders when the
    // first poll reports the job running, so this scroll alone would land on
    // the card at its pre-log height and leave the steps below the fold.
    wantApplyScroll.current = true
    pollApply()
  }

  // From PickerModal: it calls back with the picked Service.
  function applyPick(svc) {
    if (!svc.clusterIP || svc.clusterIP === 'None') return
    const portMap = {}
    ;(svc.ports || []).forEach(p => {
      const pr = (p.protocol || 'TCP').toLowerCase()
      portMap[p.port] = portMap[p.port] ? 'both' : pr
    })
    const portStr = Object.keys(portMap).map(k => `${k}:${portMap[k]}`).join(', ')
    setForm(prev => ({
      ...prev,
      targetIP: svc.clusterIP,
      service: `${svc.name}.${svc.namespace}`,
      ports: portStr || prev.ports,
    }))
    setPickerOpen(false)
  }

  // === Derived view =========================================================
  const rows = useMemo(() => {
    const arr = (data.entries || []).slice()
    arr.sort((a, b) => (a.entry.name > b.entry.name ? 1 : -1))
    return arr
  }, [data.entries])
  const removed = data?.diff?.removed || []
  const entryCount = rows.length
  const enabledCount = rows.filter(r => r.entry.enabled).length

  function changesSummary() {
    const d = data.diff || {}
    if (!d.detail) return 'Unapplied changes — entries differ from what\'s live. Apply pushes them.'
    const nNew = rows.filter(r => r.change?.status === 'new').length
    const nChg = rows.filter(r => r.change?.status === 'changed').length
    const nRm  = (d.removed || []).length
    const segs = []
    if (nNew) segs.push(`<span class="seg"><b>${nNew}</b> new</span>`)
    if (nChg) segs.push(`<span class="seg"><b>${nChg}</b> changed</span>`)
    if (nRm)  segs.push(`<span class="seg rm"><b>${nRm}</b> to remove</span>`)
    return segs.length
      ? `Unapplied changes: ${segs.join(' · ')}. These are <b>not live yet</b> — Apply pushes them.`
      : 'Unapplied changes — entries differ from what\'s live. Apply pushes them.'
  }

  return (
    <>
      <header className="app">
        <div className="bar">
          <div className="logo"><ProxyLogo /><span>ProxyCTL</span></div>
          <span className="crumb">front end for WireGuard ↔ Kubernetes proxying + Cloudflare DNS</span>
          <span className={'hdrtag' + (data.pending ? ' on' : '')} style={{ marginLeft: 'auto', cursor: 'pointer' }}
                onClick={() => document.querySelector('#applyCard')?.scrollIntoView({ behavior: 'smooth', block: 'center' })}>● unapplied changes</span>
          <UpdateCheckButton />
          <button className="sm" style={{ marginLeft: 10 }} onClick={onOpenSetup}>Setup</button>
          <button className="sm" style={{ marginLeft: 6 }} title="Sign out" onClick={onSignOut}>Sign out</button>
          {/* Only render when known — an empty .badge still paints a hollow
              bordered pill in the header. */}
          {data.mode && <span className="badge" style={{ marginLeft: 10 }}>{'apply-mode: ' + data.mode}</span>}
        </div>
      </header>

      <UpdateBanner />

      {/* Sticky action bar — shown only while there are unapplied changes. */}
      <div id="actionbar" className={'actionbar' + (data.pending ? ' on' : '')}>
        <span id="abSum" dangerouslySetInnerHTML={{ __html: changesSummary() }} />
        <button className="sm" onClick={() => document.querySelector('h2.section')?.scrollIntoView({ behavior: 'smooth', block: 'start' })}>Review</button>
        <button className="primary" onClick={applyNow}>Apply now</button>
      </div>

      <main>
        <h2 className="section">Tunnels{' '}
          {tab === 'games' && <span className="badge" style={{ marginLeft: 10 }}>{entryCount ? `${entryCount} total · ${enabledCount} enabled` : 'none yet'}</span>}
        </h2>

        {/* Tab split: raw L4 game ports vs L7 host-routed web apps. */}
        <div className="row" style={{ gap: 8, margin: '0 0 18px' }}>
          {[['games', 'Games'], ['web', 'Web Apps']].map(([k, lab]) => (
            <button key={k}
              className={tab === k ? 'primary' : 'sm'}
              onClick={() => setTab(k)}>{lab}</button>
          ))}
        </div>

        {/* First-run nudge strip. */}
        {data.setup && !data.setup.allDone && (
          <div className="nudge on">
            {(data.setup.steps || []).map((s, i) => (
              <div key={s.key} className={'step ' + (s.state || '')}
                   onClick={() => { if (s.key === 'droplet' || s.key === 'cloudflare') onOpenSetup() }}>
                <span className="n">{s.state === 'done' ? '' : String(i + 1)}</span>
                <span className="lbl">{s.title}{s.optional && <span className="opt">optional</span>}</span>
              </div>
            ))}
          </div>
        )}

        {tab === 'games' && (<>
        {/* Entries table */}
        <div className="card">
          <table>
            <thead>
              <tr>
                <th>Status</th>
                <th>Name / DNS name</th>
                <th>Public ports</th>
                <th>→ Target ClusterIP</th>
                <th className="num">Conns</th>
                <th className="num">
                  <div><span style={{ color: 'var(--link)' }}>↓in</span> / <span style={{ color: 'var(--ok-tx)' }}>↑out</span></div>
                  <div style={{ color: 'var(--muted)', fontWeight: 400, textTransform: 'none', letterSpacing: 0, fontSize: 10 }}>rate · total</div>
                </th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 && removed.length === 0 && (
                <tr><td colSpan="7" style={{ color: 'var(--muted)' }}>No entries yet — add one below.</td></tr>
              )}
              {rows.map(r => <EntryRow key={r.entry.id} r={r} stats={stats[r.entry.id]} onToggle={toggleEntry} onRebind={rebindEntry} onDelete={deleteEntry} />)}
              {removed.map(rm => (
                <tr key={'rm-' + (rm.id || rm.name)} className="ghost">
                  <td><span className="stat dis"><span className="d" /><span className="t">removing</span></span></td>
                  <td><strong>{rm.name}</strong><br /><span className="mono" style={{ color: 'var(--muted)' }}>{rm.subdomain || ''}</span><br /><span className="chg rm">● will be removed on Apply</span></td>
                  <td colSpan="5" className="mono" style={{ color: 'var(--muted)' }}>deleted locally — Apply tears down its gateway, droplet peer & DNS record</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        {/* Add entry */}
        <div className="card">
          <h2>Add tunnel entry</h2>
          <form className="wiz" onSubmit={submitEntry}>
            <div className="step">
              <p className="st">1 · Game</p>
              <div className="pair">
                <label>Game template
                  <select onChange={e => applyPreset(e.target.value)} defaultValue="">
                    <option value="">— custom —</option>
                    {PRESETS.map(g => (
                      <optgroup key={g.group} label={g.group}>
                        {g.items.map(it => <option key={it.name} value={it.ports}>{it.label}</option>)}
                      </optgroup>
                    ))}
                  </select>
                </label>
                <label>Name<input value={form.name} onChange={e => setFormPair({ name: e.target.value })} placeholder="satisfactory" required /></label>
              </div>
            </div>

            <div className="step">
              <p className="st">2 · External DNS — players connect to this</p>
              <div className="pair">
                <label>Domain
                  <select value={form.domain} onChange={e => { setFormPair({ domain: e.target.value }); cfLoadRecords(e.target.value) }}>
                    <option value="">— custom / type below —</option>
                    {domains.map(d => <option key={d} value={d}>{`<name>.${d}`}</option>)}
                  </select>
                </label>
                <label>Full DNS name
                  <input value={form.subdomain} onChange={e => setForm(p => ({ ...p, subdomain: e.target.value }))} list="cfRecs" placeholder="satisfactory.examplelabs.cc" autoComplete="off" />
                  <datalist id="cfRecs">{cfRecords.map(r => <option key={r.name} value={r.name} />)}</datalist>
                </label>
              </div>
              {/* cfMsg can carry a server-supplied error string — render it
                  as plain text, never as HTML. The not-configured notice is
                  a fixed literal, so it stays proper JSX. */}
              <p className="hint" style={{ margin: '10px 0 0' }}>
                {cfOk
                  ? cfMsg
                  : <><span className="bad">Cloudflare not configured</span> — set CF_API_TOKEN in ProxyCTL's env; until then create the grey-cloud A record by hand.</>}
              </p>
            </div>

            <div className="step">
              <p className="st">3 · Internal target — the Kubernetes pod</p>
              <div className="row" style={{ marginBottom: 12 }}>
                <button type="button" className="action sm" onClick={() => setPickerOpen(true)}>Pick from cluster…</button>
                <span className="hint" style={{ margin: 0 }}>Read-only browse of live Services — fills Target ClusterIP + Internal DNS. Or type them.</span>
              </div>
              <div className="pair">
                <label>Target ClusterIP<input value={form.targetIP} onChange={e => setForm(p => ({ ...p, targetIP: e.target.value }))} placeholder="10.43.101.26" required /></label>
                <label>Internal DNS (Kubernetes service)<input value={form.service} onChange={e => setForm(p => ({ ...p, service: e.target.value }))} placeholder="minecraft-service.gamectl" /></label>
              </div>
            </div>

            <div className="step">
              <p className="st">4 · Ports</p>
              <label>Public ports<input value={form.ports} onChange={e => setForm(p => ({ ...p, ports: e.target.value }))} placeholder="7777:udp, 8888:both" required /></label>
              <p className="hint" style={{ margin: '8px 0 0' }}><span className="mono">port:proto</span> comma-separated, proto = <span className="mono">tcp</span>/<span className="mono">udp</span>/<span className="mono">both</span>. Auto-filled from the game template — tweak if your server is customised.</p>
            </div>

            <div className="row">
              <label className="row" style={{ gap: 8, color: 'var(--muted)' }}>
                <input type="checkbox" checked={form.enabled} onChange={e => setForm(p => ({ ...p, enabled: e.target.checked }))} style={{ width: 'auto' }} /> enabled
              </label>
              <button type="submit" className="primary">Add entry</button>
              <span className="err-msg">{formErr}</span>
            </div>
          </form>
        </div>
        </>)}

        {tab === 'web' && <WebRoutes domains={domains} />}

        {/* Domains — shared by both tabs (web routes compose hostnames from
            the same base domains). */}
        <div className="card">
          <h2>Domains <span className="badge" style={{ marginLeft: 10 }}>{domains.length || 'none'}</span></h2>
          <p className="hint" style={{ margin: '0 0 14px' }}>Base domains you control. They populate the DNS-name dropdown when adding a tunnel or web route as <span className="mono">&lt;name&gt;.&lt;domain&gt;</span>.</p>
          <div className="row">
            <input value={domainInput} onChange={e => setDomainInput(e.target.value)}
                   onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addDomain() } }}
                   placeholder="examplelabs.cc  (or *.examplelabs.cc)" style={{ maxWidth: 340 }} />
            <button type="button" className="action sm" onClick={addDomain}>Add domain</button>
            <span className="err-msg" style={{ margin: 0 }}>{domainErr}</span>
          </div>
          <div className="list" style={{ marginTop: 14 }}>
            {domains.length === 0 && (
              <div className="item" style={{ color: 'var(--muted)' }}>No domains yet — add one above, or connect Cloudflare in Setup for auto-discovery.</div>
            )}
            {domains.map(dn => {
              const isAuto = autoDomains.has(dn)
              return (
                <div key={dn} className="item row" style={{ justifyContent: 'space-between' }}>
                  <span><span className="mono">{dn}</span>{isAuto && <span className="pill ready" style={{ marginLeft: 8 }}>auto · CF</span>}</span>
                  {!isAuto && <button className="sm danger" onClick={() => removeDomain(dn)}>Remove</button>}
                </div>
              )
            })}
          </div>
        </div>

        {/* Gateway keys storage location */}
        <KeysCard />

        {/* Apply */}
        <div className="card" id="applyCard">
          <h2>Apply</h2>
          <p className="hint" style={{ margin: '0 0 14px' }}>Pushes entries to the droplet + gateway, and (if configured) updates Cloudflare DNS — one click.</p>
          <div className={'pending' + (data.pending ? ' on' : '')}>
            <span className="dotp" /><span>Unapplied changes — entries differ from what's live. Click <strong>Apply now</strong> to push them.</span>
          </div>
          <div className="row">
            <button className="primary" disabled={!!applyJob?.running} onClick={applyNow}>Apply now</button>
            <ApplyMsg job={applyJob} />
          </div>
          <ApplyOutput job={applyJob} />
        </div>

        <p className="hint" style={{ textAlign: 'center', margin: '28px 0 4px', opacity: .7 }}>
          ProxyCTL — WireGuard ↔ Kubernetes proxy + Cloudflare DNS front end · loopback + token · zero stored credentials
        </p>
      </main>

      <PickerModal open={pickerOpen} onClose={() => setPickerOpen(false)} onPick={applyPick} />
    </>
  )
}

// === Sub-components ========================================================

function EntryRow({ r, stats, onToggle, onRebind, onDelete }) {
  const e = r.entry
  const portStr = (e.ports || []).map(p => `${p.port}/${p.proto}`).join(', ')
  const dr = r.drift
  let chgPill = null
  if (r.change?.status === 'new')      chgPill = <span className="chg new">● new — not yet pushed</span>
  else if (r.change?.status === 'changed') chgPill = <span className="chg changed">● changed <span className="fl">{(r.change.fields || []).join(', ')}</span></span>

  let statusCls = 'dis', statusTxt = 'disabled', statusTip = 'disabled (not forwarded)'
  if (e.enabled && stats) {
    if (stats.online) { statusCls = 'live'; statusTxt = 'live'; statusTip = 'tunnel up — recent WireGuard handshake' }
    else              { statusCls = 'down'; statusTxt = 'down'; statusTip = 'enabled but no recent handshake (gateway not up, or not applied yet)' }
  } else if (e.enabled) { statusCls = 'down'; statusTxt = 'checking…'; statusTip = 'enabled — checking tunnel…' }

  let connTxt = '–', connTip = ''
  if (stats) {
    if (stats.connections < 0) { connTxt = 'n/a'; connTip = 'install conntrack-tools on the droplet for live counts' }
    else                       { connTxt = stats.connections; connTip = stats.connections + ' active flow(s)' }
  }

  const rateStr = v => v == null ? '·' : (v > 1 ? fmtBytes(v) + '/s' : 'idle')

  return (
    <tr>
      <td><span className={'stat ' + statusCls} title={statusTip}><span className="d" /><span className="t">{statusTxt}</span></span></td>
      <td>
        <strong>{e.name}</strong>
        <br /><span className="mono" style={{ color: 'var(--muted)' }}>{e.subdomain || ''}</span>
        {chgPill && <><br />{chgPill}</>}
      </td>
      <td className="mono">{portStr}</td>
      <td className="mono">
        {e.targetIP}
        {e.service && <><br /><span style={{ color: 'var(--muted)' }}>{e.service}</span></>}
        {dr?.mismatch && (
          <><br />
          <span className="bad" style={{ fontSize: 11 }}>⚠ target drifted — {dr.found ? <>live ClusterIP is <strong>{dr.live}</strong></> : <>Service <strong>not found</strong> in cluster</>}</span>{' '}
          <button className="sm action" style={{ padding: '2px 8px', fontSize: 11 }} onClick={() => onRebind(e.id, e.name)}>Rebind</button>
          </>
        )}
      </td>
      <td className="num mono" title={connTip}>{connTxt}</td>
      <td className="num mono">
        {stats ? (
          <span style={{ display: 'inline-flex', gap: 16, justifyContent: 'flex-end' }}>
            <span style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-end', gap: 1 }}>
              <span style={{ color: 'var(--link)', fontSize: 13, whiteSpace: 'nowrap' }}>↓ {rateStr(stats.rRx)}</span>
              <span style={{ color: 'var(--muted)', fontSize: 10, whiteSpace: 'nowrap' }}>{fmtBytes(stats.rxBytes)}</span>
            </span>
            <span style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-end', gap: 1 }}>
              <span style={{ color: 'var(--ok-tx)', fontSize: 13, whiteSpace: 'nowrap' }}>↑ {rateStr(stats.rTx)}</span>
              <span style={{ color: 'var(--muted)', fontSize: 10, whiteSpace: 'nowrap' }}>{fmtBytes(stats.txBytes)}</span>
            </span>
          </span>
        ) : '–'}
      </td>
      <td className="row">
        <button className="sm" onClick={() => onToggle(e.id)}>{e.enabled ? 'Disable' : 'Enable'}</button>
        <button className="sm danger" onClick={() => onDelete(e.id, e.name, e.subdomain)}>Delete</button>
      </td>
    </tr>
  )
}

function ApplyMsg({ job }) {
  if (!job || job.idle) return null
  const now = Math.floor(Date.now() / 1000)
  const took = (job.endedAt || now) - (job.startedAt || now)
  if (job.running) {
    const cur = job.steps?.length ? job.steps[job.steps.length - 1].name : 'starting…'
    return <span className="hint" style={{ margin: 0 }}><span className="spin" />applying… {took}s — {cur} <span style={{ opacity: .6 }}>({job.steps?.length || 0} step{job.steps?.length === 1 ? '' : 's'} done)</span></span>
  }
  return <span className="hint" style={{ margin: 0 }}><span className={job.ok ? 'ok' : 'bad'}>{job.ok ? '✓ OK' : '✗ FAILED'}</span> — {job.message || ''} <span style={{ opacity: .6 }}>({took}s)</span></span>
}

function ApplyOutput({ job }) {
  if (!job || job.idle) return null
  const steps = job.steps || []
  if (!steps.length && !job.running) return null
  const okN = steps.filter(s => s.ok).length
  const badN = steps.length - okN
  const sum = job.running
    ? `Apply log — ${steps.length} step${steps.length === 1 ? '' : 's'} so far · working…`
    : `Apply log — ${steps.length} step${steps.length === 1 ? '' : 's'} · `
  const open = job.running || badN > 0
  return (
    <div style={{ marginTop: 14 }}>
      <div className={'pbar' + (!job.running && job.ok ? ' done' : '') + (!job.running && !job.ok ? ' fail' : '')}><i /></div>
      <details className="hist" {...(open ? { open: true } : {})}>
        <summary>{sum}{!job.running && <><span className="ok">{okN}✓</span>{badN > 0 && <> · <span className="bad">{badN}✗</span></>}</>}</summary>
        <div className="steps">
          {steps.map((s, i) => {
            const det = [(s.stdout || '').trim(), (s.stderr || '').trim() ? '— stderr —\n' + s.stderr.trim() : ''].filter(Boolean).join('\n')
            const icon = s.ok ? <span className="g ok">✓</span> : <span className="g bad">✗</span>
            if (!det) return <div key={i} className="stp plain">{icon}<span className="nm">{s.name}</span></div>
            return (
              <details key={i} className="stp" {...(s.ok ? {} : { open: true })}>
                <summary>{icon}<span className="nm">{s.name}</span><span className="more">details</span></summary>
                <pre>{det}</pre>
              </details>
            )
          })}
          {job.running && <div className="stp plain"><span className="spin" /><span className="nm" style={{ color: 'var(--muted)' }}>working…</span></div>}
        </div>
      </details>
    </div>
  )
}

function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"]/g, c => ({ '&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;' }[c]))
}

// KeysCard — choose (and later move) the NFS folder where per-gateway WireGuard
// keypairs are stored. Saving creates the matching StorageClass so new gateways
// land in the folder; "Move existing" re-keys current gateways into it.
function KeysCard() {
  const { toast, ask } = useUI()
  const [cfg, setCfg] = useState(null)
  const [val, setVal] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/keys-config')
    if (ok && data) { setCfg(data); setVal(data.basePath || '') }
  }, [])
  useEffect(() => { load() }, [load])

  if (!cfg) return null
  const dirty = val.trim() !== (cfg.basePath || '')
  const onOther = cfg.gatewayKeysOnOtherPath || 0

  async function save() {
    setBusy(true)
    const { ok, data } = await putJSON('/api/keys-config', { basePath: val.trim() })
    setBusy(false)
    if (!ok) { toast(data?.error || 'Save failed', true); return }
    toast('✓ Keys folder saved — new gateways will use it')
    load()
  }

  async function move() {
    const okGo = await ask(
      `Move existing gateway keys to "${cfg.basePath}"? Each tunnel briefly drops ` +
      `while ProxyCTL re-creates its gateway, regenerates its key, and re-registers ` +
      `it on the droplet. Other state is untouched.`
    )
    if (!okGo) return
    setBusy(true)
    const { ok, data } = await postJSON('/api/keys-config/migrate', {})
    setBusy(false)
    if (!ok && data?.error) { toast(data.error, true); return }
    toast('· Moving keys — watch the Apply output')
    load()
  }

  return (
    <div className="card" id="keysCard">
      <h2>Gateway keys</h2>
      <p className="hint" style={{ margin: '0 0 14px' }}>
        Where each tunnel's WireGuard keypair is stored on the NFS SSD. Tiny files;
        keeping them tidy under one folder avoids cluttering the share root.
      </p>
      <div className="row" style={{ alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
        <label style={{ color: 'var(--muted)', minWidth: 90 }}>Keys folder</label>
        <input
          className="mono" value={val} disabled={busy}
          onChange={e => setVal(e.target.value)}
          placeholder={cfg.defaultBasePath}
          style={{ minWidth: 220, flex: 1 }}
        />
        <button className="primary" disabled={busy || !dirty || !val.trim()} onClick={save}>Save</button>
      </div>
      <p className="hint mono" style={{ margin: '8px 0 0', opacity: .8 }}>{cfg.locationHint}</p>
      <p className="hint" style={{ margin: '4px 0 0', opacity: .7 }}>
        StorageClass: <span className="mono">{cfg.storageClass}</span>
        {typeof cfg.gatewayKeysTotal === 'number' && <> · {cfg.gatewayKeysTotal} gateway key volume(s)</>}
      </p>
      {onOther > 0 && (
        <div className="row" style={{ alignItems: 'center', gap: 10, marginTop: 12 }}>
          <span className="bad" style={{ fontSize: 12 }}>
            ⚠ {onOther} gateway{onOther === 1 ? '' : 's'} still on a different folder.
          </span>
          <button className="action" disabled={busy} onClick={move}>Move existing gateways</button>
        </div>
      )}
      {cfg.uncoveredNodes?.length > 0 && (
        <div style={{ marginTop: 12 }}>
          <span className="bad" style={{ fontSize: 12 }}>
            ⚠ Node{cfg.uncoveredNodes.length === 1 ? '' : 's'} added since the keys share was
            saved: <span className="mono">{cfg.uncoveredNodes.map(n => `${n.name} (${n.ip})`).join(', ')}</span>.
            If the share is restricted by IP, {cfg.uncoveredNodes.length === 1 ? 'it' : 'they'} can't mount
            it yet — replace the share's line in <span className="mono">/etc/exports</span> on the NFS
            server with the line below, then run <span className="mono">exportfs -ra</span>.
          </span>
          <pre className="mono" style={{ userSelect: 'all', whiteSpace: 'pre', overflowX: 'auto', marginTop: 8 }}>{cfg.updatedExportsLine}</pre>
          <button className="action" disabled={busy} style={{ marginTop: 6 }} onClick={async () => {
            const { ok, data } = await postJSON('/api/storage/share-setup/ack', {})
            if (!ok) { toast(data?.error || 'failed', true); return }
            toast('✓ share marked as covering all current nodes')
            load()
          }}>I've updated the exports</button>
        </div>
      )}
    </div>
  )
}
