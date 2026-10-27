import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { apiJSON } from './api.js'

// In-app changelog modal — the ProxyCTL counterpart to GameCTL's ReleaseNotes.
// Fetches the embedded changelog (/api/release-notes) and lists every release
// with colour-coded change tags, highlighting the one matching the running
// build. Used by the header "What's new" button and the update banner's
// "What's changing" link.
//
// Props:
//   - onClose:        renders the overlay + close button (modal use)
//   - currentVersion: optional; overrides which entry is highlighted (e.g. the
//                     *available* version when opened from the update banner)

// Maps a change "type" to a small coloured tag so the operator can scan
// "what's updating and why" at a glance.
const TAG = {
  fixed:    { label: 'Fix',      cls: 'fix' },
  added:    { label: 'New',      cls: 'new' },
  changed:  { label: 'Changed',  cls: 'changed' },
  removed:  { label: 'Removed',  cls: 'removed' },
  security: { label: 'Security', cls: 'security' },
}

function ChangeRow({ change }) {
  const tag = TAG[change.type] || { label: change.type || 'Note', cls: '' }
  return (
    <li className="rn-chg">
      <div className="rn-chg-head">
        <span className={'rn-tag ' + tag.cls}>{tag.label}</span>
        <span className="rn-chg-title">{change.title}</span>
      </div>
      {change.detail && <p className="rn-detail">{change.detail}</p>}
    </li>
  )
}

function ReleaseBlock({ rel, highlight }) {
  return (
    <section className={'rn-rel' + (highlight ? ' cur' : '')}>
      <div className="rn-rel-head">
        <h4>{rel.name || rel.version}</h4>
        {rel.name && rel.name !== rel.version && <span className="rn-ver">{rel.version}</span>}
        {rel.date && <span className="rn-when">· {rel.date}</span>}
        {highlight && <span className="rn-thisbuild">This build</span>}
      </div>
      {rel.summary && <p className="rn-sum">{rel.summary}</p>}
      <ul className="rn-changes">
        {(rel.changes || []).map((c, i) => <ChangeRow key={i} change={c} />)}
      </ul>
    </section>
  )
}

export default function ReleaseNotes({ onClose, currentVersion }) {
  const [data, setData] = useState(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    let off = false
    apiJSON('/api/release-notes').then(({ ok, data }) => {
      if (off) return
      if (ok) setData(data)
      else setErr(data?.error || 'Could not load release notes')
    }).catch((e) => { if (!off) setErr(e.message || 'Could not load release notes') })
    return () => { off = true }
  }, [])

  const norm = (s) => (s || '').replace(/^v/, '').trim()
  const highlightVer = norm(currentVersion || data?.current?.version || data?.version)

  const onBackdrop = (e) => { if (e.target.dataset.backdrop) onClose?.() }

  // Render through a portal to <body>: this modal is mounted from the sticky
  // header (and the update banner), and header.app has a backdrop-filter, which
  // makes it the containing block for position:fixed children — that would pin
  // the overlay to the header box instead of the viewport. Portaling to body
  // escapes that so `.overlay { inset:0 }` is always viewport-relative.
  return createPortal(
    <div className="overlay open" data-backdrop="1" onClick={onBackdrop}>
      <div className="modal rn">
        <div className="rn-head">
          <h3>What’s new in ProxyCTL</h3>
          {onClose && <button className="x" title="Close" onClick={onClose}>×</button>}
        </div>

        {err && <div className="warn">{err}</div>}
        {!data && !err && <p className="sub">Loading release notes…</p>}

        {data && (
          <div className="rn-list">
            {(data.releases || []).map((rel) => (
              <ReleaseBlock
                key={rel.version}
                rel={rel}
                highlight={norm(rel.version) === highlightVer
                  || (data.current && rel.version === data.current.version)}
              />
            ))}
            {(!data.releases || data.releases.length === 0) && (
              <p className="sub">No release notes available.</p>
            )}
          </div>
        )}
      </div>
    </div>,
    document.body
  )
}
