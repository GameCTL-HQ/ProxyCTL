// API client: JWT in localStorage, Authorization: Bearer on every protected
// /api/* request, global 401 handler emits `proxyctl:unauthorized` so the
// App can swap back to the auth gate. Same shape as GameCTL's
// kubeUI/src/api/client.js + auth/index.js.

export const TOKEN_KEY = 'proxyctl_token'
const PUBLIC_PATHS = new Set([
  '/api/token',
  '/api/auth/setup',
  '/api/auth/state',
])

export const getToken = () => localStorage.getItem(TOKEN_KEY)
export const setToken = (t) => {
  if (t) localStorage.setItem(TOKEN_KEY, t)
  else localStorage.removeItem(TOKEN_KEY)
}

export async function apiFetch(path, init = {}) {
  const headers = new Headers(init.headers || {})
  const isPublic = PUBLIC_PATHS.has(path)
  const tok = getToken()
  if (tok && !isPublic) headers.set('Authorization', 'Bearer ' + tok)
  if (init.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  const res = await fetch(path, { ...init, headers })
  if (res.status === 401 && !isPublic) {
    setToken(null)
    window.dispatchEvent(new CustomEvent('proxyctl:unauthorized'))
  }
  return res
}

export async function apiJSON(path, init) {
  const r = await apiFetch(path, init)
  let data = null
  try { data = await r.json() } catch { /* non-JSON or empty */ }
  return { ok: r.ok, status: r.status, data }
}

// Convenience POST with a JSON body.
export const postJSON = (path, body) =>
  apiJSON(path, { method: 'POST', body: JSON.stringify(body) })
export const putJSON = (path, body) =>
  apiJSON(path, { method: 'PUT', body: JSON.stringify(body) })
export const del = (path) =>
  apiJSON(path, { method: 'DELETE' })
