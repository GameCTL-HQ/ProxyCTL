import { useEffect, useState } from 'react'
import { apiJSON } from './api.js'

// Uptime-Kuma-style heartbeat bar: a row of evenly spaced bars, green when
// every sample in that time bucket was reachable, red when any of them
// weren't. Bucketing keeps it readable regardless of how many raw samples
// the window covers (24h @ 60s = 1440 samples — far too many individual
// bars to read) while still surfacing every outage as its own red bar,
// rather than one bad sample tinting the whole graph the way a single
// aggregate-colored line/area chart does.
const BAR_COUNT_COMPACT = 20
const BAR_COUNT_FULL = 50

export default function HeartbeatBar({ kind, id, hours, compact = false }) {
  const [data, setData] = useState(null)

  useEffect(() => {
    let stop = false
    const load = () => apiJSON(`/api/${kind}/${id}/uptime`)
      .then(({ ok, data }) => { if (ok && !stop) setData(data) })
      .catch(() => {})
    load()
    const t = setInterval(load, 60000)
    return () => { stop = true; clearInterval(t) }
  }, [kind, id])

  if (!data?.available) {
    return <span style={{ color: 'var(--muted-2)', fontSize: compact ? 10 : 12 }}>collecting…</span>
  }

  const maxBars = compact ? BAR_COUNT_COMPACT : BAR_COUNT_FULL
  const now = Date.now() / 1000
  const windowStart = now - hours * 3600
  const all = data.history || []
  // Anchor the visible span to the OLDEST sample actually on hand (never
  // earlier than the requested window) rather than always spanning the
  // full requested duration — otherwise, with less history than the
  // selected window, real bars get squeezed against the right edge behind
  // a wall of empty placeholders. This way bars fill in from the left as
  // history accumulates, and once there's a full window of data it's a
  // no-op (earliest sample <= windowStart already).
  const earliest = all.length ? all[0].t : now
  const start = Math.max(windowStart, Math.min(earliest, now - 60))
  const span = Math.max(60, now - start)
  // Never make a bucket finer than the real ~60s sample cadence — with a
  // short/sparse history, dividing into a fixed 50 buckets regardless
  // produces far more buckets than there is data, most of them empty and
  // invisible against the background, which looks like a handful of tiny
  // squares scattered across a mostly-dead row. Bucket size floors at the
  // sample interval instead, so bar count (and therefore bar width) scales
  // down to match how much data actually exists — one bar per real sample
  // when history is sparse, up to maxBars once there's enough of it.
  const bucketSize = Math.max(60, span / maxBars)
  const barCount = Math.max(1, Math.min(maxBars, Math.ceil(span / bucketSize)))

  const buckets = Array.from({ length: barCount }, (_, i) => {
    const bStart = start + i * bucketSize
    const bEnd = bStart + bucketSize
    const samples = all.filter(s => s.t >= bStart && s.t < bEnd)
    if (samples.length === 0) return { state: 'empty', start: bStart, end: bEnd, samples: [] }
    const down = samples.filter(s => !s.reachable).length
    return { state: down > 0 ? 'down' : 'up', start: bStart, end: bEnd, samples, down }
  })

  const known = all.filter(s => s.t >= start)
  const pct = known.length ? Math.round(100 * known.filter(s => s.reachable).length / known.length) : null
  const pctColor = pct === null ? 'var(--muted)' : pct === 100 ? 'var(--ok-tx)' : pct >= 95 ? 'var(--warn-tx)' : 'var(--err-tx)'
  const colorFor = (state) => state === 'up' ? 'var(--ok)' : state === 'down' ? 'var(--err)' : 'var(--border-2)'

  const fmtTime = (t) => new Date(t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  // Longer spans need more than a bare time on the axis: >6h can cross
  // into "yesterday" (add the weekday), and >3d can repeat a weekday
  // (add the date instead).
  const fmtAxis = hours > 72
    ? (t) => new Date(t * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
    : hours > 6
      ? (t) => new Date(t * 1000).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })
      : fmtTime

  // Always-visible time axis (not just on hover) — 2 labels compact
  // (start/now), 4 spread across the full view; back down to 2 when only
  // a narrow strip of bars exists for them to sit under.
  const axisCount = compact || barCount < 12 ? 2 : 4
  const axisTimes = Array.from({ length: axisCount }, (_, i) => start + (now - start) * (i / (axisCount - 1)))

  // Cap each bar's width so a handful of bars (short history) renders as a
  // neat left-aligned strip instead of a few page-wide slabs; with a full
  // window of buckets the cap is above the flex-computed width, so the row
  // still fills edge to edge. The axis lives inside the same width-capped
  // wrapper so its labels always line up with the bars, not the page.
  const barW = compact ? 6 : 12
  const gap = compact ? 1 : 2
  const stripWidth = `min(100%, ${barCount * (barW + gap)}px)`

  return (
    <div>
      {!compact && (
        <p style={{ fontSize: 12, color: 'var(--muted)', margin: '0 0 6px' }}>
          {pct !== null ? <><strong style={{ color: pctColor }}>{pct}%</strong> reachable</> : 'no samples yet'}
        </p>
      )}
      <div style={{ width: stripWidth }}>
        <div style={{ display: 'flex', gap, alignItems: 'stretch', height: compact ? 16 : 32 }}>
          {buckets.map((b, i) => (
            <div
              key={i}
              title={`${fmtTime(b.start)}–${fmtTime(b.end)}: ${b.state === 'empty' ? 'no samples' : b.state === 'up' ? 'all checks reachable' : `${b.down}/${b.samples.length} checks unreachable`}`}
              style={{
                flex: 1,
                borderRadius: 2,
                background: colorFor(b.state),
                opacity: b.state === 'empty' ? 0.5 : 1,
              }}
            />
          ))}
        </div>
        <div style={{ display: 'flex', justifyContent: 'space-between', gap: 10, marginTop: 3 }}>
          {axisTimes.map((t, i) => (
            <span key={i} style={{ fontSize: compact ? 8 : 10, color: 'var(--muted-2)', whiteSpace: 'nowrap' }}>{fmtAxis(t)}</span>
          ))}
        </div>
      </div>
    </div>
  )
}
