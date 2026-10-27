import { useCallback, useEffect, useRef, useState } from 'react'
import { apiJSON, postJSON } from './api.js'
import { useUI } from './ui.jsx'

function fmtBytes(n) {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = Number(n) || 0
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++ }
  return `${parseFloat(value.toFixed(unit ? 1 : 0))} ${units[unit]}`
}

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
  const seq = useRef(0)
  const timers = useRef([])

  const loadWg = useCallback(async () => {
    try {
      const { ok, data } = await apiJSON('/api/wireguard/image')
      if (ok && data) setWg(data)
    } catch { /* best-effort: keep previous wireguard status */ }
  }, [])

  const load = useCallback(async () => {
    const my = ++seq.current
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
      if (my === seq.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
    loadWg()
    const timer = setInterval(load, 30000)
    const wgTimer = setInterval(loadWg, 300000)
    return () => {
      seq.current++
      clearInterval(timer)
      clearInterval(wgTimer)
      timers.current.forEach(clearTimeout)
      timers.current = []
    }
  }, [load, loadWg])

  const later = useCallback((fn, ms) => {
    const id = setTimeout(fn, ms)
    timers.current.push(id)
    return id
  }, [])

  const updateDroplet = async () => {
    if (!await ask(
      'Update the droplet and Cloudflare connectors?<br/><br/>This installs available OS/WireGuard packages, updates the droplet cloudflared binary to its newest stable release, and rolls the cluster connector to ProxyCTL’s reviewed digest. Existing WireGuard and Cloudflare configuration is preserved. ProxyCTL will <strong>not reboot automatically</strong>.',
      { ok: 'Update all' })) return
    setAction('update')
    setActionMsg({ pending: true, text: 'Updating droplet packages and Cloudflare connectors… this can take several minutes.' })
    try {
      const { ok, data } = await postJSON('/api/droplet/bootstrap', { upgrade: true })
      const success = ok && !!data?.ok
      setActionMsg({ ok: success, text: success ? 'Droplet and Cloudflare updates completed. Refreshing status…' : (data?.error || data?.stderr || 'Maintenance update failed.') })
      if (success) toast('✓ Droplet and Cloudflare updates completed')
      await load()
    } catch (err) {
      setActionMsg({ ok: false, text: 'Droplet update failed: ' + String(err) })
    } finally {
      setAction('')
    }
  }

  const updateWireguardImage = async () => {
    const target = wg?.upstreamRef || wg?.currentDigest
    const sourceNote = wg?.upstreamRef
      ? 'the live upstream <code>latest</code> build'
      : (wg?.lookupError ? 'the embedded reviewed digest (GHCR check failed)' : 'the embedded reviewed digest')
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
      await loadWg()
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
        later(() => { load() }, 10000)
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
        <Service label="Upstream check" active={!!wg?.upstreamRef} />
      </div>
      <div className="row" style={{ marginTop: 12, gap: 10, alignItems: 'stretch' }}>
        <Metric label="Current digest" value={shortDigest(wg?.currentDigest)} detail="reviewed, embedded in ProxyCTL" />
        <Metric
          label="Upstream latest"
          value={wg?.upstreamRef ? shortDigest(wg.upstreamRef) : (wg?.lookupError ? 'Unreachable' : 'Checking…')}
          detail={wg?.updateAvailable ? 'update applies this build' : (wg?.upstreamRef ? 'already current' : (wg?.lookupError ? 'best-effort check failed; update falls back to embedded digest' : ''))}
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
          disabled={!!action || result?.busy}
          onClick={updateWireguardImage}
        >
          {action === 'wgimage' ? 'Updating…' : 'Update WireGuard'}
        </button>
        <span className="hint" style={{ margin: 0 }}>Separate from “Update droplet &amp; Cloudflare” — rolls cluster pods to the latest build and syncs the droplet’s tools to the same upstream source.</span>
      </div>
    </div>
  )

  if (loading && !result) return <div className="card"><span className="spin" /> Checking droplet…</div>
  if (!result?.configured) return <div className="card"><h2>Droplet Management</h2><p className="hint">Finish droplet setup before managing the host.</p></div>
  if (reconnecting) return (
    <div className="card">
      <h2>Droplet Management</h2>
      <div className="notice" style={{ borderColor: 'var(--info, var(--border))' }}><span className="spin" style={{ marginRight: 8 }} /> <strong>Rebooting / reconnecting</strong> — the droplet is restarting. ProxyCTL will re-attach automatically when SSH responds again.</div>
      {actionMsg && <p className={actionMsg.ok ? 'ok' : 'hint'}>{actionMsg.text}</p>}
      <button className="sm" onClick={load}>Check status now</button>
    </div>
  )
  if (!result?.reachable) return (
    <>
      <div className="card">
        <h2>Droplet Management</h2>
        <div className="notice bad"><strong>Droplet unreachable</strong> — {result?.error || 'SSH did not respond.'}</div>
        {actionMsg && <p className={actionMsg.ok ? 'ok' : 'hint'}>{actionMsg.text}</p>}
        <button className="sm" disabled={loading} onClick={load}>{loading ? 'Checking…' : 'Try again'}</button>
      </div>
      {wireguardCard}
    </>
  )

  const s = result.status || {}
  const memPct = s.memoryTotal ? (s.memoryUsed / s.memoryTotal * 100).toFixed(1) : '0.0'
  const diskPct = s.diskTotal ? (s.diskUsed / s.diskTotal * 100).toFixed(1) : '0.0'
  return (
    <>
      {result.warning && (
        <div className="notice" style={{ marginBottom: 12 }}>{result.warning}</div>
      )}
      {s.rebootRequired && (
        <div className="notice" style={{ borderColor: 'var(--warn)', marginBottom: 16 }}>
          <strong style={{ color: 'var(--warn)' }}>⚠ Reboot required</strong> — installed updates need a restart before they are fully active.
        </div>
      )}
      <div className="card">
        <div className="row" style={{ justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div>
            <h2 style={{ marginBottom: 4 }}>Droplet Management</h2>
            <strong>{s.hostname || result.ip}</strong>
            <div className="hint" style={{ margin: '3px 0 0' }}>{result.ip} · {s.os} · {s.kernel}</div>
          </div>
          <button className="sm" disabled={loading} onClick={load}>{loading ? 'Refreshing…' : 'Refresh'}</button>
        </div>

        <div className="row" style={{ marginTop: 14, gap: 8 }}>
          <Service label="WireGuard" active={s.wireguardActive} />
          <Service label="SSH" active={s.sshActive} />
          <Service label="Automatic updates" active={s.unattendedActive} />
          {s.cloudflaredInstalled
            ? <Service label="Native cloudflared" active={s.cloudflaredActive} />
            : <span className="pill" style={{ marginRight: 8 }}>○ Native cloudflared · Not installed</span>}
        </div>

        <div className="row" style={{ marginTop: 16, gap: 10, alignItems: 'stretch' }}>
          <Metric label="CPU" value={`${s.cpuUsagePct ?? 0}%`} detail={`${s.cpuCores || 0} cores · load ${s.load1 ?? 0} / ${s.load5 ?? 0} / ${s.load15 ?? 0}`} />
          <Metric label="RAM" value={`${fmtBytes(s.memoryUsed)} / ${fmtBytes(s.memoryTotal)}`} detail={`${memPct}% used · ${fmtBytes(s.memoryAvailable)} available`} />
          <Metric label="Disk /" value={`${fmtBytes(s.diskUsed)} / ${fmtBytes(s.diskTotal)}`} detail={`${diskPct}% used · ${fmtBytes(s.diskAvailable)} free`} />
          <Metric label="Uptime" value={fmtUptime(s.uptimeSeconds)} detail={s.wgVersion || 'WireGuard version unavailable'} />
          <Metric label="Cloudflare" value={s.cloudflaredInstalled ? fmtCloudflaredVersion(s.cloudflaredVersion) : 'Not installed'} detail={`Cluster connector target ${result.clusterCloudflaredVersion || 'bundled with ProxyCTL'}`} />
        </div>

        <div className="row" style={{ marginTop: 18, gap: 10 }}>
          <button className="primary" disabled={!!action || result.busy} onClick={updateDroplet}>{action === 'update' ? 'Updating…' : 'Update droplet & Cloudflare'}</button>
          <button className="danger" disabled={!!action || result.busy} onClick={rebootDroplet}>{action === 'reboot' ? 'Scheduling…' : 'Reboot droplet'}</button>
          <span className="hint" style={{ margin: 0 }}>{s.pendingUpdates || 0} pending package update{Number(s.pendingUpdates) === 1 ? '' : 's'}</span>
        </div>
        {actionMsg && <div className={actionMsg.pending ? 'hint' : (actionMsg.ok ? 'ok' : 'bad')} style={{ marginTop: 12 }}>{actionMsg.pending && <span className="spin" />}{actionMsg.text}</div>}
      </div>
      {wireguardCard}
    </>
  )
}