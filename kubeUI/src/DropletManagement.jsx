import { useCallback, useEffect, useRef, useState } from 'react'
import { apiJSON, postJSON, putJSON } from './api.js'
import { useUI } from './ui.jsx'
import DropletStatsCharts from './DropletStatsCharts.jsx'

function fmtUptime(seconds) {
  let left = Math.max(0, Number(seconds) || 0)
  const days = Math.floor(left / 86400); left %= 86400
  const hours = Math.floor(left / 3600); left %= 3600
  const minutes = Math.floor(left / 60)
  return [days && `${days}d`, (hours || days) && `${hours}h`, `${minutes}m`].filter(Boolean).join(' ')
}

function fmtCloudflaredVersion(raw) {
  const text = String(raw || '')
  return text.match(/cloudflared version\s+([^\s]+)/i)?.[1] || text || 'Version unavailable'
}

function shortDigest(ref) {
  const m = String(ref || '').match(/sha256:([0-9a-f]{12})[0-9a-f]+$/)
  if (!m) return ref || '—'
  return 'sha256:' + m[1] + '…'
}

function fmtDay(unix) {
  const n = Number(unix)
  if (!n) return '—'
  return new Date(n * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })
}

function persistedAction(kind) {
  switch (kind) {
    case 'update':
      return { pending: true, text: 'Updating droplet packages and the cluster Cloudflare connector… this can take several minutes.' }
    case 'wireguard-image':
      return { pending: true, text: 'Updating WireGuard… cluster pods recreate per gateway, then the droplet builds its copy — a few minutes total.' }
    case 'reboot':
      return { pending: true, text: 'Rebooting / reconnecting… ProxyCTL will re-attach automatically when SSH responds again.' }
    case 'apply':
      return { pending: true, text: 'Applying changes to the droplet…' }
    default:
      return kind ? { pending: true, text: 'Droplet maintenance is still running…' } : null
  }
}

function Metric({ label, value, detail }) {
  return (
    <div style={{ background: 'var(--panel-2)', border: '1px solid var(--border)', borderRadius: 9, padding: '12px 14px', minWidth: 150, flex: '1 1 170px' }}>
      <div className="hint" style={{ margin: 0, fontSize: 11, textTransform: 'uppercase', letterSpacing: '.05em' }}>{label}</div>
      <div style={{ fontSize: 21, fontWeight: 750, marginTop: 4 }}>{value}</div>
      {detail && <div className="hint" style={{ margin: '3px 0 0', fontSize: 11 }}>{detail}</div>}
    </div>
  )
}

function Service({ label, active }) {
  return <span className={'pill ' + (active ? 'ready' : 'bad')} style={{ marginRight: 8 }}>{active ? '●' : '○'} {label}</span>
}

export default function DropletManagement() {
  const { ask, toast } = useUI()
  const [result, setResult] = useState(null)
  const [loading, setLoading] = useState(true)
  const [action, setAction] = useState('')
  const [actionMsg, setActionMsg] = useState(null)
  const [reconnecting, setReconnecting] = useState(false)
  const [wg, setWg] = useState(null)
  const [autoUp, setAutoUp] = useState(null)
  const seq = useRef(0)
  const timers = useRef([])
  const loadInFlight = useRef(false)
  const serverBusy = result?.busy || ''
  const activeAction = action || serverBusy
  const visibleActionMsg = (action ? actionMsg : null) || persistedAction(serverBusy) || actionMsg
  const isReconnecting = reconnecting || serverBusy === 'reboot'

  const loadWg = useCallback(async () => {
    try {
      const { ok, data } = await apiJSON('/api/wireguard/image')
      if (ok && data) setWg(data)
    } catch { /* best-effort: keep previous wireguard status */ }
  }, [])

  const loadAutoUp = useCallback(async () => {
    try {
      const { ok, data } = await apiJSON('/api/droplet/autoupdate')
      if (ok && data) setAutoUp(data)
    } catch { /* best-effort: keep previous auto-update state */ }
  }, [])

  const load = useCallback(async (force = false) => {
    // The status endpoint is a live SSH probe with a 20s timeout. Skip
    // background ticks while one is still in flight so slow droplets don't
    // stack probes; a manual Refresh (force) always runs.
    if (!force && loadInFlight.current) return
    const my = ++seq.current
    loadInFlight.current = true
    setLoading(true)
    try {
      const { ok, data } = await apiJSON('/api/droplet/management')
      if (my !== seq.current) return
      const next = ok && data ? data : { configured: true, reachable: false, error: data?.error || 'status request failed' }
      setResult(next)
      if (next.reachable) setReconnecting(false)
    } catch (err) {
      if (my === seq.current) setResult({ configured: true, reachable: false, error: String(err) })
    } finally {
      loadInFlight.current = false
      if (my === seq.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    load(true)
    loadWg()
    loadAutoUp()
    const timer = setInterval(() => load(false), 15000)
    const wgTimer = setInterval(loadWg, 300000)
    return () => {
      seq.current++
      loadInFlight.current = false
      clearInterval(timer)
      clearInterval(wgTimer)
      timers.current.forEach(clearTimeout)
      timers.current = []
    }
  }, [load, loadWg, loadAutoUp])

  const later = useCallback((fn, ms) => {
    const id = setTimeout(fn, ms)
    timers.current.push(id)
    return id
  }, [])

  const setAutoUpDays = async (days) => {
    const { ok, data } = await putJSON('/api/droplet/autoupdate', { everyDays: days })
    if (ok && data) setAutoUp(data)
    else toast('Could not save the auto-update interval.', false)
  }

  const updateDroplet = async () => {
    if (!await ask(
      'Update the droplet and cluster Cloudflare connector?<br/><br/>This installs available OS/WireGuard packages and rolls the <strong>in-cluster</strong> connector to ProxyCTL’s reviewed digest. It does not install, update, or restart cloudflared on the droplet. Existing WireGuard and Cloudflare configuration is preserved. ProxyCTL will <strong>not reboot automatically</strong>.',
      { ok: 'Update' })) return
    setAction('update')
    setActionMsg({ pending: true, text: 'Updating droplet packages and the cluster Cloudflare connector… this can take several minutes.' })
    try {
      const { ok, data } = await postJSON('/api/droplet/bootstrap', { upgrade: true })
      const success = ok && !!data?.ok
      setActionMsg({ ok: success, text: success ? 'Droplet and cluster connector updates completed. Refreshing status…' : (data?.error || data?.stderr || 'Maintenance update failed.') })
      if (success) toast('✓ Droplet and cluster connector updates completed')
      await load(true)
      if (success) window.dispatchEvent(new Event('proxyctl:maintenance-recheck'))
    } catch (err) {
      setActionMsg({ ok: false, text: 'Droplet update failed: ' + String(err) })
    } finally {
      setAction('')
    }
  }

  const updateWireguardImage = async () => {
    // upstreamRef falls back to the current digest when GHCR is down, so the
    // lookup error — not the ref's presence — decides which source is shown.
    const target = wg?.lookupError ? wg?.currentDigest : (wg?.upstreamRef || wg?.currentDigest)
    const sourceNote = wg?.lookupError
      ? 'the embedded reviewed digest (GHCR check failed)'
      : 'the live upstream <code>latest</code> build'
    if (!await ask(
      'Update <strong>WireGuard</strong>?<br/><br/>1. Rolls the in-cluster WireGuard gateway pods to ' + sourceNote + ':<br/><code>' + (target || '…') + '</code><br/>Each gateway briefly reconnects during its pod recreation. WireGuard keys and all configuration are preserved — only the container image changes.<br/><br/>2. Syncs the remote droplet’s wireguard tools to the same pinned upstream source (built on the droplet, ~1–2 minutes; the distro package stays as rollback).',
      { ok: 'Update now' })) return
    setAction('wgimage')
    setActionMsg({ pending: true, text: 'Updating WireGuard… cluster pods recreate per gateway, then the droplet builds its copy — a few minutes total.' })
    try {
      const { ok, data } = await postJSON('/api/wireguard/image/update', {})
      const success = ok && !!data?.ok
      setActionMsg({ ok: success, text: success ? ('WireGuard updated: ' + (data?.result || '') + ' (applied ' + (data?.source || '') + ')') : (data?.error || 'WireGuard update failed.') })
      if (success) toast('✓ WireGuard updated (cluster + droplet)')
      await Promise.all([load(true), loadWg()])
      if (success) window.dispatchEvent(new Event('proxyctl:maintenance-recheck'))
    } catch (err) {
      setActionMsg({ ok: false, text: 'WireGuard image update failed: ' + String(err) })
    } finally {
      setAction('')
    }
  }

  const rebootDroplet = async () => {
    if (!await ask(
      '<strong>Reboot the remote droplet now?</strong><br/><br/>This will briefly interrupt every WireGuard proxy entry and disconnect active players. ProxyCTL will remain online and reconnect when the droplet returns.',
      { ok: 'Reboot now' })) return
    setAction('reboot')
    setActionMsg({ pending: true, text: 'Scheduling remote reboot…' })
    try {
      const { ok, data } = await postJSON('/api/droplet/reboot', {})
      const success = ok && !!data?.ok
      if (success) {
        setReconnecting(true)
        setActionMsg({ ok: true, text: 'Reboot scheduled. The droplet will reconnect in a moment.' })
        toast('✓ Droplet reboot scheduled')
        later(() => setReconnecting(false), 180000)
        later(() => { load(true) }, 10000)
        later(() => window.dispatchEvent(new Event('proxyctl:maintenance-recheck')), 11000)
      } else {
        setActionMsg({ ok: false, text: data?.error || 'Could not schedule the reboot.' })
      }
    } catch (err) {
      setActionMsg({ ok: false, text: 'Could not schedule the reboot: ' + String(err) })
    } finally {
      setAction('')
    }
  }

  const wireguardCard = (
    <div className="card" style={{ marginTop: 14 }}>
      <div className="row" style={{ justifyContent: 'space-between', alignItems: 'flex-start' }}>
        <div>
          <h2 style={{ marginBottom: 4, fontSize: 16 }}>WireGuard</h2>
          <span className="hint" style={{ margin: 0 }}>In-cluster gateway pods + droplet tools · image: GHCR <code>gamectl-hq/wireguard-kube</code></span>
        </div>
        <Service label="Upstream check" active={!!wg && !wg.lookupError} />
      </div>
      <div className="row" style={{ marginTop: 12, gap: 10, alignItems: 'stretch' }}>
        <Metric label="Current digest" value={shortDigest(wg?.currentDigest)} detail="reviewed, embedded in ProxyCTL" />
        <Metric
          label="Upstream latest"
          value={wg?.lookupError ? 'Unreachable' : (wg?.upstreamRef ? shortDigest(wg.upstreamRef) : 'Checking…')}
          detail={wg?.updateAvailable ? 'update applies this build' : (wg?.lookupError ? 'best-effort check failed; update falls back to embedded digest' : (wg?.upstreamRef ? 'already current' : ''))}
        />
        {wg?.droplet && <Metric
          label="Droplet wireguard"
          value={wg.droplet.version || (wg.droplet.error || 'Checking…')}
          detail={wg.droplet.synced ? 'synced with upstream source'
            : (wg.droplet.version ? 'behind upstream ' + (wg.droplet.expected || '')
              : (wg.droplet.error === 'droplet not configured' ? 'no droplet configured' : 'unreachable — best-effort probe'))}
        />}
      </div>
      <div className="row" style={{ marginTop: 14, gap: 10, alignItems: 'center' }}>
        <button
          className="primary"
          disabled={!!activeAction}
          onClick={updateWireguardImage}
        >
          {activeAction === 'wgimage' || activeAction === 'wireguard-image' ? 'Updating…' : 'Update WireGuard'}
        </button>
        <span className="hint" style={{ margin: 0 }}>Separate from “Update droplet &amp; cluster connector” — rolls cluster pods to the latest build and syncs the droplet’s tools to the same upstream source.</span>
      </div>
    </div>
  )

  if (loading && !result) return <div className="card"><span className="spin" /> Checking droplet…</div>
  if (!result?.configured) return <div className="card"><h2>Droplet Management</h2><p className="hint">Finish droplet setup before managing the host.</p></div>
  if (isReconnecting) return (
    <div className="card">
      <h2>Droplet Management</h2>
      <div className="notice" style={{ borderColor: 'var(--info, var(--border))' }}><span className="spin" style={{ marginRight: 8 }} /> <strong>Rebooting / reconnecting</strong> — the droplet is restarting. ProxyCTL will re-attach automatically when SSH responds again.</div>
      {actionMsg && <p className={actionMsg.ok ? 'ok' : 'hint'}>{actionMsg.pending && <span className="spin" />}{actionMsg.text}</p>}
      <button className="sm" onClick={load}>Check status now</button>
    </div>
  )
  if (!result?.reachable) return (
    <>
      <div className="card">
        <h2>Droplet Management</h2>
        <div className="notice bad"><strong>Droplet unreachable</strong> — {result?.error || 'SSH did not respond.'}</div>
        {visibleActionMsg && <p className={visibleActionMsg.ok ? 'ok' : 'hint'}>{visibleActionMsg.pending && <span className="spin" />}{visibleActionMsg.text}</p>}
        <button className="sm" disabled={loading} onClick={load}>{loading ? 'Checking…' : 'Try again'}</button>
      </div>
      {wireguardCard}
    </>
  )

  const s = result.status || {}
  return (
    <>
      {result.warning && (
        <div className="notice" style={{ marginBottom: 12 }}>{result.warning}</div>
      )}
      {s.rebootRequired && (
        <div className="notice" style={{ borderColor: 'var(--warn)', marginBottom: 16, display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
          <span><strong style={{ color: 'var(--warn)' }}>⚠ Reboot required</strong> — installed updates need a restart before they are fully active. Players on the tunnel will briefly disconnect.</span>
          <button className="sm danger" style={{ marginLeft: 'auto' }} disabled={!!activeAction} onClick={rebootDroplet}>{activeAction === 'reboot' ? 'Scheduling…' : 'Reboot now'}</button>
        </div>
      )}
      <div className="card">
        <div className="row" style={{ justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div>
            <h2 style={{ marginBottom: 4 }}>Droplet Management</h2>
            <strong>{s.hostname || result.ip}</strong>
            <div className="hint" style={{ margin: '3px 0 0' }}>{result.ip} · {s.os} · {s.kernel}</div>
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span className="hint" style={{ margin: 0 }}>live · refreshes every 15s</span>
            <button className="sm" disabled={loading} onClick={() => load(true)}>{loading ? 'Refreshing…' : 'Refresh'}</button>
          </div>
        </div>

        <div className="row" style={{ marginTop: 14, gap: 8 }}>
          <Service label="WireGuard" active={s.wireguardActive} />
          <Service label="SSH" active={s.sshActive} />
          <Service label="Automatic updates" active={s.unattendedActive} />
          {s.cloudflaredInstalled && <Service label="Optional native cloudflared · not managed" active={s.cloudflaredActive} />}
        </div>

        <div className="row" style={{ marginTop: 16, gap: 10, alignItems: 'stretch' }}>
          <Metric label="Uptime" value={fmtUptime(s.uptimeSeconds)} detail={s.wgVersion || 'WireGuard version unavailable'} />
          <Metric label="Cluster connector" value={result.clusterCloudflaredVersion || 'Bundled'} detail="Managed in Kubernetes · never installed on the droplet" />
          {s.cloudflaredInstalled && <Metric label="Optional native cloudflared" value={fmtCloudflaredVersion(s.cloudflaredVersion)} detail="Detected on droplet · not managed by ProxyCTL" />}
        </div>

        <DropletStatsCharts />

        <div className="row" style={{ marginTop: 18, gap: 10 }}>
          <button className="primary" disabled={!!activeAction} onClick={updateDroplet}>{activeAction === 'update' ? 'Updating…' : 'Update droplet & cluster connector'}</button>
          <button className="danger" disabled={!!activeAction} onClick={rebootDroplet}>{activeAction === 'reboot' ? 'Scheduling…' : 'Reboot droplet'}</button>
          <span className="hint" style={{ margin: 0 }}>{s.pendingUpdates || 0} pending package update{Number(s.pendingUpdates) === 1 ? '' : 's'}</span>
        </div>
        <div className="row" style={{ marginTop: 12, gap: 10, alignItems: 'center', flexWrap: 'wrap' }}>
          <label className="hint" style={{ margin: 0, whiteSpace: 'nowrap' }}>Auto-update droplet</label>
          <select
            aria-label="Auto-update droplet interval"
            value={autoUp?.everyDays ?? 0}
            disabled={!!activeAction}
            onChange={e => setAutoUpDays(+e.target.value)}
            style={{ maxWidth: 220 }}
          >
            <option value={0}>Off</option>
            <option value={1}>Every day</option>
            <option value={3}>Every 3 days</option>
            <option value={7}>Every 7 days</option>
            <option value={14}>Every 14 days</option>
            <option value={30}>Every 30 days</option>
          </select>
          <span className="hint" style={{ margin: 0 }}>
            {autoUp?.everyDays
              ? <>last run {fmtDay(autoUp.lastRun)} · next {fmtDay(autoUp.nextRun)} — runs this update automatically; a needed reboot still goes through the Reboot button</>
              : 'runs the droplet + WireGuard update automatically; a needed reboot is only ever scheduled by you'}
          </span>
        </div>
        {visibleActionMsg && <div className={visibleActionMsg.pending ? 'hint' : (visibleActionMsg.ok ? 'ok' : 'bad')} style={{ marginTop: 12 }}>{visibleActionMsg.pending && <span className="spin" />}{visibleActionMsg.text}</div>}
      </div>
      {wireguardCard}
    </>
  )
}