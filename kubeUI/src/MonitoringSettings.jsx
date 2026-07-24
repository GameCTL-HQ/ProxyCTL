import { useEffect, useState } from 'react'
import { apiJSON, postJSON, putJSON } from './api.js'
import HeartbeatBar from './HeartbeatBar.jsx'
import LatencyLine from './LatencyLine.jsx'

const DURATIONS = [
  { label: '1h', hours: 1 },
  { label: '6h', hours: 6 },
  { label: '12h', hours: 12 },
  { label: '1d', hours: 24 },
  { label: '3d', hours: 72 },
  { label: '7d', hours: 168 },
]

// Monitoring settings: what the background uptime sampler is currently
// watching (same data behind the Tunnels/Web Apps table sparklines) and the
// Discord webhook that fires on reachability transitions. Each row expands
// into a larger graph than the table's compact sparkline, with a single
// duration toggle that applies to every expanded graph on the page.
export default function MonitoringSettings() {
  const [summary, setSummary] = useState(null)
  const [summaryErr, setSummaryErr] = useState('')
  const [expanded, setExpanded] = useState(() => new Set())
  const [hours, setHours] = useState(1)

  useEffect(() => {
    let stop = false
    const load = () => apiJSON('/api/monitoring/summary')
      .then(({ ok, data }) => { if (!stop) { if (ok) setSummary(data.instances || []); else setSummaryErr(data?.error || 'failed to load') } })
      .catch(() => {})
    load()
    const t = setInterval(load, 15000)
    return () => { stop = true; clearInterval(t) }
  }, [])

  const [cfg, setCfg] = useState(null)
  const [url, setUrl] = useState('')
  const [enabled, setEnabled] = useState(false)
  const [retentionDays, setRetentionDays] = useState(30)
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [msg, setMsg] = useState('')
  const [err, setErr] = useState('')

  useEffect(() => {
    apiJSON('/api/alerts/config').then(({ ok, data }) => {
      if (!ok) return
      setCfg(data)
      setUrl(data.discordWebhookUrl || '')
      setEnabled(!!data.enabled)
      setRetentionDays(data.retentionDays > 0 ? data.retentionDays : 30)
    })
  }, [])

  const save = async () => {
    setSaving(true); setMsg(''); setErr('')
    const body = { discordWebhookUrl: url.trim(), enabled, retentionDays: Math.max(1, Number(retentionDays) || 30) }
    const { ok, data } = await putJSON('/api/alerts/config', body)
    setSaving(false)
    if (!ok) { setErr(data?.error || 'save failed'); return }
    setMsg('Saved.')
    setCfg(body)
  }

  const test = async () => {
    setTesting(true); setMsg(''); setErr('')
    const { ok, data } = await postJSON('/api/alerts/test', {})
    setTesting(false)
    if (!ok) { setErr(data?.error || 'test failed'); return }
    setMsg('Test message sent — check your Discord channel.')
  }

  const cfgRetention = cfg?.retentionDays > 0 ? cfg.retentionDays : 30
  const dirty = cfg && (url.trim() !== (cfg.discordWebhookUrl || '') || enabled !== !!cfg.enabled || Number(retentionDays) !== cfgRetention)

  return (
    <>
      <p className="sub">
        What the background sampler is currently watching, and where it sends alerts when a
        Proxy Entry's tunnel or a Web App's HTTPS reachability changes.
      </p>

      <div className="card">
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 4 }}>
          <h2 style={{ margin: 0 }}>Currently monitored</h2>
          <div style={{ marginLeft: 'auto', display: 'flex', gap: 4 }}>
            {DURATIONS.map(d => (
              <button key={d.hours} className="sm"
                style={hours === d.hours ? { background: 'var(--accent-h)', borderColor: 'var(--accent-h)', color: '#fff' } : {}}
                onClick={() => setHours(d.hours)}>
                {d.label}
              </button>
            ))}
          </div>
        </div>
        <p className="hint" style={{ marginBottom: 10 }}>Click a row to expand its graph.</p>
        {summaryErr && <p className="bad">{summaryErr}</p>}
        {!summary && !summaryErr && <p style={{ color: 'var(--muted)' }}>Loading…</p>}
        {summary && summary.length === 0 && (
          <p style={{ color: 'var(--muted)' }}>
            No samples yet — the background sampler runs every 60s once entries/routes are enabled.
          </p>
        )}
        {summary && summary.length > 0 && (
          <ul style={{ listStyle: 'none', padding: 0, margin: 0 }}>
            {summary.map(s => {
              const isOpen = expanded.has(s.id)
              const toggle = () => setExpanded(prev => {
                const next = new Set(prev)
                next.has(s.id) ? next.delete(s.id) : next.add(s.id)
                return next
              })
              return (
                <li key={s.id} style={{ borderTop: '1px solid var(--border-2)' }}>
                  <div
                    onClick={toggle}
                    style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '7px 0', cursor: 'pointer' }}
                  >
                    <span style={{ color: 'var(--muted)', fontSize: 10, width: 10 }}>{isOpen ? '▼' : '▶'}</span>
                    <span className="dot" style={{ background: s.reachable ? 'var(--ok)' : 'var(--err)' }} />
                    <strong style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.name}</strong>
                    <span className="badge" style={{ margin: 0 }}>{s.kind === 'webapp' ? 'web app' : 'tunnel'}</span>
                    <span style={{ color: 'var(--muted)', fontSize: 12 }}>
                      {s.reachable ? `up${s.latencyMs ? ` · ${s.latencyMs}ms` : ''}` : 'unreachable'}
                    </span>
                  </div>
                  {isOpen && (
                    <div style={{ padding: '4px 0 14px 20px', display: 'flex', flexDirection: 'column', gap: 14 }}>
                      <HeartbeatBar kind={s.kind === 'webapp' ? 'webroutes' : 'entries'} id={s.id} hours={hours} />
                      <LatencyLine kind={s.kind === 'webapp' ? 'webroutes' : 'entries'} id={s.id} hours={hours} />
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        )}
      </div>

      <div className="card">
        <h2>Data retention</h2>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <input
            type="number" min="1" max="365" style={{ width: 80 }}
            value={retentionDays}
            onChange={e => setRetentionDays(e.target.value)}
          />
          <span style={{ fontSize: 13 }}>days of uptime history</span>
        </div>
        <p className="hint" style={{ marginTop: 8 }}>
          History is snapshotted to ProxyCTL's data volume every few minutes, so it survives
          restarts. Saved with the Save button below.
        </p>
      </div>

      <div className="card">
        <h2>Discord alerts</h2>
        <p className="hint" style={{ marginBottom: 14 }}>
          Fires a message whenever a monitored entry's reachability changes. Only transitions
          notify, not every sample, so this won't spam you.
        </p>

        <label style={{ display: 'block', marginBottom: 10 }}>
          <span style={{ display: 'block', fontSize: 12, color: 'var(--muted)', marginBottom: 4 }}>Webhook URL</span>
          <input
            className="mono"
            style={{ width: '100%' }}
            placeholder="https://discord.com/api/webhooks/…"
            value={url}
            onChange={e => setUrl(e.target.value)}
          />
        </label>
        <label style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 14, cursor: 'pointer' }}>
          <input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)} />
          Enabled
        </label>

        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <button className="primary" disabled={saving || !dirty} onClick={save}>{saving ? 'Saving…' : 'Save'}</button>
          <button disabled={testing || !cfg?.discordWebhookUrl} title={!cfg?.discordWebhookUrl ? 'Save a webhook URL first' : 'Send a test message'} onClick={test}>
            {testing ? 'Sending…' : 'Send test message'}
          </button>
        </div>
        {msg && <p style={{ color: 'var(--ok-tx)', marginTop: 10 }}>{msg}</p>}
        {err && <p className="bad" style={{ marginTop: 10 }}>{err}</p>}

        <details style={{ marginTop: 14, fontSize: 12, color: 'var(--muted)' }}>
          <summary style={{ cursor: 'pointer', color: 'var(--text-2)' }}>How do I get a Discord webhook URL?</summary>
          <ol style={{ marginTop: 8, paddingLeft: 20 }}>
            <li>In Discord, open the server you want alerts in.</li>
            <li>Server Settings → Integrations → Webhooks.</li>
            <li>New Webhook — name it and pick the channel.</li>
            <li>Copy Webhook URL, paste it above, then Save.</li>
          </ol>
        </details>
      </div>
    </>
  )
}
