import { useEffect, useState } from 'react'
import { apiJSON } from './api.js'
import ReleaseNotes from './ReleaseNotes.jsx'

const DISMISS_KEY = 'proxyctlUpdateDismissed' // stores the version the user ignored

// In-app update notifier + one-click self-update (the ProxyCTL counterpart to
// GameCTL's UpdateBanner). Auto-checks /api/update/check on mount; when a newer
// release is published it shows a banner with an in-app "What's changing"
// changelog and "Update now" (rolls ProxyCTL's own Deployment → re-pulls the
// latest image). "Later" hides it for that version; the header "Check for
// updates" button dispatches `proxyctl:recheck-updates` to bring it back.
export default function UpdateBanner() {
  const [st, setSt] = useState(null)        // { current, latest, updateAvailable, releaseUrl }
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  const [dismissed, setDismissed] = useState(() => localStorage.getItem(DISMISS_KEY) || '')
  const [showNotes, setShowNotes] = useState(false)

  const load = (force = false) =>
    apiJSON(`/api/update/check${force ? '?force=1' : ''}`)
      .then(({ ok, data }) => { if (ok) setSt(data) })
      .catch(() => { /* soft-fail: no banner */ })

  useEffect(() => {
    load()
    const onRecheck = () => {
      // Manual check: clear any prior dismissal so the banner can reappear.
      localStorage.removeItem(DISMISS_KEY)
      setDismissed('')
      load(true)
    }
    window.addEventListener('proxyctl:recheck-updates', onRecheck)
    return () => window.removeEventListener('proxyctl:recheck-updates', onRecheck)
  }, [])

  if (!st || !st.updateAvailable) return null
  if (!msg && dismissed && dismissed === st.latest) return null

  const apply = async () => {
    setBusy(true); setMsg('')
    const { ok, data } = await apiJSON('/api/update/apply', { method: 'POST' })
    if (ok && data?.ok && data?.warning) {
      // A manual out-of-band step is needed too (new RBAC ProxyCTL can't
      // grant itself) — keep this on screen instead of auto-reloading it
      // away in 7s.
      setBusy(false)
      setMsg(`Updated, but one more step: ${data.warning}`)
    } else if (ok && data?.ok) {
      setMsg('Updating… ProxyCTL is rolling to the latest image — this page will reconnect shortly.')
      setTimeout(() => window.location.reload(), 7000)
    } else {
      setBusy(false)
      setMsg(data?.error || 'Update failed — try again, or redeploy manually.')
    }
  }

  const later = () => {
    localStorage.setItem(DISMISS_KEY, st.latest)
    setDismissed(st.latest)
  }

  return (
    <div className="updbar">
      <span className="updbar-dot" />
      <span className="updbar-txt">
        {msg || <>ProxyCTL update available — <b>{st.current}</b> → <b>{st.latest}</b></>}
      </span>
      {!msg && (
        <span className="updbar-actions">
          <button className="sm" onClick={() => setShowNotes(true)}>What’s changing</button>
          {st.releaseUrl && (
            <a className="updbar-link" href={st.releaseUrl} target="_blank" rel="noreferrer">On GitHub ↗</a>
          )}
          <button className="sm" onClick={later}>Later</button>
          <button className="sm primary" disabled={busy} onClick={apply}>{busy ? 'Updating…' : 'Update now'}</button>
        </span>
      )}

      {showNotes && (
        <ReleaseNotes currentVersion={st.latest} onClose={() => setShowNotes(false)} />
      )}
    </div>
  )
}
