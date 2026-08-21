import { useCallback, useEffect, useState } from 'react'
import { apiJSON, postJSON, putJSON, del, apiFetch } from './api.js'
import { useUI } from './ui.jsx'
import PickerModal from './PickerModal.jsx'
import HeartbeatBar from './HeartbeatBar.jsx'

// Web apps — the L7 section. Each web route is hostname → in-cluster
// Service, exposed through a Cloudflare Tunnel: cloudflared runs in the
// cluster, dials out to Cloudflare's edge (TLS + WAF + DDoS there), and
// routes each hostname straight to the Service. No droplet, no certs.
// onChanged fires after a MUTATION (add / edit / toggle / delete) so the
// parent can refresh the unapplied-changes indicator. Deliberately not
// called from load(): the pending banner lives on Admin's /api/entries
// payload, which nothing re-fetched after a web-route edit — so the bar
// stayed hidden until some other action happened to refresh it (usually
// clicking Apply, i.e. exactly too late to be useful).
export default function WebRoutes({ domains, onRoutesChange, onChanged }) {
  const { ask, toast } = useUI()
  const [routes, setRoutes] = useState([])
  const [cfConfigured, setCfConfigured] = useState(false)
  const [tunnel, setTunnel] = useState({ connectorPresent: false, cloudflaredReady: false })
  const [pickerOpen, setPickerOpen] = useState(false)
  const [formErr, setFormErr] = useState('')
  const blank = { hostname: '', domain: '', namespace: '', service: '', port: '' }
  const [form, setForm] = useState(blank)
  // Non-null while the form is editing an existing route rather than
  // adding one. A web route has no server-generated state (no tunnel IP,
  // no key — cloudflared routes purely by hostname), so unlike a proxy
  // entry the PUT can carry the whole object safely.
  const [editingId, setEditingId] = useState(null)

  const load = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/webroutes')
    if (ok && data) {
      setRoutes(data.routes || [])
      setCfConfigured(!!data.cfConfigured)
      onRoutesChange?.(data.routes || [])
    }
    const t = await apiJSON('/api/tunnel/status')
    if (t.ok && t.data) setTunnel(t.data)
  }, [onRoutesChange])

  useEffect(() => { load() }, [load])

  // Compose <name>.<domain> into hostname when a domain is picked.
  function setFormPair(patch) {
    setForm(prev => {
      const next = { ...prev, ...patch }
      if (next.domain && ('host' in patch || 'domain' in patch)) {
        const lead = (patch.host ?? next.hostname.split('.')[0] ?? 'app')
          .trim().toLowerCase().replace(/[^a-z0-9-]/g, '-') || 'app'
        next.hostname = `${lead}.${next.domain}`
      }
      return next
    })
  }

  // startEdit loads an existing route into the form. Editing used to mean
  // delete + re-add, which dropped the route from the tunnel and rebuilt
  // its DNS record just to correct a port.
  function startEdit(rt) {
    setEditingId(rt.id)
    setFormErr('')
    setForm({
      hostname: rt.hostname || '',
      domain: (rt.hostname || '').split('.').slice(1).join('.'),
      namespace: rt.namespace || '',
      service: rt.service || '',
      port: rt.port ? String(rt.port) : '',
    })
    document.querySelector('#webRouteForm')?.scrollIntoView({ behavior: 'smooth', block: 'center' })
  }

  function cancelEdit() {
    setEditingId(null)
    setFormErr('')
    setForm(blank)
  }

  async function submit(ev) {
    ev.preventDefault()
    setFormErr('')
    // Editing keeps the route's current enabled state; a new one starts on.
    const current = editingId ? routes.find(r => r.id === editingId) : null
    const body = {
      hostname: form.hostname.trim(),
      namespace: form.namespace.trim(),
      service: form.service.trim(),
      port: parseInt(form.port, 10) || 0,
      enabled: current ? current.enabled !== false : true,
    }
    const { ok, data } = editingId
      ? await putJSON(`/api/webroutes/${editingId}`, body)
      : await postJSON('/api/webroutes', body)
    if (!ok) { setFormErr(data?.error || 'failed'); return }
    setEditingId(null)
    setForm(blank)
    load()
    onChanged?.()
  }

  async function toggle(id) {
    await apiFetch(`/api/webroutes/${id}/toggle`, { method: 'POST' })
    load()
    onChanged?.()
  }
  async function remove(rt) {
    if (!await ask(`Delete web route <strong>${esc(rt.hostname)}</strong>? It stays live until you Apply (which drops it from the tunnel).`, { ok: 'Delete route' })) return
    await del(`/api/webroutes/${rt.id}`)
    load()
    onChanged?.()
  }

  // From PickerModal — fill namespace/service/port from the picked Service.
  function onPick(svc) {
    setPickerOpen(false)
    const firstPort = (svc.ports || [])[0]
    setForm(prev => ({
      ...prev,
      namespace: svc.namespace,
      service: svc.name,
      port: firstPort ? String(firstPort.port) : prev.port,
    }))
  }

  // Tunnel status banner — one of four states.
  function banner() {
    if (!cfConfigured) {
      return ['bad', <>Web apps route through a <strong>Cloudflare Tunnel</strong> — add your Cloudflare API token in <strong>Setup → Cloudflare</strong> first. The token needs the <span className="mono">Account › Cloudflare Tunnel › Edit</span> permission.</>]
    }
    if (tunnel.cloudflaredReady) {
      return ['ok', <>Cloudflare Tunnel connected — <strong>cloudflared</strong> is running. Add routes below; Apply publishes them.</>]
    }
    if (tunnel.connectorPresent) {
      return ['warn', <>Cloudflare Tunnel connector is starting — give it a moment, then refresh.</>]
    }
    return ['warn', <>Cloudflare Tunnel isn't deployed yet. It comes up automatically on your first <strong>Apply</strong>, or set it up now in <strong>Setup → Cloudflare Tunnel</strong>.</>]
  }
  const [bClass, bText] = banner()

  return (
    <>
      <p className="sub">Web apps are <strong>HTTP-only</strong>, proxied
        through a Cloudflare Tunnel — no droplet, no public IP. In exchange
        you get automatic TLS certificates and Cloudflare's edge (WAF / DDoS
        protection) for free. Non-HTTP game ports live under{' '}
        <strong>Proxy Entries</strong>.</p>

      <div className="nudge on" style={{ cursor: 'default' }}>
        <div className="step" style={{ cursor: 'default' }}>
          <span className="n">{bClass === 'ok' ? '' : '!'}</span>
          <span className="lbl" style={{ color: bClass === 'bad' ? 'var(--err-tx)' : bClass === 'ok' ? 'var(--ok-tx)' : 'var(--warn-tx)' }}>{bText}</span>
        </div>
      </div>

      <div className="card">
        <div className="table-scroll">
        <table>
          <thead>
            <tr><th>Status</th><th>Uptime — last hour</th><th>Hostname</th><th>&rarr; Backend service</th><th></th></tr>
          </thead>
          <tbody>
            {routes.length === 0 && (
              <tr><td colSpan="5" style={{ color: 'var(--muted)' }}>No web routes yet — add one below.</td></tr>
            )}
            {routes.map(rt => (
              <tr key={rt.id}>
                {/* Reachability from the sampler, not just the enabled flag —
                    this cell used to render every enabled route amber. */}
                <td><span className={'stat ' + statusClass(rt)}>
                  <span className="d" /><span className="t">{statusText(rt)}</span></span></td>
                <td>{rt.enabled ? <HeartbeatBar kind="webroutes" id={rt.id} hours={1} compact /> : <span style={{ fontSize: 10, color: 'var(--muted-2)' }}>disabled</span>}</td>
                <td>
                  <a href={`https://${rt.hostname}`} target="_blank" rel="noopener noreferrer"
                    style={{ color: 'var(--link)', fontWeight: 600, textDecoration: 'none' }}>
                    https://{rt.hostname}
                  </a>
                </td>
                <td className="mono">{rt.namespace}/{rt.service}:{rt.port}</td>
                <td className="row" style={{ flexWrap: 'nowrap', whiteSpace: 'nowrap' }}>
                  <button className="sm" onClick={() => startEdit(rt)}>Edit</button>
                  <button className="sm" onClick={() => toggle(rt.id)}>{rt.enabled ? 'Disable' : 'Enable'}</button>
                  <button className="sm danger" onClick={() => remove(rt)}>Delete</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        </div>
      </div>

      <div className="card" id="webRouteForm">
        <h2>{editingId ? 'Edit web app' : 'Add web app'}</h2>
        {editingId && (
          <p className="hint" style={{ margin: '0 0 12px' }}>
            Editing an existing route &mdash; it keeps its id and enabled state.
            Changes go live on the next <strong>Apply</strong>.
          </p>
        )}
        <form className="wiz" onSubmit={submit}>
          <div className="step">
            <p className="st">1 &middot; Public hostname</p>
            <div className="pair">
              <label>Domain
                <select value={form.domain} onChange={e => setFormPair({ domain: e.target.value })}>
                  <option value="">&mdash; custom / type below &mdash;</option>
                  {(domains || []).map(d => <option key={d} value={d}>{`<name>.${d}`}</option>)}
                </select>
              </label>
              <label>Full hostname
                <input value={form.hostname} onChange={e => setForm(p => ({ ...p, hostname: e.target.value }))}
                       placeholder="jellyfin.examplelabs.cc" autoComplete="off" required />
              </label>
            </div>
            <p className="hint" style={{ margin: '10px 0 0' }}>Apply creates a proxied CNAME for this hostname pointing at the tunnel — Cloudflare handles DNS + TLS.</p>
          </div>

          <div className="step">
            <p className="st">2 &middot; Backend &mdash; the Kubernetes Service</p>
            <div className="row" style={{ marginBottom: 12 }}>
              <button type="button" className="action sm" onClick={() => setPickerOpen(true)}>Pick from cluster&hellip;</button>
              <span className="hint" style={{ margin: 0 }}>Fills namespace + service + port. Or type them.</span>
            </div>
            <div className="pair">
              <label>Namespace<input value={form.namespace} onChange={e => setForm(p => ({ ...p, namespace: e.target.value }))} placeholder="media" required /></label>
              <label>Service<input value={form.service} onChange={e => setForm(p => ({ ...p, service: e.target.value }))} placeholder="jellyfin" required /></label>
            </div>
            <label>Service port<input value={form.port} onChange={e => setForm(p => ({ ...p, port: e.target.value }))} placeholder="8096" required style={{ maxWidth: 200 }} /></label>
          </div>

          <div className="row">
            <button type="submit" className="primary">{editingId ? 'Save changes' : 'Add web app'}</button>
            {editingId && <button type="button" className="sm" onClick={cancelEdit}>Cancel</button>}
            <span className="err-msg">{formErr}</span>
          </div>
        </form>
      </div>

      <PickerModal open={pickerOpen} onClose={() => setPickerOpen(false)} onPick={onPick} />
    </>
  )
}

// A disabled route is grey; an enabled one follows its last probe —
// green when the public hostname answered, amber when it didn't. A route
// with no sample yet reads "enabled" rather than claiming either.
function statusClass(rt) {
  if (!rt.enabled) return 'dis'
  if (rt.state === 'up') return 'live'
  if (rt.state === 'down') return 'down'
  return 'dis'
}

function statusText(rt) {
  if (!rt.enabled) return 'disabled'
  if (rt.state === 'up') return 'online'
  if (rt.state === 'down') return 'unreachable'
  return 'enabled'
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"]/g, c => ({ '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;' }[c]))
}
