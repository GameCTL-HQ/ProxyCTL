import { useEffect, useState } from 'react'
import { apiJSON, postJSON } from './api.js'

// Nudges an install still on the OLD IP-allow-list SSH lockdown (from
// before the control tunnel existed) to convert over — the whole reason
// this exists: that lockdown depends on the operator's home IP staying
// put, which is exactly the failure mode that locked ProxyCTL out of its
// own droplet once already. Deliberately NOT part of the Setup wizard: an
// already-configured install has no reason to revisit that, so this is a
// dismissible banner in the main app instead, with a deep link straight to
// the wizard's "Lock down" step.
//
// The conversion is two steps once you land there (both in the same
// place): set up + verify the control tunnel, then restrict SSH to it —
// which REPLACES the public-IP allow-list entirely (SSH stops accepting
// connections on the public IP at all). Dismissing this banner ("Not
// now") only hides the banner — the small header button next to "Setup"
// keeps the option available for whenever you're ready.
//
// Trigger: sshLockedDown true (old method in use) AND sshTunnelOnly false
// (conversion not finished) AND not previously dismissed. Dismissal is
// persisted server-side (next to the droplet config) so it survives
// across browsers/devices — a one-time nudge, not a nag on every load.
export default function ControlTunnelNudge({ onOpenSetup }) {
  const [d, setD] = useState(null)
  const [busy, setBusy] = useState(false)
  const [hidden, setHidden] = useState(false)

  useEffect(() => {
    apiJSON('/api/droplet')
      .then(({ ok, data }) => { if (ok) setD(data) })
      .catch(() => { /* soft-fail: no banner */ })
  }, [])

  if (!d || hidden) return null
  if (!d.sshLockedDown || d.sshTunnelOnly || d.controlTunnelNudgeDismissed) return null

  const dismiss = async () => {
    setBusy(true)
    setHidden(true) // optimistic — don't make them wait on the network to stop seeing this
    await postJSON('/api/droplet/control-tunnel/dismiss-nudge', {}).catch(() => {})
    setBusy(false)
  }

  return (
    <div className="updbar">
      <span className="updbar-dot" />
      <span className="updbar-txt">
        Your SSH lockdown depends on your home IP staying the same — recommend switching to the new method in case it changes (e.g. a DHCP lease renewal).
        <br />
        <span className="hint" style={{ margin: 0 }}>
          New method: a dedicated WireGuard tunnel just for ProxyCTL's own SSH management. {d.controlTunnelReady
            ? "It's verified and ready — the recommended next step replaces your public-IP allow-list with it entirely, so SSH only works through the tunnel from then on."
            : 'Set it up, then the recommended follow-up replaces your public-IP allow-list with it entirely — no more IP list to keep current when your home IP changes.'}
        </span>
      </span>
      <span className="updbar-actions">
        <button className="sm" disabled={busy} onClick={dismiss}>Not now</button>
        <button className="sm primary" onClick={() => onOpenSetup?.('lockdown')}>
          {d.controlTunnelReady ? 'Switch to tunnel-only' : 'Set up control tunnel'}
        </button>
      </span>
    </div>
  )
}
