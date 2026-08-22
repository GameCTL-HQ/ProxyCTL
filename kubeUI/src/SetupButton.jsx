import { useEffect, useState } from 'react'
import { apiJSON } from './api.js'

// The header's "Setup" launcher — also doubles as the persistent SSH
// conversion nudge (ProxyCTL counterpart to UpdateCheckButton's amber
// "update available" highlight): whenever an install is still on the old
// public-IP SSH lockdown and hasn't converted to the control tunnel, this
// SAME button highlights and jumps straight to the wizard's Lock down
// step instead of wherever it'd normally resume. Deliberately folded into
// one button rather than a second one next to it — the header was
// getting crowded, and this option needs to survive the bigger banner
// (ControlTunnelNudge.jsx) being dismissed without adding its own button
// to do that.
export default function SetupButton({ onOpenSetup }) {
  const [d, setD] = useState(null)

  useEffect(() => {
    apiJSON('/api/droplet')
      .then(({ ok, data }) => { if (ok) setD(data) })
      .catch(() => { /* soft-fail: plain "Setup" */ })
  }, [])

  const needsConversion = !!(d && d.sshLockedDown && !d.sshTunnelOnly)

  return (
    <button
      className={'sm' + (needsConversion ? ' upd-avail' : '')}
      style={{ marginLeft: 10 }}
      title={needsConversion
        ? 'Your SSH lockdown depends on your home IP staying the same — switch to the control tunnel instead'
        : 'Setup'}
      onClick={() => onOpenSetup(needsConversion ? 'lockdown' : undefined)}
    >
      {needsConversion ? '● Setup' : 'Setup'}
    </button>
  )
}
