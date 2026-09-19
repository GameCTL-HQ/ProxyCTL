import { useCallback, useEffect, useState } from 'react'
import { apiJSON, getToken, setToken } from './api.js'
import AuthGate from './AuthGate.jsx'
import Admin from './Admin.jsx'
import SetupWizard from './SetupWizard.jsx'
import { UIProvider } from './ui.jsx'

// Top-level state machine. Three exclusive views:
//   - 'auth'  : no/expired JWT, or pre-claim → AuthGate
//   - 'setup' : authenticated but droplet not bootstrapped → SetupWizard
//   - 'app'   : authenticated + bootstrapped → Admin
// Same routing logic as the original web/index.html's routeView() +
// startApp() pair, but state-driven instead of CSS-class-driven.

export default function App() {
  const [view, setView] = useState('booting')
  const [authError, setAuthError] = useState('')
  // Which wizard step to land on when opening Setup from within the app —
  // e.g. the control-tunnel nudge banner deep-links straight to 'lockdown'
  // instead of the wizard's normal resume-where-you-left-off step.
  const [setupStep, setSetupStep] = useState(null)

  // After login/claim, decide where to land based on droplet state.
  const finishBoot = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/droplet')
    if (!ok) { setView('setup'); return }
    setView(data?.bootstrapped ? 'app' : 'setup')
  }, [])

  // Initial boot: do we have a token? Does it still work? Decide view.
  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const { data: state } = await apiJSON('/api/auth/state')
      if (cancelled) return
      const tok = getToken()
      if (state?.needsSetup || !tok) { setView('auth'); return }
      // Verify the token by probing a protected endpoint.
      const { ok } = await apiJSON('/api/entries')
      if (cancelled) return
      if (!ok) { setView('auth'); return }
      finishBoot()
    })()
    return () => { cancelled = true }
  }, [finishBoot])

  // Global 401 listener — any protected fetch losing its token drops us
  // back to the gate.
  useEffect(() => {
    const onUnauth = () => {
      setAuthError('Session expired. Please sign in again.')
      setView('auth')
    }
    window.addEventListener('proxyctl:unauthorized', onUnauth)
    return () => window.removeEventListener('proxyctl:unauthorized', onUnauth)
  }, [])

  const signOut = useCallback(() => {
    setToken(null)
    setAuthError('')
    setView('auth')
  }, [])

  // Wire the body class so the original CSS selectors (body.view-setup
  // hides the admin chrome, etc.) keep working unchanged.
  useEffect(() => {
    document.body.className = view === 'app'   ? 'view-app'
                            : view === 'setup' ? 'view-setup'
                            :                    'view-auth'
  }, [view])

  return (
    <UIProvider>
      {view === 'booting' && null /* brief blank while /api/auth/state lands */}
      {view === 'auth'    && <AuthGate initialError={authError} onAuthed={finishBoot} />}
      {view === 'setup'   && <SetupWizard onFinish={() => setView('app')} onSignOut={signOut} initialStep={setupStep} />}
      {view === 'app'     && <Admin onSignOut={signOut} onOpenSetup={(step) => { setSetupStep(step || null); setView('setup') }} />}
    </UIProvider>
  )
}
