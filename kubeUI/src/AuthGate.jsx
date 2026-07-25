import { useEffect, useRef, useState } from 'react'
import { apiJSON, setToken } from './api.js'
import { ProxyLogo } from './brand.jsx'

// AuthGate: shown when no JWT is in localStorage or when an expired one
// triggers a 401 from a protected /api/* call. Picks claim vs login from
// /api/auth/state.needsSetup, same as GameCTL's App.jsx.
export default function AuthGate({ initialError, onAuthed }) {
  const [mode, setMode] = useState(null) // 'claim' | 'login'
  const [error, setError] = useState(initialError || '')
  const tokenRef = useRef(null)
  const userRef = useRef(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const { data } = await apiJSON('/api/auth/state')
      if (cancelled) return
      setMode(data?.needsSetup ? 'claim' : 'login')
    })()
    return () => { cancelled = true }
  }, [])

  // Runs AFTER the mode flips and React renders the matching form, so
  // the refs are actually attached. install.sh prints URLs like
  // http://.../?token=<bootstrap>, and this picks the token out of the
  // URL and drops it into the bootstrap field, leaving focus on the
  // username so the operator only has to enter user + password.
  useEffect(() => {
    if (mode === 'claim') {
      const q = new URLSearchParams(location.search).get('token')
      if (q && tokenRef.current) tokenRef.current.value = q
      ;(q ? userRef.current : tokenRef.current)?.focus()
    } else if (mode === 'login') {
      userRef.current?.focus()
    }
  }, [mode])

  async function submitClaim(ev) {
    ev.preventDefault()
    const form = ev.currentTarget
    setError('')
    const pw  = form.password.value
    const pw2 = form.password2.value
    if (pw !== pw2) { setError("Passwords don't match."); return }
    const body = {
      bootstrapToken: form.bootstrapToken.value.trim(),
      username:       form.username.value.trim(),
      password:       pw,
    }
    const { ok, status, data } = await apiJSON('/api/auth/setup', {
      method: 'POST', body: JSON.stringify(body),
    })
    if (!ok) { setError(data?.error || `Claim failed (HTTP ${status})`); return }
    if (data?.access_token) setToken(data.access_token)
    history.replaceState({}, '', location.pathname)
    onAuthed()
  }

  async function submitLogin(ev) {
    ev.preventDefault()
    const form = ev.currentTarget
    setError('')
    const body = {
      username: form.username.value.trim(),
      password: form.password.value,
    }
    const { ok, data } = await apiJSON('/api/token', {
      method: 'POST', body: JSON.stringify(body),
    })
    if (!ok) { setError(data?.error || 'Invalid credentials.'); return }
    if (data?.access_token) setToken(data.access_token)
    onAuthed()
  }

  if (!mode) return null // brief blank flash while /api/auth/state lands

  return (
    <div id="authView" data-mode={mode}>
      <div className="panel">
        <div className="brand">
          <ProxyLogo />
          <h1>ProxyCTL</h1>
          <span className="crumb">{mode === 'claim' ? 'first-time setup' : 'sign in'}</span>
        </div>
        {error && <div className="err">{error}</div>}
        {mode === 'claim' ? (
          <form onSubmit={submitClaim} autoComplete="off">
            <h2>Claim your admin account</h2>
            <p className="sub">This ProxyCTL instance is fresh. Paste the{' '}
              <strong>bootstrap token</strong> printed by{' '}
              <span style={{ color: 'var(--text)' }}>install.sh</span>{' '}
              and pick a username + password.</p>
            <label>Bootstrap token</label>
            <input ref={tokenRef} name="bootstrapToken" type="password" required spellCheck="false" autoComplete="off" placeholder="paste the token from install.sh" />
            <label>New username</label>
            <input ref={userRef} name="username" required minLength={3} maxLength={64} spellCheck="false" autoComplete="username" placeholder="e.g. admin" />
            <label>New password</label>
            <input name="password" type="password" required minLength={12} placeholder="at least 12 characters" autoComplete="new-password" />
            <label>Confirm password</label>
            <input name="password2" type="password" required minLength={12} placeholder="re-type password" autoComplete="new-password" />
            <button type="submit" className="primary">Create admin &amp; sign in</button>
            <p className="hint">Bootstrap token is consumed by this form &mdash; there is
              no way to retrieve it afterwards. To start over later:{' '}
              <code>kubectl delete secret proxyctl-auth</code> + rollout restart.</p>
          </form>
        ) : (
          <form onSubmit={submitLogin} autoComplete="off">
            <h2>Sign in</h2>
            <p className="sub">Welcome back.</p>
            <label>Username</label>
            <input ref={userRef} name="username" required spellCheck="false" autoComplete="username" />
            <label>Password</label>
            <input name="password" type="password" required autoComplete="current-password" />
            <button type="submit" className="primary">Sign in</button>
          </form>
        )}
      </div>
    </div>
  )
}
