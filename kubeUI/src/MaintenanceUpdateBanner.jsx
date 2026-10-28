import { useCallback, useEffect, useMemo, useState } from 'react'
import { apiJSON } from './api.js'

const DISMISS_KEY = 'proxyctl_maintenance_update_dismissed'
const POLL_MS = 15 * 60 * 1000

function updateDetails(management, wireguard) {
  const out = []
  const s = management?.reachable ? management.status : null
  const packages = Number(s?.pendingUpdates || 0)
  const wgBehind = wireguard?.droplet && !wireguard.droplet.error && wireguard.droplet.synced === false

  if (wireguard?.updateAvailable || wgBehind) {
    const parts = []
    if (wireguard?.updateAvailable) parts.push('gateway image')
    if (wgBehind) parts.push(`droplet tools ${wireguard.droplet.version || 'installed version'} → ${wireguard.droplet.expected}`)
    out.push({ kind: 'wireguard', text: `WireGuard update available (${parts.join(' + ')})` })
  }
  if (packages > 0) out.push({ kind: 'packages', text: `${packages} droplet package update${packages === 1 ? '' : 's'} available` })
  if (s?.rebootRequired) out.push({ kind: 'reboot', text: 'Droplet reboot required to finish installed updates' })
  return out
}

export default function MaintenanceUpdateBanner({ onOpenDroplet }) {
  const [management, setManagement] = useState(null)
  const [wireguard, setWireguard] = useState(null)
  const [dismissed, setDismissed] = useState(() => {
    try { return localStorage.getItem(DISMISS_KEY) || '' } catch { return '' }
  })

  const load = useCallback(async () => {
    try {
      const [m, w] = await Promise.all([
        apiJSON('/api/droplet/management'),
        apiJSON('/api/wireguard/image'),
      ])
      if (m.ok) setManagement(m.data)
      if (w.ok) setWireguard(w.data)
    } catch { /* best-effort poll: keep the last good state */ }
  }, [])

  useEffect(() => {
    load()
    const timer = setInterval(load, POLL_MS)
    window.addEventListener('proxyctl:maintenance-recheck', load)
    return () => {
      clearInterval(timer)
      window.removeEventListener('proxyctl:maintenance-recheck', load)
    }
  }, [load])

  const updates = useMemo(() => updateDetails(management, wireguard), [management, wireguard])
  const fingerprint = updates.map(x => `${x.kind}:${x.text}`).join('|')
  if (!fingerprint || dismissed === fingerprint) return null

  const later = () => {
    try { localStorage.setItem(DISMISS_KEY, fingerprint) } catch { /* storage may be disabled */ }
    setDismissed(fingerprint)
  }

  return (
    <div className="updbar" role="alert" style={{ borderColor: 'var(--warn)' }}>
      <span className="updbar-dot" />
      <span className="updbar-txt">
        <strong>Remote maintenance available</strong><br />
        {updates.map((u, i) => <span key={u.kind}>{i ? ' · ' : ''}{u.text}</span>)}
      </span>
      <span className="updbar-actions">
        <button className="sm" onClick={later}>Later</button>
        <button className="sm primary" onClick={onOpenDroplet}>Open Droplet Management</button>
      </span>
    </div>
  )
}
