import { useCallback, useEffect, useState } from 'react'
import { apiJSON, postJSON } from './api.js'
import { useUI } from './ui.jsx'

// Persistent SSH-security dashboard in the main app — not just buried in
// the Setup wizard. Shown in EVERY lockdown mode, tunnel-only included:
// the firewall gate only stops OUTSIDE traffic — tunnel-connected devices
// do reach sshd, so a peer with tunnel access but a wrong/missing SSH key
// (a device mid key-setup, or a stolen tunnel config without the key)
// racks up real failures and gets banned. Learned the hard way: the panel
// used to hide under tunnel-only, right when the operator banned their
// own device testing a new key and had no GUI to undo it.
//
// Ban policy is a single tier, no escalation — permanent from the first
// offense by default, tunable from this panel (see effectiveF2BPolicy in
// server/fail2ban.go). Always revertible with "Allow back in", and the
// whole service can be switched off here too.
//
// Refreshes on mount and every REFRESH_MS while mounted, plus a manual
// Refresh button. Each refresh is a handful of real SSH commands to the
// droplet (see fail2banStatus in server/main.go) — deliberately not a
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
  const { ask, toast } = useUI()
  const [droplet, setDroplet] = useState(null)
  const [f2b, setF2b] = useState(null)
  const [loading, setLoading] = useState(false)
  const [unbanBusyIP, setUnbanBusyIP] = useState('')
  const [banInput, setBanInput] = useState('')
  const [banBusy, setBanBusy] = useState(false)
  const [banMsg, setBanMsg] = useState(null)
  const [polEdit, setPolEdit] = useState(null) // null = editor closed; {maxRetry, findTime, banTime, permanent}
  const [polBusy, setPolBusy] = useState(false)
  const [polMsg, setPolMsg] = useState(null)
  const [svcBusy, setSvcBusy] = useState(false)

  const [loadErr, setLoadErr] = useState(false)

  const load = useCallback(async () => {
    const { ok: dOk, data: d } = await apiJSON('/api/droplet').catch(() => ({ ok: false }))
    if (dOk) setDroplet(d)
    if (!dOk || !d?.configured) return
    setLoading(true)
    const { ok, data } = await apiJSON('/api/droplet/fail2ban').catch(() => ({ ok: false }))
    if (ok) setF2b(data)
    setLoadErr(!ok) // a failed fetch used to leave "Checking…" up forever
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

  // Prefill the editor from the live effective policy so "open, tweak one
  // field, apply" round-trips the rest unchanged.
  const openPolicyEditor = () => {
    const permanent = !f2b?.banTime || f2b.banTime === '-1'
    setPolMsg(null)
    setPolEdit({
      maxRetry: String(f2b?.maxRetry ?? 3),
      findTime: f2b?.findTime || '10m',
      banTime: permanent ? '24h' : f2b.banTime,
      permanent,
    })
  }

  const savePolicy = async () => {
    const body = {
      maxRetry: parseInt(polEdit.maxRetry, 10) || 0,
      findTime: polEdit.findTime.trim(),
      banTime: polEdit.permanent ? '-1' : polEdit.banTime.trim(),
    }
    const banDesc = body.banTime === '-1'
      ? 'banned <b>permanently</b> — only "Allow back in" below ever lifts it'
      : `banned for <b>${body.banTime}</b>, then automatically released`
    if (!await ask(
      `Apply this ban policy to the droplet?<br/><br/>` +
      `Ban after <b>${body.maxRetry}</b> failed attempt(s) within <b>${body.findTime}</b>, ${banDesc}.<br/><br/>` +
      `Applying restarts fail2ban — existing bans blink out for a moment while it flips over, then re-assert from its own database.`,
      { ok: 'Apply policy' })) return
    setPolBusy(true)
    setPolMsg({ pending: true })
    try {
      const { data: r } = await postJSON('/api/droplet/fail2ban/policy', body)
      setPolMsg({ ok: !!r.ok, text: r.ok ? '✓ policy applied' : '✗ ' + (r.error || 'apply failed') })
      if (r.ok) { setPolEdit(null); await load() }
    } finally { setPolBusy(false) }
  }

  const toggleService = async () => {
    const turningOff = !!f2b?.active
    const msg = turningOff
      ? `Turn fail2ban <b>off</b>?<br/><br/>While it's off, nothing watches SSH auth failures and <b>existing bans stop being enforced</b> — ` +
        `they re-assert automatically when you turn it back on. Pubkey-only auth (and whichever SSH restriction is active) still stands either way.`
      : `Turn fail2ban on?<br/><br/>Re-asserts the current ban policy and re-applies existing bans from its database.`
    if (!await ask(msg, { ok: turningOff ? 'Turn off' : 'Turn on' })) return
    setSvcBusy(true)
    try {
      const { data: r } = await postJSON('/api/droplet/fail2ban/service', { enabled: !turningOff })
      if (!r.ok) toast('✗ ' + (r.error || (turningOff ? 'disable failed' : 'enable failed')), true)
      await load()
    } finally { setSvcBusy(false) }
  }

  if (!droplet?.configured) return null // nothing to show before the droplet exists

  const banned = f2b?.banned || []
  const recentAttempts = f2b?.recentAttempts || []
  const banHistory = f2b?.banHistory || []
  const permanentBans = !f2b?.banTime || f2b.banTime === '-1'

  // What fail2ban means depends on the lockdown mode. Under tunnel-only
  // it's defense-in-depth: you already need an authenticated WireGuard
  // connection to reach sshd at all, but a peer WITH the tunnel and
  // WITHOUT the right SSH key (your own device mid key-setup, or a stolen
  // tunnel config) still generates real failures and real bans. Under an
  // IP allow-list or no lockdown, it's front-line.
  const hintText = droplet.sshTunnelOnly
    ? <><strong>Extra layer behind WireGuard</strong> — only tunnel-connected devices reach sshd at all, but one connecting with a wrong or missing SSH key still racks up failures here and gets banned. That includes your own devices while you're setting up a key — <strong>"Allow back in"</strong> below is the undo.</>
    : droplet.sshLockedDown
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
        // "not installed" message before we actually know either way. A
        // FAILED fetch says so instead of leaving "Checking…" up forever.
        <p className="hint" style={{ margin: '10px 0 0' }}>
          {loading ? 'Checking…' : loadErr ? "Couldn't reach the droplet to check fail2ban — Refresh to retry." : ''}
        </p>
      ) : !f2b.installed ? (
        <p className="sub" style={{ margin: '10px 0 0' }}>
          {droplet.sshTunnelOnly
            ? <>Not installed yet. Even tunnel-only SSH benefits: a device holding a tunnel config but not the right SSH key can hammer sshd freely until fail2ban is here to ban it.</>
            : <>Port 22 is reachable from the internet with no brute-force protection watching it yet.</>}{' '}
          <button className="sm" onClick={() => onOpenSetup?.('lockdown')}>Set up fail2ban</button>
        </p>
      ) : !f2b.active ? (
        // Installed but switched off (the toggle here, or a crash) — the
        // status endpoint fast-paths this state, so render it as its own
        // clear "it's off" screen with the one action that matters up
        // front, instead of the full dashboard full of zeros.
        <>
          <p className="sub" style={{ margin: '10px 0' }}>
            fail2ban is installed but currently <strong>off</strong> — nothing is watching SSH auth failures and{' '}
            <strong>existing bans are not being enforced</strong>. They re-assert automatically from its database
            when you turn it back on.
          </p>
          <div className="row" style={{ gap: 8, alignItems: 'center', flexWrap: 'wrap', margin: '0 0 12px' }}>
            <button className="sm primary" disabled={svcBusy} onClick={toggleService}>
              {svcBusy ? 'working…' : 'Turn fail2ban on'}
            </button>
          </div>
          {banHistory.length > 0 && (
            <details style={{ marginTop: 4 }}>
              <summary className="hint" style={{ cursor: 'pointer', fontSize: 13, fontWeight: 600, color: 'var(--text)' }}>
                Ban history — all time ({banHistory.filter(e => e.action === 'ban').length} bans logged)
              </summary>
              <HistoryTable rows={banHistory} />
            </details>
          )}
        </>
      ) : (
        <>
          <p className="sub" style={{ margin: '10px 0' }}>
            Bans after <strong>{f2b.maxRetry}</strong> failed attempts within <strong>{f2b.findTime}</strong> — {permanentBans
              ? <><strong>permanently</strong>, first offense, no temporary grace period.</>
              : <>banned for <strong>{f2b.banTime}</strong>, then automatically released.</>}{' '}
            {droplet.sshTunnelOnly
              ? <>Only tunnel-connected devices reach sshd here, so a ban means a peer that has the tunnel but failed SSH auth anyway — a device mid key-setup, or a tunnel config in the wrong hands without the key.</>
              : droplet.sshLockedDown
              ? <>SSH is restricted to your allow-listed public IPs, not the WireGuard tunnel — so this is genuinely banning whoever still reaches sshd and fails.</>
              : <>SSH isn't restricted to any IP list yet — so this is genuinely banning whoever tries and fails, from anywhere on the internet.</>}{' '}
            Every ban is still just one click to reverse below if it's ever wrong.
          </p>
          <div className="row" style={{ gap: 8, alignItems: 'center', flexWrap: 'wrap', margin: '0 0 12px' }}>
            {!polEdit && <button className="sm" onClick={openPolicyEditor}>Edit ban policy</button>}
            <button className="sm" disabled={svcBusy} onClick={toggleService}>
              {svcBusy ? 'working…' : (f2b.active ? 'Turn fail2ban off' : 'Turn fail2ban on')}
            </button>
            <span className="hint" style={{ margin: 0 }}>
              {polMsg?.pending && <><span className="spin" />applying policy…</>}
              {polMsg && !polMsg.pending && <span className={polMsg.ok ? 'ok' : 'bad'}>{polMsg.text}</span>}
            </span>
          </div>
          {polEdit && (
            <div className="notice" style={{ marginBottom: 14 }}>
              <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'flex-end' }}>
                <label style={{ fontSize: 12.5 }}>Failed attempts before ban
                  <input type="number" min="1" max="20" value={polEdit.maxRetry} style={{ maxWidth: 90 }}
                    onChange={e => setPolEdit(p => ({ ...p, maxRetry: e.target.value }))} />
                </label>
                <label style={{ fontSize: 12.5 }}>…within window
                  <input value={polEdit.findTime} placeholder="10m" style={{ maxWidth: 90 }}
                    onChange={e => setPolEdit(p => ({ ...p, findTime: e.target.value }))} />
                </label>
                <label style={{ fontSize: 12.5, display: 'flex', alignItems: 'center', gap: 6, paddingBottom: 8 }}>
                  <input type="checkbox" checked={polEdit.permanent}
                    onChange={e => setPolEdit(p => ({ ...p, permanent: e.target.checked }))} />
                  Ban permanently
                </label>
                {!polEdit.permanent && (
                  <label style={{ fontSize: 12.5 }}>Ban for
                    <input value={polEdit.banTime} placeholder="24h" style={{ maxWidth: 90 }}
                      onChange={e => setPolEdit(p => ({ ...p, banTime: e.target.value }))} />
                  </label>
                )}
                <button className="sm primary" disabled={polBusy} onClick={savePolicy}>{polBusy ? 'applying…' : 'Apply policy'}</button>
                <button className="sm" disabled={polBusy} onClick={() => setPolEdit(null)}>Cancel</button>
              </div>
              <p className="hint" style={{ margin: '8px 0 0' }}>
                Durations are a number plus a unit — s, m, h, d, w (e.g. <span className="mono">10m</span>, <span className="mono">24h</span>).
                Applying re-renders the jail config on the droplet and restarts fail2ban.
              </p>
            </div>
          )}
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
