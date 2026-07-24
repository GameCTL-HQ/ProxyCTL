import { useEffect, useState } from 'react'
import { apiJSON } from './api.js'

// Latency/ping trend line — the companion to HeartbeatBar, matching how
// Uptime Kuma pairs a status heartbeat with a separate response-time
// chart below it. Unlike the heartbeat bar, this is a plain single-color
// trend line (per-sample up/down coloring is already the bar's job) — only
// samples with a real latency measurement plot; down samples and entries
// with no timing signal (Proxy Entries on UDP-only ports have no
// connect-time signal to measure) just don't appear rather than showing a
// misleading zero.
export default function LatencyLine({ kind, id, hours }) {
  const [data, setData] = useState(null)
  const [hover, setHover] = useState(null)

  useEffect(() => {
    let stop = false
    const load = () => apiJSON(`/api/${kind}/${id}/uptime`)
      .then(({ ok, data }) => { if (ok && !stop) setData(data) })
      .catch(() => {})
    load()
    const t = setInterval(load, 60000)
    return () => { stop = true; clearInterval(t) }
  }, [kind, id])

  if (!data?.available) return null

  const cutoff = Date.now() / 1000 - hours * 3600
  const pts = (data.history || []).filter(s => s.t >= cutoff && s.reachable && s.latencyMs > 0)

  if (pts.length < 2) {
    return (
      <p style={{ color: 'var(--muted)', fontSize: 12 }}>
        No latency data for this window — this entry may be UDP-only (no connect-time signal to measure).
      </p>
    )
  }

  const W = 640, H = 70, PAD = 4
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t || t0 + 1
  const vMax = Math.max(...pts.map(p => p.latencyMs)) * 1.15 || 1
  const x = (t) => PAD + (W - 2 * PAD) * (t - t0) / Math.max(1, t1 - t0)
  const y = (v) => H - PAD - (H - 2 * PAD) * (v / vMax)
  const path = pts.map((p, i) => `${i ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(p.latencyMs).toFixed(1)}`).join(' ')
  const area = `${path} L${x(t1).toFixed(1)},${H - PAD} L${x(t0).toFixed(1)},${H - PAD} Z`
  const avg = Math.round(pts.reduce((s, p) => s + p.latencyMs, 0) / pts.length)

  const fmtAxis = hours > 72
    ? (t) => new Date(t * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
    : hours > 6
      ? (t) => new Date(t * 1000).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })
      : (t) => new Date(t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  const axisTimes = Array.from({ length: 4 }, (_, i) => t0 + (t1 - t0) * (i / 3))

  const onMove = (e) => {
    const rect = e.currentTarget.getBoundingClientRect()
    const t = t0 + (t1 - t0) * Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width))
    let best = pts[0]
    for (const p of pts) if (Math.abs(p.t - t) < Math.abs(best.t - t)) best = p
    setHover(best)
  }

  return (
    <div>
      <p style={{ fontSize: 12, color: 'var(--muted)', margin: '0 0 6px' }}>
        Latency — avg <strong style={{ color: 'var(--link)' }}>{avg}ms</strong> over the last {hours}h
      </p>
      <div style={{ position: 'relative' }} onMouseMove={onMove} onMouseLeave={() => setHover(null)}>
        <svg viewBox={`0 0 ${W} ${H}`} style={{ width: '100%', height: 70, display: 'block' }}>
          <path d={area} fill="var(--link)" opacity="0.12" />
          <path d={path} fill="none" stroke="var(--link)" strokeWidth="1.5" strokeLinejoin="round" />
          {hover && (
            <>
              <line x1={x(hover.t)} x2={x(hover.t)} y1={PAD} y2={H - PAD} stroke="var(--border)" strokeWidth="1" />
              <circle cx={x(hover.t)} cy={y(hover.latencyMs)} r="3" fill="var(--link)" stroke="var(--panel)" strokeWidth="1.5" />
            </>
          )}
        </svg>
        {hover && (
          <div style={{
            position: 'absolute', top: -2, right: 0, fontSize: 10, padding: '2px 6px', borderRadius: 4,
            background: 'var(--panel-2)', border: '1px solid var(--border)', color: 'var(--text-2)', pointerEvents: 'none',
          }}>
            {Math.round(hover.latencyMs)}ms · {new Date(hover.t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
          </div>
        )}
      </div>
      <div style={{ display: 'flex', justifyContent: 'space-between', marginTop: 3 }}>
        {axisTimes.map((t, i) => (
          <span key={i} style={{ fontSize: 10, color: 'var(--muted-2)' }}>{fmtAxis(t)}</span>
        ))}
      </div>
    </div>
  )
}
