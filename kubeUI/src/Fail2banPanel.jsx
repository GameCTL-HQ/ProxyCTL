import { useCallback, useEffect, useState } from 'react'
import { apiJSON, postJSON } from './api.js'

// Persistent SSH-security dashboard in the main app — not just buried in
// the Setup wizard. The point: port 22 stays reachable (even restricted to
// the control tunnel, sshd itself is still listening on every interface),
// so this is the ongoing reassurance that it's actively watched — live
// fail2ban counters + a scrollable banned-IP list, right where the
// operator already looks.
//
// Ban policy is deliberately a single tier, no escalation: this box's real
// traffic is ProxyCTL's own control tunnel and authenticated
// personal-access devices, so anyone reaching sshd from outside that and
// failing repeatedly isn't legitimate — bans are permanent from the first
// offense (see server/fail2ban.go), always revertible with "Allow back in".
//
// Refreshes on mount and every REFRESH_MS while mounted, plus a manual
// Refresh button. Each refresh is a handful of real SSH commands to the
// droplet (see fail2banStatus in server/fail2ban.go) — deliberately not a
// tight poll loop, just often enough to feel current.
const REFRESH_MS = 120000

const locOf = (r) => [r.city, r.country].filter(Boolean).join(', ') || '—'

const PAGE_SIZE = 8

// Page-number tabs at the bottom of a grid — only rendered once there's
// more than one page. Full numbered tabs up to a point; beyond that, a
// plain "page X of Y" so this can't ever render an unbounded row of tabs
// (permanent bans only ever accumulate, never expire on their own, so
// these lists can genuinely grow over months).
function Pager({ page, pageCount, onChange }) {
  if (pageCount <= 1) return null
  return (
    <div style={{ display: 'flex', gap: 4, justifyContent: 'center', alignItems: 'center', marginTop: 8, flexWrap: 'wrap' }}>
      <button className="sm" disabled={page === 0} onClick={() => onChange(page - 1)}>‹ Prev</button>
      {pageCount <= 12 ? (
        Array.from({ length: pageCount }, (_, i) => (
          <button key={i} className={'sm' + (i === page ? ' primary' : '')} onClick={() => onChange(i)}>{i + 1}</button>
        ))
      ) : (
        <span className="hint" style={{ margin: 0 }}>Page {page + 1} of {pageCount}</span>
      )}
      <button className="sm" disabled={page === pageCount - 1} onClick={() => onChange(page + 1)}>Next ›</button>
    </div>
  )
}

// Clamps to a valid page whenever the underlying row count changes (e.g.
// after a ban/unban shrinks or grows the list) instead of getting stuck
// on a now out-of-range page.
function usePage(rowCount) {
  const [page, setPage] = useState(0)
  const pageCount = Math.max(1, Math.ceil(rowCount / PAGE_SIZE))
  const clamped = Math.min(page, pageCount - 1)
  return [clamped, setPage, pageCount]
}

// A paginated grid of banned IPs — all permanent, so no "until" column.
function BanTable({ rows, unbanBusyIP, onUnban }) {
  const [page, setPage, pageCount] = usePage(rows.length)
  const shown = rows.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE)
  return (
    <div style={{ border: '1px solid var(--border)', borderRadius: 8 }}>
      <table className="mono" style={{ width: '100%', fontSize: 12.5, borderCollapse: 'collapse' }}>
        <thead>
          <tr style={{ textAlign: 'left', opacity: 0.7, background: 'var(--panel-soft)' }}>
            <th style={{ padding: '6px 8px' }}>IP</th>
            <th style={{ padding: '6px 8px' }}>Location</th>
            <th style={{ padding: '6px 8px' }}></th>
          </tr>
        </thead>
        <tbody>
          {shown.map(b => (
            <tr key={b.ip}>
              <td style={{ padding: '6px 8px' }}>{b.ip}</td>
              <td style={{ padding: '6px 8px' }}>{locOf(b)}</td>
              <td style={{ padding: '6px 8px' }}>
                <button className="sm" disabled={unbanBusyIP === b.ip} onClick={() => onUnban(b.ip)}>
                  {unbanBusyIP === b.ip ? 'un-banning…' : 'Allow back in'}
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <Pager page={page} pageCount={pageCount} onChange={setPage} />
    </div>
  )
}

// Paginated grid of not-yet-banned attempt counts.
function AttemptsTable({ rows, maxRetry }) {
  const [page, setPage, pageCount] = usePage(rows.length)
  const shown = rows.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE)
  return (
    <div style={{ border: '1px solid var(--border)', borderRadius: 8 }}>
      <table className="mono" style={{ width: '100%', fontSize: 12.5, borderCollapse: 'collapse' }}>
        <thead>
          <tr style={{ textAlign: 'left', opacity: 0.7, background: 'var(--panel-soft)' }}>
            <th style={{ padding: '6px 8px' }}>IP</th>
            <th style={{ padding: '6px 8px' }}>Location</th>
            <th style={{ padding: '6px 8px' }}>Attempts</th>
          </tr>
        </thead>
        <tbody>
          {shown.map(a => (
            <tr key={a.ip}>
              <td style={{ padding: '6px 8px' }}>{a.ip}</td>
              <td style={{ padding: '6px 8px' }}>{locOf(a)}</td>
              <td style={{ padding: '6px 8px' }}>{a.attempts} / {maxRetry}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <Pager page={page} pageCount={pageCount} onChange={setPage} />
    </div>
  )
}

// Paginated grid of all-time ban/unban events.
function HistoryTable({ rows }) {
  const [page, setPage, pageCount] = usePage(rows.length)
  const shown = rows.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE)
  return (
    <div style={{ border: '1px solid var(--border)', borderRadius: 8, marginTop: 8 }}>
      <table className="mono" style={{ width: '100%', fontSize: 12.5, borderCollapse: 'collapse' }}>
        <thead>
          <tr style={{ textAlign: 'left', opacity: 0.7, background: 'var(--panel-soft)' }}>
            <th style={{ padding: '6px 8px' }}>When</th>
            <th style={{ padding: '6px 8px' }}>IP</th>
            <th style={{ padding: '6px 8px' }}>Location</th>
            <th style={{ padding: '6px 8px' }}>Action</th>
          </tr>
        </thead>
        <tbody>
          {shown.map((e, i) => (
            <tr key={i}>
              <td style={{ padding: '6px 8px' }}>{e.at ? new Date(e.at * 1000).toLocaleString() : '—'}</td>
              <td style={{ padding: '6px 8px' }}>{e.ip}</td>
              <td style={{ padding: '6px 8px' }}>{locOf(e)}</td>
              <td style={{ padding: '6px 8px' }}><span className={e.action === 'ban' ? 'bad' : 'ok'}>{e.action}</span></td>
            </tr>
          ))}
        </tbody>
      </table>
      <Pager page={page} pageCount={pageCount} onChange={setPage} />
    </div>
  )
}

export default function Fail2banPanel({ onOpenSetup }) {
  const [droplet, setDroplet] = useState(null)
  const [f2b, setF2b] = useState(null)
  const [loading, setLoading] = useState(false)
  const [unbanBusyIP, setUnbanBusyIP] = useState('')
  const [banInput, setBanInput] = useState('')
  const [banBusy, setBanBusy] = useState(false)
  const [banMsg, setBanMsg] = useState(null)

  const load = useCallback(async () => {
    const { ok: dOk, data: d } = await apiJSON('/api/droplet').catch(() => ({ ok: false }))
    if (dOk) setDroplet(d)
    if (!dOk || !d?.configured) return
    setLoading(true)
    const { ok, data } = await apiJSON('/api/droplet/fail2ban').catch(() => ({ ok: false }))
    if (ok) setF2b(data)
    setLoading(false)
  }, [])

  useEffect(() => {
    load()
    const id = setInterval(load, REFRESH_MS)
    return () => clearInterval(id)
  }, [load])

  const unbanIP = async (ip) => {
    setUnbanBusyIP(ip)
    await postJSON('/api/droplet/fail2ban/unban', { ip }).catch(() => {})
    await load()
    setUnbanBusyIP('')
  }

  const banIP = async () => {
    const ip = banInput.trim()
    if (!ip) return
    setBanBusy(true)
    setBanMsg({ pending: true })
    try {
      const { data: r } = await postJSON('/api/droplet/fail2ban/ban', { ip })
      setBanMsg({ ok: !!r.ok, text: r.ok ? `✓ ${ip} banned` : '✗ ' + (r.error || 'ban failed') })
      if (r.ok) { setBanInput(''); await load() }
    } finally { setBanBusy(false) }
  }

  if (!droplet?.configured) return null // nothing to show before the droplet exists
  // Tunnel-only lockdown firewalls port 22 shut (PROXYCTL-SSH DROP gate, see
  // render.go) before packets ever reach sshd — fail2ban watches sshd's own
  // auth-failure log, so it has nothing to see and nothing useful to do here.
  // Only relevant when SSH stays reachable by IP (public-IP allow-list, or
  // no lockdown at all) — reappears automatically if the operator switches
  // away from tunnel-only.
  if (droplet.sshTunnelOnly) return null

  const banned = f2b?.banned || []
  const recentAttempts = f2b?.recentAttempts || []
  const banHistory = f2b?.banHistory || []

  // This panel only ever renders when SSH is NOT tunnel-only (see the
  // sshTunnelOnly guard above) — meaning port 22 is genuinely reachable by
  // IP, either restricted to an allow-list (Option B) or wide open (no
  // lockdown chosen yet). Either way, unlike tunnel-only mode, real traffic
  // reaches sshd here — fail2ban is actually doing the banning, not sitting
  // idle behind a firewall gate that already dropped everything.
  const hintText = droplet.sshLockedDown
    ? <><strong>Real, active defense</strong> — SSH is restricted to your allow-listed public IPs at the sshd layer, but anyone can still reach sshd and try; fail2ban is what actually bans repeat failures.</>
    : <><strong>Primary defense right now</strong> — no IP restriction is set up, so SSH is reachable from the whole internet; fail2ban (plus pubkey-only auth) is what's actually keeping it locked down.</>

  return (
    <div className="card" id="securityCard" style={{ marginBottom: 18 }}>
      <div className="notice" style={{ marginTop: 0, marginBottom: 14, fontSize: 13 }}>{hintText}</div>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h3 style={{ margin: 0 }}>
          SSH fail2ban security{' '}
          {f2b?.installed && (
            <span className={f2b.active ? 'ok' : 'bad'} style={{ fontSize: 13, fontWeight: 400 }}>
              · {f2b.active ? 'active' : 'installed, not active'}
            </span>
          )}
        </h3>
        <button className="sm" disabled={loading} onClick={load}>{loading ? 'Refreshing…' : 'Refresh'}</button>
      </div>

      {!f2b ? (
        // Haven't heard back from the droplet yet (first mount, or a
        // manual Refresh) — say nothing definite rather than flashing the
        // "not installed" message before we actually know either way.
        <p className="hint" style={{ margin: '10px 0 0' }}>{loading ? 'Checking…' : ''}</p>
      ) : !f2b.installed ? (
        <p className="sub" style={{ margin: '10px 0 0' }}>
          Port 22 is reachable from the internet with no brute-force protection watching it yet.{' '}
          <button className="sm" onClick={() => onOpenSetup?.('lockdown')}>Set up fail2ban</button>
        </p>
      ) : (
        <>
          <p className="sub" style={{ margin: '10px 0' }}>
            Bans after <strong>{f2b.maxRetry}</strong> failed attempts within <strong>{f2b.findTime}</strong> — <strong>permanently</strong>,
            first offense, no temporary grace period. {droplet.sshLockedDown
              ? <>SSH is restricted to your allow-listed public IPs, not the WireGuard tunnel — so this is genuinely banning whoever still reaches sshd and fails.</>
              : <>SSH isn't restricted to any IP list yet — so this is genuinely banning whoever tries and fails, from anywhere on the internet.</>}{' '}
            Every ban is still just one click to reverse below if it's ever wrong.
          </p>
          <div className="row" style={{ gap: 18, fontSize: 13, opacity: 0.85, marginBottom: 14 }}>
            <span>Banned now: <strong>{banned.length}</strong></span>
            <span>Failed attempts: <strong>{f2b.currentlyFailed ?? 0}</strong> right now ({f2b.totalFailed ?? 0} total, {f2b.totalBanned ?? 0} bans lifetime)</span>
          </div>

          <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 6 }}>Banned IPs</div>
          {banned.length > 0 ? (
            <BanTable rows={banned} unbanBusyIP={unbanBusyIP} onUnban={unbanIP} />
          ) : (
            <p className="hint" style={{ margin: '0 0 14px' }}>No bans currently in effect.</p>
          )}
          <div style={{ marginTop: 10, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
            <input value={banInput} onChange={e => setBanInput(e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); banIP() } }}
              placeholder="Ban an IP or CIDR manually (e.g. 203.0.113.42)" style={{ maxWidth: 260 }} />
            <button className="sm" disabled={banBusy || !banInput.trim()} onClick={banIP}>Ban</button>
            <span className="hint" style={{ margin: 0 }}>
              {banMsg?.pending && <><span className="spin" />banning…</>}
              {banMsg && !banMsg.pending && <span className={banMsg.ok ? 'ok' : 'bad'}>{banMsg.text}</span>}
            </span>
          </div>

          <div style={{ fontSize: 13, fontWeight: 600, margin: '16px 0 6px' }}>
            Recent attempts <span style={{ fontWeight: 400, opacity: .7 }}>(not yet banned, last {f2b.findTime})</span>
          </div>
          {recentAttempts.length > 0 ? (
            <AttemptsTable rows={recentAttempts} maxRetry={f2b.maxRetry} />
          ) : (
            <p className="hint" style={{ margin: 0 }}>No failed attempts in the last {f2b.findTime}.</p>
          )}

          <details style={{ marginTop: 16 }}>
            <summary className="hint" style={{ cursor: 'pointer', fontSize: 13, fontWeight: 600, color: 'var(--text)' }}>
              Ban history — all time ({banHistory.filter(e => e.action === 'ban').length} bans logged)
            </summary>
            {banHistory.length > 0 ? (
              <HistoryTable rows={banHistory} />
            ) : (
              <p className="hint" style={{ margin: '8px 0 0' }}>No ban history in the current log.</p>
            )}
          </details>
        </>
      )}
    </div>
  )
}
