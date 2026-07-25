import { useCallback, useEffect, useState } from 'react'
import { apiJSON, postJSON, del, apiFetch } from './api.js'
import { useUI } from './ui.jsx'
import PickerModal from './PickerModal.jsx'
import HeartbeatBar from './HeartbeatBar.jsx'

// Web apps — the L7 section. Each web route is hostname → in-cluster
// Service, exposed through a Cloudflare Tunnel: cloudflared runs in the
// cluster, dials out to Cloudflare's edge (TLS + WAF + DDoS there), and
// routes each hostname straight to the Service. No droplet, no certs.
export default function WebRoutes({ domains, onRoutesChange }) {
  const { ask, toast } = useUI()
  const [routes, setRoutes] = useState([])
  const [cfConfigured, setCfConfigured] = useState(false)
  const [tunnel, setTunnel] = useState({ connectorPresent: false, cloudflaredReady: false })
  const [pickerOpen, setPickerOpen] = useState(false)
  const [formErr, setFormErr] = useState('')
  const blank = { hostname: '', domain: '', namespace: '', service: '', port: '' }
  const [form, setForm] = useState(blank)

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

  async function submit(ev) {
    ev.preventDefault()
    setFormErr('')
    const body = {
      hostname: form.hostname.trim(),
      namespace: form.namespace.trim(),
      service: form.service.trim(),
      port: parseInt(form.port, 10) || 0,
      enabled: true,
    }
    const { ok, data } = await postJSON('/api/webroutes', body)
    if (!ok) { setFormErr(data?.error || 'failed'); return }
    setForm(blank)
    load()
  }

  async function toggle(id) {
    await apiFetch(`/api/webroutes/${id}/toggle`, { method: 'POST' })
    load()
  }
  async function remove(rt) {
    if (!await ask(`Delete web route <strong>${esc(rt.hostname)}</strong>? It stays live until you Apply (which drops it from the tunnel).`, { ok: 'Delete route' })) return
    await del(`/api/webroutes/${rt.id}`)
    load()
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
                <td><span className={'stat ' + (rt.enabled ? 'down' : 'dis')}>
                  <span className="d" /><span className="t">{rt.enabled ? 'enabled' : 'disabled'}</span></span></td>
                <td>{rt.enabled ? <HeartbeatBar kind="webroutes" id={rt.id} hours={1} compact /> : <span style={{ fontSize: 10, color: 'var(--muted-2)' }}>disabled</span>}</td>
                <td>
                  <a href={`https://${rt.hostname}`} target="_blank" rel="noopener noreferrer"
                    style={{ color: 'var(--link)', fontWeight: 600, textDecoration: 'none' }}>
                    https://{rt.hostname}
                  </a>
                </td>
                <td className="mono">{rt.namespace}/{rt.service}:{rt.port}</td>
                <td className="row">
                  <button className="sm" onClick={() => toggle(rt.id)}>{rt.enabled ? 'Disable' : 'Enable'}</button>
                  <button className="sm danger" onClick={() => remove(rt)}>Delete</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        </div>
      </div>

      <div className="card">
        <h2>Add web app</h2>
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
            <button type="submit" className="primary">Add web app</button>
            <span className="err-msg">{formErr}</span>
          </div>
        </form>
      </div>

      <PickerModal open={pickerOpen} onClose={() => setPickerOpen(false)} onPick={onPick} />
    </>
  )
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"]/g, c => ({ '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;' }[c]))
}
