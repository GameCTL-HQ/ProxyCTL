import { useEffect, useState } from 'react'
import { apiJSON } from './api.js'

// Loud orange "DEV BUILD" pill shown only when the running binary is NOT a
// tagged release (version "dev", a git SHA from scripts/clusterdeploy.sh, or
// anything not vX.Y.Z). Released images built by CI from a v* tag render
// nothing. Mirrors GameCTL's DevBadge.
//
// This matters beyond cosmetics: a dev build also (a) never reports an
// available update, because the checker only compares semver tags, and
// (b) shows the "Unreleased" section in What's-new. Without this pill both
// behaviours look like bugs rather than "you're running an untagged build".
export default function DevBadge() {
  const [version, setVersion] = useState(null)

  useEffect(() => {
    apiJSON('/api/version')
      .then(({ data }) => setVersion(data?.version ?? ''))
      .catch(() => {})
  }, [])

  if (version === null) return null
  if (/^v\d+\.\d+\.\d+/.test(version)) return null

  return (
    <span
      className="badge devbadge"
      title={`Development build (${version || 'unversioned'}) — not a tagged release. Update checks stay silent and What's-new shows the Unreleased section.`}
    >
      Dev build{version && version !== 'dev' ? ` · ${version}` : ''}
    </span>
  )
}
