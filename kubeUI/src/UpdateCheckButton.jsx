import { useEffect, useState } from 'react'
import { apiJSON } from './api.js'
import ReleaseNotes from './ReleaseNotes.jsx'

// Header control (ProxyCTL counterpart to GameCTL's UpdateCheckButton): shows
// the running version, a "What's new" changelog popup, and a manual "Check for
// updates" action. It auto-checks on load and, when an update is available,
// highlights itself (amber) so the operator is prompted even if the banner was
// dismissed. A manual check forces a fresh poll (bypasses the server cache) and
// re-shows the banner via the `proxyctl:recheck-updates` event.
export default function UpdateCheckButton() {
  const [version, setVersion] = useState('')
  const [available, setAvailable] = useState(false)
  const [latest, setLatest] = useState('')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  const [showNotes, setShowNotes] = useState(false)

  const apply = (d) => {
    setAvailable(!!d?.updateAvailable)
    setLatest(d?.latest || '')
  }

  useEffect(() => {
    let off = false
    apiJSON('/api/version').then(({ ok, data }) => { if (!off && ok) setVersion(data?.version || '') })
    apiJSON('/api/update/check').then(({ ok, data }) => { if (!off && ok) apply(data) })
    return () => { off = true }
  }, [])

  const flash = (m) => { setMsg(m); setTimeout(() => setMsg(''), 6000) }

  const check = async () => {
    setBusy(true); setMsg('')
    const { ok, data } = await apiJSON('/api/update/check?force=1')
    setBusy(false)
    if (!ok) { flash('Check failed'); return }
    apply(data)
    if (data?.updateAvailable) {
      flash(`Update available: ${data.latest}`)
      window.dispatchEvent(new CustomEvent('proxyctl:recheck-updates'))
    } else if (data?.note) {
      flash(data.note)
    } else {
      flash('Up to date ✓')
    }
  }

  // When an update is available, surface the banner (it carries the details +
  // the one-click update) and scroll to it instead of re-checking.
  const showBanner = () => {
    window.dispatchEvent(new CustomEvent('proxyctl:recheck-updates'))
    window.scrollTo({ top: 0, behavior: 'smooth' })
  }

  return (
    <>
      {msg && <span className="upd-msg">{msg}</span>}
      <button
        className="sm"
        title={version ? `Running ${version} — see what’s new` : 'See what’s new'}
        onClick={() => setShowNotes(true)}
      >
        What’s new
      </button>
      <button
        className={'sm' + (available ? ' upd-avail' : '')}
        disabled={busy}
        title={available
          ? 'Show the update details'
          : (version ? `Running ${version} — check for updates` : 'Check for updates')}
        onClick={available ? showBanner : check}
      >
        {busy ? 'Checking…' : available ? `● Update available${latest ? ` (${latest})` : ''}` : 'Check for updates'}
      </button>

      {showNotes && <ReleaseNotes onClose={() => setShowNotes(false)} />}
    </>
  )
}
