import { useEffect, useState } from 'react'
import { apiJSON } from './api.js'

// In-app update notifier + one-click self-update (the ProxyCTL counterpart to
// GameCTL's UpdateBanner). Auto-checks /api/update/check on mount; when a newer
// release is published it shows a banner with "Update now" (rolls ProxyCTL's
// own Deployment → re-pulls the latest image). Dismissable; the check is cached
// server-side so this is cheap.
export default function UpdateBanner() {
  const [st, setSt] = useState(null)        // { current, latest, updateAvailable, releaseUrl }
  const [dismissed, setDismissed] = useState(false)
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')

  useEffect(() => {
    let off = false
    apiJSON('/api/update/check').then(({ ok, data }) => { if (!off && ok) setSt(data) })
    return () => { off = true }
  }, [])

  if (dismissed || !st || !st.updateAvailable) return null

  const apply = async () => {
    setBusy(true); setMsg('')
    const { ok, data } = await apiJSON('/api/update/apply', { method: 'POST' })
    if (ok && data?.ok) {
      setMsg('Updating… ProxyCTL is rolling to the latest image — this page will reconnect shortly.')
      setTimeout(() => window.location.reload(), 7000)
    } else {
      setBusy(false)
      setMsg(data?.error || 'Update failed — try again, or redeploy manually.')
    }
  }

  return (
    <div className="updbar">
      <span className="updbar-dot" />
      <span className="updbar-txt">
        {msg || <>ProxyCTL update available — <b>{st.current}</b> → <b>{st.latest}</b></>}
      </span>
      {!msg && (
        <span className="updbar-actions">
          {st.releaseUrl && (
            <a className="updbar-link" href={st.releaseUrl} target="_blank" rel="noreferrer">What’s new ↗</a>
          )}
          <button className="sm" disabled={busy} onClick={apply}>{busy ? 'Updating…' : 'Update now'}</button>
          <button className="updbar-x" title="Dismiss" onClick={() => setDismissed(true)}>×</button>
        </span>
      )}
    </div>
  )
}
