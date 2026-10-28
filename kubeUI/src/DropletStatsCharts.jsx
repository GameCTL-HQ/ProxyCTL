import { useEffect, useMemo, useState } from 'react'
import { apiJSON } from './api.js'

const RANGES = [
  { label: '1h', hours: 1 },
  { label: '3h', hours: 3 },
  { label: '6h', hours: 6 },
  { label: '12h', hours: 12 },
  { label: '24h', hours: 24 },
  { label: '2d', hours: 48 },
  { label: '3d', hours: 72 },
]

// One line per metric, same visual language as LatencyLine (area fill +
// line + hover crosshair + time axis). Percentages use a fixed 0-100
// scale so jumps are honest; Disk / is min-max scaled because a disk
// sitting at 75% would otherwise render as a flat line near the top.
const METRICS = [
  { key: 'cpuPct', label: 'CPU', unit: '%', min: 0, max: 100, color: 'var(--accent)' },
  { key: 'memPct', label: 'RAM', unit: '%', min: 0, max: 100, color: 'var(--ok)' },
  { key: 'diskPct', label: 'Disk /', unit: '%', min: null, max: null, color: 'var(--warn)' },
]

// segmentsSplits reachable samples into consecutive runs so that
// unreachable gaps (droplet offline / sampling skipped) render as breaks
// in the line instead of a misleading bridge.
function segments(pts) {
  const runs = []
  let cur = []
  let prev = null
  for (const p of pts) {
    if (!p.reachable) {
      if (cur.length) runs.push(cur)
      cur = []
      prev = null
      continue
    }
    // a run also breaks when samples are >4x apart in time (restart gap
    // where the ring simply has no points)
    if (prev && p.t - prev > 240) {
      if (cur.length) runs.push(cur)
      cur = []
    }
    cur.push(p)
    prev = p.t
  }
  if (cur.length) runs.push(cur)
  return runs
}

function TrendChart({ label, pts, color, min, max, unit, t0, t1, avg }) {
  const [hover, setHover] = useState(null)
  const W = 640, H = 70, PAD = 4

  const yMin = min == null ? Math.min(...pts.map(p => p.v)) : min
  const yMax = max == null ? Math.max(...pts.map(p => p.v)) : max
  const span = Math.max(1, yMax - yMin)
  const yPad = max == null ? span * 0.15 : 0
  const lo = min == null ? Math.max(0, yMin - yPad) : yMin
  const hi = max == null ? yMax + yPad : yMax
  const x = (t) => PAD + (W - 2 * PAD) * (t - t0) / Math.max(1, t1 - t0)
  const y = (v) => H - PAD - (H - 2 * PAD) * ((v - lo) / Math.max(0.001, hi - lo))

  const runs = segments(pts)
  const paths = runs.map(run =>
    run.map((p, i) => `${i ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(' '))
  const areas = runs.filter(run => run.length > 1).map(run => {
    const line = run.map((p, i) => `${i ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(' ')
    return `${line} L${x(run[run.length - 1].t).toFixed(1)},${H - PAD} L${x(run[0].t).toFixed(1)},${H - PAD} Z`
  })

  const fmtAxis = (t1 - t0) > 48 * 3600
    ? (t) => new Date(t * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
    : (t1 - t0) > 6 * 3600
      ? (t) => new Date(t * 1000).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })
      : (t) => new Date(t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  const axisTimes = Array.from({ length: 4 }, (_, i) => t0 + (t1 - t0) * (i / 3))

  const onMove = (e) => {
    const rect = e.currentTarget.getBoundingClientRect()
    const t = t0 + (t1 - t0) * Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width))
    let best = null
    for (const p of pts) if (!p.reachable) continue
    if (best === null || Math.abs(p.t - t) < Math.abs(best.t - t)) best = p
    setHover(best)
  }

  return (
    <div>
      <p style={{ fontSize: 12, color: 'var(--muted)', margin: '0 0 6px' }}>
        {label} — now <strong style={{ color }}>{pts.length ? `${(pts[pts.length - 1].v).toFixed(1)}${unit}` : '—'}</strong>
        {avg != null && <span> · avg <strong style={{ color }}>{avg}{unit}</strong></span>}
      </p>
      <div style={{ position: 'relative' }} onMouseMove={onMove} onMouseLeave={() => setHover(null)}>
        <svg viewBox={`0 0 ${W} ${H}`} style={{ width: '100%', height: 70, display: 'block' }}>
          {areas.map((d, i) => <path key={`a${i}`} d={d} fill={color} opacity="0.12" />)}
          {paths.map((d, i) => (
            <path key={`p${i}`} d={d} fill="none" stroke={color} strokeWidth="1.5" strokeLinejoin="round" />
          ))}
          {hover && (
            <>
              <line x1={x(hover.t)} x2={x(hover.t)} y1={PAD} y2={H - PAD} stroke="var(--border)" strokeWidth="1" />
              <circle cx={x(hover.t)} cy={y(hover.v)} r="3" fill={color} stroke="var(--panel)" strokeWidth="1.5" />
            </>
          )}
        </svg>
        {hover && (
          <div style={{
            position: 'absolute', top: -2, right: 0, fontSize: 10, padding: '2px 6px', borderRadius: 4,
            background: 'var(--panel-2)', border: '1px solid var(--border)', color: 'var(--text-2)', pointerEvents: 'none',
          }}>
            {hover.v.toFixed(1)}{unit} · {new Date(hover.t * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}
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

// DropletStatsCharts — over-time resource graphs for the droplet (CPU /
// RAM / Disk), fed by the 60s background sampler. The current-value cards
// above refresh on their own 15s poll; these charts refetch the ring
// every 30s and filter the selected window client-side.
export default function DropletStatsCharts() {
  const [data, setData] = useState(null)
  const [hours, setHours] = useState(24)

  useEffect(() => {
    let stop = false
    const load = () => apiJSON('/api/droplet/stats/history')
      .then(({ ok, data }) => { if (ok && !stop) setData(data) })
      .catch(() => {})
    load()
    const t = setInterval(load, 30000)
    return () => { stop = true; clearInterval(t) }
  }, [])

  const now = Date.now() / 1000
  const t0 = now - hours * 3600
  const inWindow = useMemo(() => {
    if (!data?.samples?.length) return []
    return data.samples.filter(s => s.t >= t0)
  }, [data, t0])
  const withData = inWindow.filter(s => s.reachable)

  return (
    <div style={{ borderTop: '1px solid var(--border-2)', marginTop: 16, paddingTop: 14 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 10 }}>
        <span style={{ fontSize: 13, color: 'var(--muted)' }}>
          Resources over time <span style={{ color: 'var(--muted-2)' }}>· sampled every 60s, keeps 7 days</span>
        </span>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 4 }}>
          {RANGES.map(d => (
            <button
              key={d.hours}
              className="sm"
              aria-label={`Show last ${d.label}`}
              style={hours === d.hours ? { background: 'var(--accent-h)', borderColor: 'var(--accent-h)', color: '#fff' } : {}}
              onClick={() => setHours(d.hours)}
            >
              {d.label}
            </button>
          ))}
        </div>
      </div>
      {withData.length < 2 ? (
        <p style={{ color: 'var(--muted)', fontSize: 12 }}>
          Collecting samples — the background sampler records every 60s, so this graph fills in over the
          next few minutes. Current values above are already live (refreshed every 15s).
        </p>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          {METRICS.map(m => {
            const pts = inWindow.map(s => ({ t: s.t, reachable: s.reachable, v: s[m.key] || 0 }))
            const vals = withData.map(s => s[m.key] || 0)
            const avg = vals.length ? vals.reduce((a, b) => a + b, 0) / vals.length : null
            return <TrendChart key={m.key} label={m.label} unit={m.unit} color={m.color} min={m.min} max={m.max} pts={pts} t0={t0} t1={now} avg={avg != null ? avg.toFixed(1) : null} />
          })}
        </div>
      )}
    </div>
  )
}
