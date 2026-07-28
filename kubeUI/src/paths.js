// Path normalization for the NFS share fields.
//
// Mirrors the server's normalizeNFSExport / normalizeKeysBasePath
// (server/keysconfig.go) so what the UI shows, and what it sends, is what
// the backend will actually store. Previously each caller did its own ad-hoc
// trim — `.replace(/\/$/, '')` strips ONE trailing slash and nothing strips
// the folder's LEADING slash, so an export typed as "/mnt/ssd/" or a folder
// typed as "/ProxyCTL/Keys" rendered and saved as "/mnt/ssd//ProxyCTL/Keys".
//
// Deliberately does NOT resolve ".." the way Go's path.Clean does: the server
// rejects traversal on the raw value rather than silently rewriting it into a
// path the operator never typed, and the UI should show them the same string
// the server is judging.

// collapse turns any run of slashes into one.
const collapse = (p) => String(p).replace(/\/{2,}/g, '/')

// cleanExport — an absolute share path with no trailing slash.
// "/mnt/ssd/" and "/mnt//ssd" both become "/mnt/ssd". Bare "/" is valid.
export function cleanExport(p) {
  const s = collapse(String(p ?? '').trim())
  if (!s) return ''
  if (s === '/') return s
  return s.replace(/\/+$/, '')
}

// cleanFolder — a relative folder under the share, no leading or trailing
// slash. "/ProxyCTL/Keys/" becomes "ProxyCTL/Keys".
export function cleanFolder(p) {
  const s = collapse(String(p ?? '').trim())
  return s.replace(/^\/+/, '').replace(/\/+$/, '')
}

// joinPath — join with exactly one separator, whatever the parts carry.
export function joinPath(base, rest) {
  const b = cleanExport(base)
  const r = cleanFolder(rest)
  if (!b) return r
  if (!r) return b
  return b === '/' ? `/${r}` : `${b}/${r}`
}
