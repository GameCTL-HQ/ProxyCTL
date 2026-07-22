import { useCallback, useEffect, useState } from 'react'
import { apiJSON, postJSON, putJSON, del } from './api.js'
import { useUI } from './ui.jsx'
import { ProxyLogo } from './brand.jsx'
import { copyText } from './clipboard.js'
import PersonalAccessPanel from './PersonalAccessPanel.jsx'

// Wizard step order — index into STEPS is the current screen. 'storage' is
// first: it decides where ProxyCTL's gateway keys live before anything else
// is configured.
const STEPS = ['welcome', 'storage', 'target', 'prep', 'lockdown', 'cf', 'tunnel', 'done']

// Where to land on auto-open / refresh. Server's droplet state drives it.
// Indexed via STEPS.indexOf so reordering steps can't silently skew resumes.
function resumeStep(d) {
  if (d.bootstrapped) return STEPS.indexOf('tunnel')
  if (d.configured)   return STEPS.indexOf('prep')
  return STEPS.indexOf('storage')
}

function shellQ(s) { return "'" + String(s).replaceAll("'", "'\\''") + "'" }

// Numbered header for the Lock down SSH step's 1-2-3 sequence — a plain
// number until that step is done, then a checkmark, so it reads as a
// checklist rather than three independent, skippable options.
function StepHeader({ n, done, title }) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 10 }}>
      <span className={'stepbadge' + (done ? ' done' : '')}>{done ? '✓' : n}</span>
      <strong style={{ fontSize: 15 }}>{title}</strong>
    </div>
  )
}


export default function SetupWizard({ onFinish, onSignOut, initialStep }) {
  const { ask, toast } = useUI()
  const [step, setStep] = useState(0)
  const [droplet, setDroplet] = useState({})
  const [cfState, setCfState] = useState({ configured: false })

  // Per-step transient state.
  const [ip, setIp] = useState('')
  const [user, setUser] = useState('root')
  const [port, setPort] = useState('22')
  const [testMsg, setTestMsg] = useState(null) // {ok, text}
  const [testOut, setTestOut] = useState('')

  const [bootMsg, setBootMsg] = useState(null)
  const [bootOut, setBootOut] = useState('')
  const [bootUpgrade, setBootUpgrade] = useState(true)
  const [bootBusy, setBootBusy] = useState(false)

  const [lockInput, setLockInput] = useState('')
  const [detectMsg, setDetectMsg] = useState(null)
  const [lockMsg, setLockMsg] = useState(null)
  const [lockOut, setLockOut] = useState('')
  const [lockBusy, setLockBusy] = useState(false)
  const [showIPForm, setShowIPForm] = useState(false) // manual IP-allowlist is now the secondary/advanced path

  const [tunnelLockMsg, setTunnelLockMsg] = useState(null)
  const [tunnelLockOut, setTunnelLockOut] = useState('')
  const [tunnelLockBusy, setTunnelLockBusy] = useState(false)

  const [controlTunnel, setControlTunnel] = useState({ pubKey: '', ready: false })
  const [ctMsg, setCtMsg] = useState(null)
  const [ctOut, setCtOut] = useState('')
  const [ctBusy, setCtBusy] = useState(false)

  const [fail2ban, setFail2ban] = useState({ installed: false })
  const [f2bMsg, setF2bMsg] = useState(null)
  const [f2bOut, setF2bOut] = useState('')
  const [f2bBusy, setF2bBusy] = useState(false)
  const [unbanBusyIP, setUnbanBusyIP] = useState('')

  // Detects a lockdown file on the droplet that doesn't match what
  // droplet.json believes (see sshDriftCheck in server/main.go) — most
  // often a stale file left by an apply that failed partway through,
  // silently armed to go live on some later, unrelated sshd reload.
  const [sshDrift, setSSHDrift] = useState(null)
  const [driftFixBusy, setDriftFixBusy] = useState(false)
  const [driftFixMsg, setDriftFixMsg] = useState(null)

  // A UFW rule restricting port 22, set up by hand before ProxyCTL managed
  // SSH at all — invisible to both Option A and Option B below (neither
  // touches UFW), so it's an independent, unmanaged failure point: an old
  // "allow only my IP" rule breaks the operator's own access on any IP
  // change no matter which option is chosen (see ufwStatus in server/main.go).
  const [ufw, setUFW] = useState(null)
  const [ufwFixBusy, setUFWFixBusy] = useState(false)
  const [ufwFixMsg, setUFWFixMsg] = useState(null)

  const [cfToken, setCfToken] = useState('')
  const [cfMsg, setCfMsg] = useState(null)
  const [cfBusy, setCfBusy] = useState(false)
  // 'idle' | 'checking' | 'ok' | 'error' — drives the Re-check button's own
  // label directly (Re-check → Checking… → Successful/Failed), not a
  // separate hint span.
  const [cfCheckState, setCfCheckState] = useState('idle')

  const [tunnel, setTunnel] = useState({ cfConfigured: false, connectorPresent: false, cloudflaredReady: false })
  const [tunnelMsg, setTunnelMsg] = useState(null) // {ok, text} | {pending:true}
  const [tunnelCheckState, setTunnelCheckState] = useState('idle') // same three(+one)-state model as cfCheckState

  const [keysCfg, setKeysCfg] = useState(null) // /api/keys-config
  const [keysVal, setKeysVal] = useState('')
  // Explicit NFS share for the keys. Blank = keep them on the share the
  // install's own StorageClass already uses.
  const [keysSrv, setKeysSrv] = useState('')
  const [keysExp, setKeysExp] = useState('')
  const [keysBusy, setKeysBusy] = useState(false)
  // /api/storage/share-setup for the typed export: node list + paste-ready
  // share-creation commands (exports line restricted to the node IPs).
  const [shareSetup, setShareSetup] = useState(null)
  // /api/storage/status — how /data is provisioned. mode "ephemeral" means a
  // fresh install still on its setup-mode emptyDir: the Storage step is then
  // REQUIRED (test the share, adopt it) before anything else.
  const [storage, setStorage] = useState(null)
  const [probeMsg, setProbeMsg] = useState(null)   // share test result
  const [adoptMsg, setAdoptMsg] = useState(null)   // adopt progress
  const [fallbackSC, setFallbackSC] = useState('')

  const loadStorage = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/storage/status')
    setStorage(ok && data ? data : null)
    return ok ? data : null
  }, [])

  const loadDroplet = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/droplet')
    if (ok && data) {
      setDroplet(data)
      if (data.ip)   setIp(data.ip)
      if (data.user) setUser(data.user)
      if (data.port) setPort(String(data.port))
    }
  }, [])

  const loadControlTunnel = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/droplet/control-tunnel')
    setControlTunnel(ok && data ? data : { pubKey: '', ready: false })
  }, [])

  const loadFail2ban = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/droplet/fail2ban')
    setFail2ban(ok && data ? data : { installed: false })
  }, [])

  const loadSSHDrift = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/droplet/ssh/drift')
    setSSHDrift(ok && data?.checked ? data : null)
  }, [])

  // filePresent && !trackedTunnelOnly: a stale file, safe to just remove
  // (unlockSSH already does exactly rm + reload + sync state). The other
  // direction (file missing but tracked as tunnel-only) needs the wizard's
  // own restrict flow to recreate it correctly, not a plain unlock.
  const fixSSHDrift = async () => {
    if (!sshDrift) return
    setDriftFixBusy(true)
    setDriftFixMsg({ pending: true })
    try {
      if (sshDrift.filePresent && !sshDrift.trackedTunnelOnly) {
        const { data: r } = await postJSON('/api/droplet/unlock-ssh', {})
        setDriftFixMsg({ ok: r.ok, text: r.ok ? '✓ Stale file removed' : '✗ ' + (r.stderr || 'failed — see Setup logs') })
      } else {
        await applyTunnelOnlyLockdown()
        setDriftFixMsg(null) // applyTunnelOnlyLockdown already reports via tunnelLockMsg
      }
      await Promise.all([loadDroplet(), loadSSHDrift()])
    } finally { setDriftFixBusy(false) }
  }

  const loadUFW = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/droplet/ufw')
    setUFW(ok && data?.checked ? data : null)
  }, [])

  const fixUFW = async () => {
    if (!ufw?.sshRules?.length) return
    const lines = ufw.sshRules.map(r => r.raw).join('<br/>')
    if (!await ask(`Remove this UFW rule?<br/><br/><span class="mono">${lines}</span><br/><br/>` +
      `A wide-open port-22 rule is added first if one isn't already there (ProxyCTL's own mechanism needs that to keep working), ` +
      `then only the rule(s) above are removed — never any other rule, never UFW itself. ` +
      `ProxyCTL verifies its own SSH access still works right after and automatically reverts to the exact prior rules if it doesn't.`,
      { ok: 'Remove rule' })) return
    setUFWFixBusy(true)
    setUFWFixMsg({ pending: true })
    try {
      const { data: r } = await postJSON('/api/droplet/ufw/fix', {})
      setUFWFixMsg({ ok: !!r.ok, text: r.ok ? '✓ removed' : '✗ ' + (r.error || (r.reverted ? 'failed — reverted, no change made' : 'failed — see droplet logs')) })
      await loadUFW()
    } finally { setUFWFixBusy(false) }
  }

  const refreshCf = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/cf/status')
    setCfState(ok && data ? data : { configured: false })
  }, [])

  const refreshTunnel = useCallback(async () => {
    setTunnelCheckState('checking')
    try {
      const s = await apiJSON('/api/tunnel/status')
      const data = s.ok && s.data ? s.data : { cfConfigured: false }
      setTunnel(data)
      setTunnelCheckState(data.cloudflaredReady ? 'ok' : 'error')
    } catch {
      setTunnelCheckState('error')
    }
  }, [])

  // setupTunnel runs the full Cloudflare Tunnel reconcile on demand —
  // ensures the tunnel, deploys the cloudflared connector. Surfaces the
  // first failed step (e.g. token missing the Tunnel:Edit scope).
  const setupTunnel = useCallback(async () => {
    setTunnelMsg({ pending: true })
    const { ok, data } = await apiJSON('/api/tunnel/setup', { method: 'POST' })
    const steps = data?.steps || []
    if (ok && data?.ok) {
      setTunnelMsg({ ok: true, text: '✓ Cloudflare Tunnel set up — cloudflared deploying' })
      // The connector pods take ~15-20s to go ready; re-poll a few
      // times so the status flips to "connected" on its own.
      for (const delay of [6000, 14000, 25000]) setTimeout(refreshTunnel, delay)
    } else {
      const bad = steps.find(s => !s.ok)
      const why = bad ? `${bad.name}: ${bad.stderr || 'failed'}` : (data?.error || 'tunnel setup failed')
      setTunnelMsg({ ok: false, text: '✗ ' + why })
    }
    refreshTunnel()
  }, [refreshTunnel])

  // Initial load. Decides which step to land on. Ephemeral storage (fresh
  // install, setup-mode emptyDir) always wins: nothing durable exists until
  // the Storage step is completed, so the wizard opens there.
  useEffect(() => {
    ;(async () => {
      const st = await loadStorage()
      const { data } = await apiJSON('/api/droplet')
      if (data) {
        setDroplet(data)
        if (data.ip)   setIp(data.ip)
        if (data.user) setUser(data.user)
        if (data.port) setPort(String(data.port))
      }
      if (initialStep && STEPS.includes(initialStep)) setStep(STEPS.indexOf(initialStep))
      else if (st?.mode === 'ephemeral') setStep(STEPS.indexOf('storage'))
      else setStep(resumeStep(data || {}))
    })()
  }, [loadStorage, initialStep])

  const loadKeys = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/keys-config')
    if (ok && data) {
      setKeysCfg(data)
      setKeysVal(data.basePath || '')
      // Prefill only a share the operator actually chose — leaving these blank
      // when unset is what keeps the discovered default in play (and shows it
      // as placeholder text rather than committing them to it).
      setKeysSrv(data.nfsServer || '')
      setKeysExp(data.nfsExport || '')
    }
  }, [])

  // Refresh CF / Tunnel / Storage state when we land on the matching step.
  useEffect(() => { if (STEPS[step] === 'cf') refreshCf() }, [step, refreshCf])
  useEffect(() => { if (STEPS[step] === 'tunnel') refreshTunnel() }, [step, refreshTunnel])
  useEffect(() => { if (STEPS[step] === 'storage') loadKeys() }, [step, loadKeys])
  useEffect(() => { if (STEPS[step] === 'lockdown') loadControlTunnel() }, [step, loadControlTunnel])
  useEffect(() => { if (STEPS[step] === 'lockdown') loadFail2ban() }, [step, loadFail2ban])
  useEffect(() => { if (STEPS[step] === 'lockdown') loadSSHDrift() }, [step, loadSSHDrift])
  useEffect(() => { if (STEPS[step] === 'lockdown') loadUFW() }, [step, loadUFW])

  // Fetch the node list (for the "restrict to these IPs" note) on entering the
  // storage step, and regenerate the optional example commands as the export
  // path is typed (debounced — each fetch hits the k8s API for the node list).
  useEffect(() => {
    if (STEPS[step] !== 'storage') return
    const exp = keysExp.trim()
    const q = exp.startsWith('/') ? '?export=' + encodeURIComponent(exp) : ''
    const t = setTimeout(async () => {
      const { ok, data } = await apiJSON('/api/storage/share-setup' + q)
      setShareSetup(ok && data ? data : null)
    }, q ? 400 : 0)
    return () => clearTimeout(t)
  }, [step, keysExp])

  // === Storage step actions ==================================================
  async function probeShare() {
    const srv = keysSrv.trim(), exp = keysExp.trim()
    if (!srv || !exp) { toast('Enter the NFS server and export path first', true); return }
    setProbeMsg({ pending: true })
    const { ok, data } = await postJSON('/api/storage/test', { nfsServer: srv, nfsExport: exp })
    if (!ok) { setProbeMsg({ ok: false, text: '✗ ' + (data?.error || 'test failed') }); return }
    setProbeMsg({
      ok: !!data.ok,
      text: data.ok ? data.message : '✗ ' + (data.error || 'test failed'),
      detail: data.detail || '',
      srv, exp, // adopt is only enabled for the exact values that passed
      // Present only when the probe actually found something — a prior
      // install's app/ data and/or gateway key folders already sitting in
      // this exact directory (see parseExistingDataProbe in
      // server/storage_setup.go). Moving in reuses it as-is; this is here
      // so that's a decision, not a surprise, on a re-install pointed at
      // an old share.
      existingData: data.existingData || null,
    })
  }

  // moveIntoShare wraps adopt() with a confirmation when the probe found
  // existing ProxyCTL data in this exact directory (a re-install pointed at
  // an old share, most often) — adopt() itself never deletes or overwrites
  // anything, it just mounts the same directory, so this is purely making
  // "you're about to reuse prior data" a decision instead of a surprise.
  async function moveIntoShare() {
    const srv = keysSrv.trim(), exp = keysExp.trim()
    const ex = probeMsg?.existingData
    if (ex && (ex.appFiles > 0 || ex.keyDirs > 0)) {
      const parts = []
      if (ex.appFiles > 0) parts.push(`<b>${ex.appFiles}</b> existing app config file(s)`)
      if (ex.keyDirs > 0) parts.push(`<b>${ex.keyDirs}</b> existing gateway key folder(s)`)
      if (!await ask(
        `This directory already has ProxyCTL data in it — ${parts.join(' and ')}.` +
        `<br/><br/>Moving in reuses this data as-is; nothing is deleted or overwritten. ` +
        `If that's unexpected, pick a different (empty) directory instead.`,
        { ok: 'Continue with existing data' })) return
    }
    await adopt({ nfsServer: srv, nfsExport: exp })
  }

  // adopt: repoint ProxyCTL's own /data (share or StorageClass fallback).
  // The pod restarts; poll /healthz until it's back, then reload the page —
  // sessions are in-memory, so a fresh sign-in may be asked for, and the
  // wizard resumes past the (now-durable) storage step.
  async function adopt(body) {
    setAdoptMsg({ pending: true, text: 'Saving…' })
    const { ok, data } = await postJSON('/api/storage/adopt', body)
    if (!ok || !data?.ok) {
      setAdoptMsg({ ok: false, text: '✗ ' + (data?.error || 'adopt failed') })
      return
    }
    setAdoptMsg({ pending: true, text: data.message || 'Restarting onto the new storage…' })
    const started = Date.now()
    const poll = async () => {
      try {
        // raw fetch: /healthz is unauthenticated and cheap
        const r = await fetch('/healthz', { cache: 'no-store' })
        if (r.ok) {
          const st = await loadStorage().catch(() => null)
          if (st && st.mode !== 'ephemeral') { window.location.reload(); return }
        }
      } catch {}
      if (Date.now() - started < 120000) setTimeout(poll, 3000)
      else setAdoptMsg({ ok: false, text: '✗ ProxyCTL did not come back within 2 minutes — check the pod: kubectl -n proxyctl get pods' })
    }
    setTimeout(poll, 4000)
  }

  // Step gate: returns a message to disable Next, or '' when allowed.
  function nextBlocker() {
    const cur = STEPS[step]
    // Once the droplet's been bootstrapped at least once, the required
    // steps are already done — this is a revisit for review/tweaks, not
    // a first-time linear walk-through. Don't block Next just because
    // navigating back several steps (or jumping via a crumb) landed on
    // one whose OWN gating condition doesn't currently hold; the step's
    // own action buttons (Generate, Test, Prepare, ...) are still there
    // if something actually needs to be redone.
    if (droplet.bootstrapped) return ''
    if (cur === 'storage' && storage?.mode === 'ephemeral') {
      return 'ProxyCTL is on temporary storage — test your NFS share and click "Save & move in" (or use cluster storage) to continue.'
    }
    if (cur === 'target' && !droplet.hasKey) return 'Click Generate to create the keypair.'
    if (cur === 'target' &&  droplet.hasKey && !droplet.configured) return 'Paste the install command on the droplet, fill in the IP, then run Test (must show ✓ ssh OK).'
    if (cur === 'prep'   && !droplet.bootstrapped) return 'Click Prepare droplet, wait for ✓.'
    return ''
  }
  function nextLabel() {
    const cur = STEPS[step]
    if (cur === 'done') return 'Finish'
    if (cur === 'cf'       && !cfState.configured)        return 'Skip Cloudflare'
    if (cur === 'lockdown' && !droplet.sshLockedDown)     return 'Skip — leave SSH open'
    // "Skip" only while the tunnel is genuinely not set up — once setup
    // succeeds (even if the connector pods are still rolling) it's Next.
    if (cur === 'tunnel' && !tunnel.cloudflaredReady && !tunnelMsg?.ok) return 'Skip — no web apps'
    return 'Next'
  }

  async function saveKeys() {
    if (!keysVal.trim()) { toast('Enter a folder for the keys', true); return false }
    const srv = keysSrv.trim(), exp = keysExp.trim()
    // Half a share can't mount — catch it here rather than at apply time.
    if (!!srv !== !!exp) {
      toast('Give both an NFS server and an export path, or leave both blank', true)
      return false
    }
    setKeysBusy(true)
    const { ok, data } = await putJSON('/api/keys-config', {
      basePath: keysVal.trim(), nfsServer: srv, nfsExport: exp,
    })
    setKeysBusy(false)
    if (!ok) { toast(data?.error || 'Save failed', true); return false }
    toast(data?.mode === 'share' ? '✓ Keys share set' : '✓ Keys folder set')
    loadKeys()
    return true
  }

  // True when the keys step holds anything not yet persisted.
  function keysDirty() {
    return keysVal.trim() !== (keysCfg?.basePath || '') ||
      keysSrv.trim() !== (keysCfg?.nfsServer || '') ||
      keysExp.trim() !== (keysCfg?.nfsExport || '')
  }

  async function next() {
    // On the storage step, persist a changed folder/share before advancing so
    // the choice made during setup actually sticks.
    if (STEPS[step] === 'storage' && keysVal.trim() && keysDirty()) {
      await saveKeys()
    }
    if (step < STEPS.length - 1) { setStep(step + 1); return }
    onFinish()
  }
  function back() { if (step > 0) setStep(step - 1) }
  function skip() { onFinish() }

  // === Actions ===============================================================
  async function genKey() {
    const { data } = await postJSON('/api/droplet/generate', {})
    if (data?.error) { toast('✗ ' + data.error, true); return }
    loadDroplet()
  }
  async function regen() {
    if (!await ask("Rotate ProxyCTL's keypair? The current public key on the droplet stops working immediately — you'll need to install the NEW public key in <span class='mono'>~/.ssh/authorized_keys</span> before SSH works again.", { ok: 'Regenerate' })) return
    const { data } = await postJSON('/api/droplet/regenerate', {})
    if (data?.error) { toast('✗ ' + data.error, true); return }
    await loadDroplet()
    toast('✓ New keypair generated — install the new public key on the droplet')
  }
  async function saveAndTest() {
    if (!ip.trim()) { toast('Enter the droplet IP first', true); return }
    const { ok, data } = await putJSON('/api/droplet', { ip: ip.trim(), user: user.trim() || 'root', port: parseInt(port || '22', 10) })
    if (!ok) { toast('✗ ' + (data?.error || 'save failed'), true); return }
    setDroplet(data)
    setTestMsg({ pending: true })
    const { data: r } = await postJSON('/api/droplet/test', {})
    setTestMsg({ ok: !!r.ok, text: r.ok ? '✓ ssh OK' : `✗ ssh failed (exit ${r.exitCode}) — make sure you pasted the public key on the droplet's authorized_keys, then click Test again.` })
    const parts = []
    if (r.stdout) parts.push(r.stdout)
    if (r.stderr) parts.push('--- stderr ---\n' + r.stderr)
    setTestOut(parts.join('\n').trim() || '(no output)')
    loadDroplet()
  }
  async function bootstrap() {
    setBootBusy(true)
    setBootMsg({ pending: true, upgrade: bootUpgrade })
    setBootOut('')
    try {
      const { data: r } = await postJSON('/api/droplet/bootstrap', { upgrade: bootUpgrade })
      setBootMsg({ ok: !!r.ok, text: r.ok ? '✓ droplet ready' : `✗ bootstrap failed (exit ${r.exitCode})` })
      const parts = []
      if (r.stdout) parts.push(r.stdout)
      if (r.stderr) parts.push('--- stderr ---\n' + r.stderr)
      setBootOut(parts.join('\n').trim() || '(no output)')
      if (r.ok) loadDroplet()
    } finally { setBootBusy(false) }
  }
  async function detectIP() {
    setDetectMsg({ pending: true })
    const { data: r } = await apiJSON('/api/droplet/egress-ip')
    if (r?.error || !r?.egressIP) { setDetectMsg({ ok: false, text: '✗ ' + (r?.error || 'detect failed') }); return }
    const cur = lockInput.split(',').map(s => s.trim()).filter(Boolean)
    if (!cur.includes(r.egressIP)) cur.unshift(r.egressIP)
    setLockInput(cur.join(', '))
    setDetectMsg({ ok: true, text: `✓ ${r.egressIP} (this is ProxyCTL's egress IP — keep it on the list)` })
  }
  async function applyLockdown() {
    const ips = lockInput.split(',').map(s => s.trim()).filter(Boolean)
    if (!ips.length) { toast('Add at least one IP first', true); return }
    if (!await ask(`Lock down SSH on the droplet to ONLY these IPs?<br><br><span class="mono">${ips.join(', ')}</span><br><br>ProxyCTL verifies its own access immediately after applying and auto-reverts if it would be locked out. You can also recover via your provider's web console.`, { ok: 'Apply lockdown' })) return
    setLockBusy(true)
    setLockMsg({ pending: true })
    setLockOut('')
    try {
      const { data: r } = await postJSON('/api/droplet/lockdown-ssh', { ips })
      setLockMsg({ ok: !!r.ok, text: r.ok ? '✓ SSH locked down + verified' : '✗ ' + (r.error || `lockdown failed (exit ${r.exitCode})`) })
      const parts = []
      if (r.stdout) parts.push(r.stdout)
      if (r.stderr) parts.push('--- stderr ---\n' + r.stderr)
      setLockOut(parts.join('\n').trim() || '(no output)')
      if (r.ok) {
        loadDroplet()
        // Option B has no firewall gate (sshd-level only, by design — see
        // lockdownSSH's doc comment) so an allow-listed IP that changes
        // doesn't lock anyone out, it just quietly stops being enforced.
        // fail2ban is the actual deterrent here, so make sure it's set up
        // the first time Option B gets used, not just if the operator
        // separately remembered step 2. Gated on installed+active so
        // editing the allow-list later doesn't keep restarting fail2ban —
        // systemctl restart briefly tears down its iptables chain on stop
        // before rebuilding it on start, which for a moment really does
        // un-firewall every currently-banned IP, not just relog it.
        if (!fail2ban.installed || !fail2ban.active) setupFail2ban()
      }
    } finally { setLockBusy(false) }
  }
  async function applyTunnelOnlyLockdown() {
    const already = droplet.sshLockedDown && !droplet.sshTunnelOnly
    const firewallOnly = droplet.sshTunnelOnly && !droplet.sshFirewallGate
    const msg = firewallOnly
      ? `Add a network-level firewall gate on top of the existing sshd-level restriction?<br><br>` +
        `Port 22 will be dropped outright for anyone not on the tunnel, instead of merely refused by sshd — the same allow-list, enforced one layer earlier. ` +
        `ProxyCTL verifies a fresh connection over the tunnel immediately after and reverts just the firewall change if that fails; sshd's own restriction is untouched either way.`
      : already
      ? `Switch SSH over from your current public-IP allow-list to the control tunnel only?<br><br>` +
        `After this, SSH will <b>only</b> be reachable through ProxyCTL's tunnel — not from your laptop directly over the internet, and not from any of the IPs currently allow-listed. ` +
        `ProxyCTL verifies a fresh connection over the tunnel immediately after and reverts back to your current allow-list if that fails. You can also recover via your provider's web console.`
      : `Restrict SSH on the droplet to ONLY ProxyCTL's control tunnel?<br><br>` +
        `After this, SSH will <b>only</b> be reachable through ProxyCTL's tunnel — direct SSH from your laptop over the internet won't work anymore. ` +
        `ProxyCTL verifies a fresh connection over the tunnel immediately after and reverts if that fails. You can also recover via your provider's web console.`
    if (!await ask(msg, { ok: firewallOnly ? 'Add firewall lockdown' : 'Restrict to tunnel only' })) return
    setTunnelLockBusy(true)
    setTunnelLockMsg({ pending: true })
    setTunnelLockOut('')
    try {
      const { data: r } = await postJSON('/api/droplet/lockdown-ssh-tunnel', {})
      const okText = r.firewallWarning
        ? '✓ sshd restriction verified — firewall layer warning below'
        : '✓ SSH restricted to the control tunnel + verified'
      setTunnelLockMsg({ ok: !!r.ok, text: r.ok ? okText : '✗ ' + (r.error || 'restrict failed') })
      const parts = []
      if (r.firewallWarning) parts.push(r.firewallWarning)
      if (r.stdout) parts.push(r.stdout)
      if (r.stderr) parts.push('--- stderr ---\n' + r.stderr)
      setTunnelLockOut(parts.join('\n').trim() || '(no output)')
      if (r.ok) loadDroplet()
    } finally { setTunnelLockBusy(false) }
  }
  async function setupControlTunnel() {
    setCtBusy(true)
    setCtMsg({ pending: true })
    setCtOut('')
    try {
      const { data: r } = await postJSON('/api/droplet/control-tunnel/setup', {})
      setCtMsg({ ok: !!r.ok, text: r.ok ? '✓ control tunnel verified' : '✗ ' + (r.message || r.error || `${r.step || 'setup'} step failed`) })
      const parts = []
      if (r.stdout) parts.push(r.stdout)
      if (r.stderr) parts.push('--- stderr ---\n' + r.stderr)
      setCtOut(parts.join('\n').trim() || '(no output)')
      loadControlTunnel()
    } finally { setCtBusy(false) }
  }
  async function setupFail2ban() {
    setF2bBusy(true)
    setF2bMsg({ pending: true })
    setF2bOut('')
    try {
      const { data: r } = await postJSON('/api/droplet/fail2ban/setup', {})
      setF2bMsg({ ok: !!r.ok, text: r.ok ? '✓ fail2ban installed + configured' : `✗ setup failed (exit ${r.exitCode})` })
      const parts = []
      if (r.stdout) parts.push(r.stdout)
      if (r.stderr) parts.push('--- stderr ---\n' + r.stderr)
      setF2bOut(parts.join('\n').trim() || '(no output)')
      loadFail2ban()
    } finally { setF2bBusy(false) }
  }
  async function unbanIP(ip) {
    if (!await ask(`Unban <span class="mono">${ip}</span> now?`, { ok: 'Unban' })) return
    setUnbanBusyIP(ip)
    try {
      const { data: r } = await postJSON('/api/droplet/fail2ban/unban', { ip })
      if (!r.ok) toast('✗ unban failed', true)
      loadFail2ban()
    } finally { setUnbanBusyIP('') }
  }
  async function cfSave() {
    if (!cfToken.trim()) { toast('Paste your Cloudflare token first', true); return }
    setCfBusy(true)
    setCfMsg({ pending: true })
    try {
      const { data: r } = await postJSON('/api/cf/token', { token: cfToken.trim() })
      if (r?.error) {
        setCfMsg({ ok: false, text: '✗ ' + r.error })
      } else {
        setCfMsg({ ok: true, text: '✓ token verified & saved' })
        setCfToken('')
        refreshCf()
      }
    } finally { setCfBusy(false) }
  }
  async function cfRetest() {
    setCfCheckState('checking')
    try {
      const { data: r } = await postJSON('/api/cf/test', {})
      setCfCheckState(r?.ok ? 'ok' : 'error')
      if (r?.ok) toast(`✓ token active${r.expiresOn ? (' · expires ' + r.expiresOn) : ''}`)
      else       toast(`✗ ${r?.error || ('status=' + r?.status)}`, true)
      await refreshCf()
    } catch {
      setCfCheckState('error')
    }
  }
  async function cfRemove() {
    if (!await ask('Remove the Cloudflare token from ProxyCTL? DNS automation stops working until you save a new one. Existing DNS records in Cloudflare are NOT touched.', { ok: 'Remove token' })) return
    await del('/api/cf/token')
    toast('Cloudflare token removed')
    refreshCf()
  }

  // Shared by the Cloudflare and Web Tunnel Re-check buttons: the button's
  // OWN label carries the result (Re-check → Checking… → Successful/Failed)
  // instead of a separate toast/hint doing the talking.
  const checkStateLabel = (state) =>
    state === 'checking' ? 'Checking…' : state === 'ok' ? 'Successful' : state === 'error' ? 'Failed' : 'Re-check'

  const cur = STEPS[step]
  const installCmd = droplet.publicKey
    ? `mkdir -m700 -p ~/.ssh && echo ${shellQ(droplet.publicKey)} >> ~/.ssh/authorized_keys`
    : ''

  return (
    <div id="setupWiz" className="setupwiz on">
      <div className="setup-hdr">
        <ProxyLogo />
        <h1>ProxyCTL</h1>
        <span className="crumb">First-time setup</span>
        <button className="sm" style={{ marginLeft: 'auto' }} onClick={onSignOut}>Sign out</button>
      </div>
      <div className="panel">
        <div className="crumbs">
          {[['storage','Storage'],['target','Connect'],['prep','Prepare'],['lockdown','Lock down'],['cf','Cloudflare'],['tunnel','Web Tunnel']].map(([k, lab], i) => {
            const idx = STEPS.indexOf(k)
            // While actively ON a step, reflect its real status right away
            // (e.g. Cloudflare/the web tunnel going green the moment the
            // check actually succeeds, not only after clicking past it).
            // Steps already behind you stay "done" regardless of whether
            // they were completed or explicitly skipped — skipping an
            // optional step is still "nothing more to do here right now".
            const satisfiedNow = {
              storage: storage?.mode !== 'ephemeral',
              target: !!droplet.configured,
              prep: !!droplet.bootstrapped,
              lockdown: !!droplet.sshLockedDown,
              cf: !!cfState.configured,
              tunnel: !!tunnel.cloudflaredReady,
            }[k]
            const state = idx < step ? 'done' : idx === step ? (satisfiedNow ? 'done' : 'active') : ''
            // Jumping straight to a step: always fine going backward (same
            // as "Back", just multi-step); jumping AHEAD is only offered
            // once the droplet's already bootstrapped at least once — a
            // genuine first-time setup still walks the required steps in
            // order.
            const clickable = idx <= step || droplet.bootstrapped
            return (
              <span key={k} className={'crumb ' + state + (clickable ? ' clickable' : '')}
                onClick={clickable ? () => setStep(idx) : undefined}>
                <span className="dot" />{i + 1} {lab}
              </span>
            )
          })}
        </div>
        <div className="body">
          {cur === 'welcome' && (
            <div className="step on">
              <h2>Welcome to ProxyCTL</h2>
              <p className="lede">A few small steps and you'll have a public droplet tunneling traffic into a Kubernetes service. First you'll pick the NFS share ProxyCTL keeps its WireGuard keys on; the droplet steps after that are required; lock-down, Cloudflare and the web tunnel are optional — skip any you don't need.</p>
              <p className="lede">ProxyCTL needs <strong>one thing on the droplet</strong>: a public SSH key it can use to reach root. We'll generate the key here (private half never leaves this app), give you a one-line command to install the public half on a fresh droplet, and take it from there.</p>
              <p className="lede">You can skip the wizard and configure everything from the admin page later — it's the same forms either way.</p>
            </div>
          )}

          {cur === 'target' && (
            <div className="step on">
              <h2>Connect to your droplet</h2>
              <p className="lede">ProxyCTL needs its own ed25519 keypair so it can SSH into the droplet on every Apply. The <strong>private half stays in ProxyCTL</strong> (no API ever returns it); the public half goes on the droplet via the one-line command below.</p>

              {/* A droplet (any cheap public VPS) is a hard requirement — this
                  collapsible spells out what to buy for operators who don't
                  have one yet, without cluttering the step for those who do. */}
              <details className="card setupcard" style={{ margin: '0 0 18px' }}>
                <summary><strong>Need a droplet?</strong> <span className="hint" style={{ margin: 0 }}>Required — any cheap public VPS works. Here's what to get.</span></summary>
                <div style={{ padding: '16px 20px' }}>
                  <p className="lede" style={{ marginTop: 0 }}>
                    ProxyCTL needs a small public server to receive traffic and forward it down the WireGuard tunnel.
                    The cheapest DigitalOcean droplet is plenty — it only shuffles packets.
                  </p>
                  <p style={{ margin: '0 0 12px' }}>
                    <a href="https://cloud.digitalocean.com/droplets/new" target="_blank" rel="noopener noreferrer">
                      Create a droplet on DigitalOcean →
                    </a>
                  </p>
                  <label>Recommended specs</label>
                  <ul style={{ margin: '6px 0 14px 18px', lineHeight: 1.7 }}>
                    <li><strong>Image:</strong> Ubuntu 24.04 (LTS) x64</li>
                    <li><strong>Plan:</strong> Basic, Regular CPU (SSD)</li>
                    <li><strong>Size:</strong> $4/mo — 1 vCPU, 512 MB RAM, 10 GB SSD, 500 GB transfer <span className="mono">(s-1vcpu-512mb-10gb)</span></li>
                    <li><strong>Region:</strong> whichever is closest to your players (lowest latency)</li>
                    <li><strong>Access:</strong> any method that gets you a root shell once — you'll paste ProxyCTL's install command below</li>
                  </ul>
                  <img src="/droplet-plan.png" alt="DigitalOcean droplet creation showing Ubuntu 24.04 LTS and the $4/mo Basic plan"
                       style={{ width: '100%', maxWidth: 720, borderRadius: 10, border: '1px solid var(--border)', display: 'block' }} />
                  <p className="hint" style={{ marginTop: 10 }}>
                    Bandwidth is the only spec worth sizing up: every byte your players send passes through the droplet,
                    and DigitalOcean charges for transfer over the included allowance.
                  </p>
                </div>
              </details>
              {!droplet.hasKey ? (
                <button className="primary" onClick={genKey}>Generate keypair</button>
              ) : (
                <>
                  <label>Run this on the droplet (root shell, or DO web console)</label>
                  <pre className="mono" style={{ userSelect: 'all' }}>{installCmd}</pre>
                  <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
                    <button className="sm" onClick={() => copyText(installCmd, toast)}>Copy one-line install</button>
                    <button className="sm danger" onClick={regen}>Regenerate keypair</button>
                  </div>
                  <p className="hint" style={{ marginTop: 8 }}>Regenerating rotates the keypair. The OLD public key stops working immediately — you'll have to re-run the install command on the droplet before SSH succeeds.</p>
                  <label style={{ marginTop: 18 }}>Droplet public IP</label>
                  <input value={ip} onChange={e => setIp(e.target.value)} placeholder="e.g. 192.0.2.123" />
                  <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10, marginTop: 6 }}>
                    <div><label>SSH user</label><input value={user} onChange={e => setUser(e.target.value)} placeholder="root" /></div>
                    <div><label>SSH port</label><input value={port} onChange={e => setPort(e.target.value)} placeholder="22" /></div>
                  </div>
                  <div style={{ marginTop: 14, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                    <button className="sm" onClick={saveAndTest}>Test connection</button>
                    <span className="hint" style={{ margin: 0 }}>
                      {testMsg?.pending && <><span className="spin" />testing ssh…</>}
                      {testMsg && !testMsg.pending && <span className={testMsg.ok ? 'ok' : 'bad'} dangerouslySetInnerHTML={{ __html: testMsg.text }} />}
                    </span>
                  </div>
                  {testOut && <pre className="mono">{testOut}</pre>}
                </>
              )}
            </div>
          )}

          {cur === 'prep' && (
            <div className="step on">
              <h2>Prepare the droplet (one-time)</h2>
              <p className="lede">ProxyCTL will SSH in and install <span className="mono">wireguard</span>, <span className="mono">iptables</span>, <span className="mono">conntrack</span>; persist the kernel sysctls; generate the droplet's WireGuard keypair (only if none exists); and bring up <span className="mono">wg-quick@wg0</span>. <strong>Idempotent</strong> — safe to re-run.</p>
              <label style={{ display: 'flex', alignItems: 'flex-start', gap: 10, cursor: 'pointer', fontSize: 13.5, textTransform: 'none', letterSpacing: 'normal', fontWeight: 400, color: 'var(--text-2)', margin: '14px 0' }}>
                <input type="checkbox" checked={bootUpgrade} onChange={e => setBootUpgrade(e.target.checked)} style={{ width: 'auto', marginTop: 3 }} />
                <span><strong style={{ color: 'var(--text)' }}>Also apply system updates</strong> (<span className="mono">apt update &amp;&amp; apt upgrade</span>) — recommended on a fresh droplet. Conservative: won't pull in new packages or kernel changes that would need a reboot. Adds 1–3 min depending on how stale the image is. Already-patched droplets see no change.</span>
              </label>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <button className="primary" disabled={bootBusy} onClick={bootstrap}>Prepare droplet</button>
                <span className="hint" style={{ margin: 0 }}>
                  {bootMsg?.pending && <><span className="spin" />running on the droplet ({bootMsg.upgrade ? 'up to ~5 min — apt upgrade runs first' : 'up to ~2 min'})…</>}
                  {bootMsg && !bootMsg.pending && <span className={bootMsg.ok ? 'ok' : 'bad'}>{bootMsg.text}</span>}
                </span>
              </div>
              {droplet.bootstrapped && (
                <div style={{ marginTop: 10 }} className="notice">
                  <strong>Droplet ready.</strong><br />
                  WireGuard public key (recorded):<br />
                  <span className="mono" style={{ userSelect: 'all', fontSize: 12 }}>{droplet.wgPublicKey || ''}</span>
                  {droplet.wanIface && (
                    <div style={{ marginTop: 10 }}>
                      Public interface (NAT rules bind to it):{' '}
                      {(droplet.wanIfaces?.length || 0) > 1 ? (
                        <select className="mono" style={{ width: 'auto', display: 'inline-block' }}
                          value={droplet.wanIfaceManual ? droplet.wanIface : ''}
                          onChange={async e => {
                            const { ok, data } = await postJSON('/api/droplet/wan-iface', { iface: e.target.value })
                            if (!ok) { toast(data?.error || 'failed', true); return }
                            toast(e.target.value
                              ? `✓ pinned ${e.target.value} — applies on the next Apply`
                              : '✓ back to auto-detection')
                            loadDroplet()
                          }}>
                          <option value="">Auto — detected: {droplet.wanIface}</option>
                          {droplet.wanIfaces.map(l => {
                            const name = l.split(' ')[0]
                            return <option key={name} value={name}>{l}{name === droplet.wanIface ? '  (detected)' : ''}</option>
                          })}
                        </select>
                      ) : (
                        <span className="mono">{droplet.wanIface}</span>
                      )}
                      {' '}<span className="hint" style={{ margin: 0 }}>
                        {droplet.wanIfaceManual
                          ? 'pinned by you — auto-detection is off for this droplet'
                          : 'auto-detected, re-checked on every Apply'}
                      </span>
                    </div>
                  )}
                </div>
              )}
              {bootOut && <pre className="mono">{bootOut}</pre>}
            </div>
          )}

          {cur === 'lockdown' && (
            <div className="step on">
              <h2>Lock down SSH</h2>
              <p className="lede">Three steps, in order — the recommended end-state (SSH reachable only through ProxyCTL's own tunnel, with brute-force bans as a second layer) needs all three:</p>

              {/* ufw.sshRules is server-filtered to ONLY specific-IP/CIDR
                  port-22 rules — a wide-open ("Anywhere") one is never
                  included and never offered for removal here. ProxyCTL's
                  own iptables chain depends on a wide-open port-22 UFW
                  rule existing downstream to actually accept traffic it
                  RETURNs; removing the wrong kind of rule breaks even
                  legitimate tunnel-sourced SSH, not just outside access
                  (see splitUFWSSHRules / isWideOpenUFWFrom in server/main.go). */}
              {ufw?.installed && ufw.active && ufw.sshRules?.length > 0 && (
                <div className="warn" style={{ marginBottom: 18 }}>
                  <strong>Resolve this first:</strong> this droplet has a UFW rule restricting port 22 to a specific IP,
                  from before ProxyCTL managed SSH access at all. It's invisible to — and unmanaged by — <strong>either</strong> option
                  below: if that IP ever changes, UFW itself cuts off your access, regardless of whether you end up on
                  Option A (recommended) or Option B. Removing it also makes sure a wide-open port-22 rule exists in its
                  place first — ProxyCTL's own mechanism needs that to keep working — so only ProxyCTL decides who
                  actually reaches SSH from there.
                  <pre className="mono" style={{ marginTop: 10, whiteSpace: 'pre-wrap' }}>{ufw.sshRules.map(r => r.raw).join('\n')}</pre>
                  <div style={{ marginTop: 10, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                    <button className="sm" disabled={ufwFixBusy} onClick={fixUFW}>Remove UFW SSH rule</button>
                    <span className="hint" style={{ margin: 0 }}>
                      {ufwFixMsg?.pending && <><span className="spin" />removing…</>}
                      {ufwFixMsg && !ufwFixMsg.pending && <span className={ufwFixMsg.ok ? 'ok' : 'bad'}>{ufwFixMsg.text}</span>}
                    </span>
                  </div>
                </div>
              )}

              <div className="warn" style={{ marginBottom: 18 }}>
                <strong>How these fit together:</strong> SSH is <strong>not</strong> open to everyone by default — steps 1 and 3 below decide who's allowed to connect <em>at all</em> (either the public IPs you list, or devices authenticated over the WireGuard tunnel — never both, never anyone else). fail2ban (step 2) is a separate, additional layer on top: it watches for repeated failed connection <em>attempts</em> and bans those source IPs at the firewall — it's about punishing bad behavior from whoever tries, not about deciding who's allowed to try in the first place.
              </div>

              {sshDrift?.drift && (
                <div className="warn" style={{ marginBottom: 18 }}>
                  {sshDrift.filePresent
                    ? <><strong>Drift detected:</strong> a tunnel-only lockdown file exists on this droplet, but ProxyCTL doesn't currently
                      believe SSH is restricted. Most likely a stale file left by an earlier attempt that didn't fully complete —
                      dormant for now, but it could silently go live on some later, unrelated sshd reload or reboot with no warning.</>
                    : <><strong>Drift detected:</strong> ProxyCTL believes SSH is restricted to tunnel-only, but the lockdown file is
                      missing from this droplet — SSH may currently be reachable from outside the tunnel despite what this page says.</>}
                  <div style={{ marginTop: 10, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                    <button className="sm" disabled={driftFixBusy} onClick={fixSSHDrift}>
                      {sshDrift.filePresent ? 'Remove stale file' : 'Re-apply tunnel-only lockdown'}
                    </button>
                    <span className="hint" style={{ margin: 0 }}>
                      {driftFixMsg?.pending && <><span className="spin" />fixing…</>}
                      {driftFixMsg && !driftFixMsg.pending && <span className={driftFixMsg.ok ? 'ok' : 'bad'}>{driftFixMsg.text}</span>}
                    </span>
                  </div>
                </div>
              )}

              <div className="notice" style={{ marginBottom: 18 }}>
                <StepHeader n={1} done={controlTunnel.ready} title="Control tunnel" />
                A dedicated WireGuard link ProxyCTL uses for its own SSH to the droplet, so that connection survives your home IP changing (the public-IP allow-list below can't). {controlTunnel.ready
                  ? <>Verified and preferred for every droplet-bound command; the public IP below stays as an automatic fallback.</>
                  : <>Not set up yet — droplet-bound commands use only the public IP until you run this.</>}
                <div style={{ marginTop: 10, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  <button className="sm" disabled={ctBusy} onClick={setupControlTunnel}>
                    {controlTunnel.ready ? 'Re-verify control tunnel' : 'Set up control tunnel'}
                  </button>
                  <span className="hint" style={{ margin: 0 }}>
                    {ctMsg?.pending && <><span className="spin" />pushing peer + verifying…</>}
                    {ctMsg && !ctMsg.pending && <span className={ctMsg.ok ? 'ok' : 'bad'}>{ctMsg.text}</span>}
                  </span>
                </div>
                {ctOut && <pre className="mono" style={{ marginTop: 10 }}>{ctOut}</pre>}
              </div>

              <div className="notice" style={{ marginBottom: 18 }}>
                <StepHeader n={2} done={fail2ban.installed && fail2ban.active} title="fail2ban" />
                Watches sshd's own auth failures and firewalls off anyone hammering it, independent of whichever SSH restriction you land on below. {fail2ban.installed
                  ? <>Bans after <strong>{fail2ban.maxRetry} failed attempts</strong> within <strong>{fail2ban.findTime}</strong> — <strong>permanently</strong>, first offense, no temporary grace period. {fail2ban.active ? 'Active.' : '⚠ installed but not active.'}</>
                  : <>Not installed yet.</>}
                <div style={{ marginTop: 10, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  <button className="sm" disabled={f2bBusy} onClick={setupFail2ban}>
                    {fail2ban.installed ? 'Re-apply fail2ban config' : 'Set up fail2ban'}
                  </button>
                  <span className="hint" style={{ margin: 0 }}>
                    {f2bMsg?.pending && <><span className="spin" />installing + configuring…</>}
                    {f2bMsg && !f2bMsg.pending && <span className={f2bMsg.ok ? 'ok' : 'bad'}>{f2bMsg.text}</span>}
                  </span>
                </div>
                {f2bOut && <pre className="mono" style={{ marginTop: 10 }}>{f2bOut}</pre>}

                {fail2ban.installed && (
                  <div style={{ marginTop: 14 }}>
                    <div style={{ fontSize: 13, opacity: 0.8, marginBottom: 6 }}>
                      Currently banned: {fail2ban.banned?.length || 0}
                      {' · '}failed attempts so far: {fail2ban.currentlyFailed ?? 0} (total {fail2ban.totalFailed ?? 0}, {fail2ban.totalBanned ?? 0} bans lifetime)
                    </div>
                    {(fail2ban.banned || []).length > 0 && (
                      <table className="mono" style={{ width: '100%', fontSize: 12.5, borderCollapse: 'collapse' }}>
                        <thead>
                          <tr style={{ textAlign: 'left', opacity: 0.7 }}>
                            <th style={{ padding: '4px 8px 4px 0' }}>IP</th>
                            <th style={{ padding: '4px 8px' }}>Location</th>
                            <th style={{ padding: '4px 0' }}></th>
                          </tr>
                        </thead>
                        <tbody>
                          {fail2ban.banned.map(b => (
                            <tr key={b.ip}>
                              <td style={{ padding: '4px 8px 4px 0' }}>{b.ip}</td>
                              <td style={{ padding: '4px 8px' }}>{[b.city, b.country].filter(Boolean).join(', ') || '—'}</td>
                              <td style={{ padding: '4px 0' }}>
                                <button className="sm" disabled={unbanBusyIP === b.ip} onClick={() => unbanIP(b.ip)}>
                                  {unbanBusyIP === b.ip ? 'un-banning…' : 'Unban'}
                                </button>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    )}
                  </div>
                )}
              </div>

              <StepHeader n={3} done={!!droplet.sshLockedDown} title="Restrict SSH access" />
              <p className="lede">Two separate ways to do this — pick one; applying either fully replaces whichever was active before, they're never combined.</p>

              {/* Option A: WireGuard tunnel-only — recommended. */}
              <div className="notice" style={{ marginBottom: 18 }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 8 }}>
                  <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                    <strong>Option A — WireGuard tunnel only</strong>
                    <span className="badge" style={{ margin: 0, border: '1px solid var(--ok)', background: 'var(--ok-bg)', color: 'var(--ok-tx)' }}>recommended</span>
                  </span>
                  {droplet.sshTunnelOnly && <span className="ok" style={{ fontSize: 12.5 }}>● Active</span>}
                </div>
                <p style={{ margin: '8px 0 0' }}>
                  SSH is reachable <strong>only</strong> through ProxyCTL's control tunnel and devices registered below — no public IP list to maintain.
                  {!controlTunnel.ready && ' Set up the control tunnel above first to unlock this option.'}
                </p>

                {controlTunnel.ready && !droplet.sshTunnelOnly && (
                  <div className="warn" style={{ marginTop: 10 }}>
                    <strong>What this changes:</strong> direct SSH from your laptop over the internet stops working — only the tunnel and devices below can reach port 22. Verified automatically after applying, with auto-revert on failure; your provider's console is always a fallback.
                  </div>
                )}

                {/* Registering a device comes BEFORE the restrict button so
                    the natural reading order nudges toward doing it first —
                    but it's not required. Some operators genuinely never
                    want personal SSH access at all and are fine relying on
                    ProxyCTL's own control tunnel alone; that's a legitimate
                    choice, not a mistake to block. */}
                {controlTunnel.ready && (
                  <div style={{ marginTop: 14 }}>
                    <PersonalAccessPanel />
                  </div>
                )}

                {controlTunnel.ready && (
                  <div style={{ marginTop: 14, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                    <button className="primary"
                      disabled={tunnelLockBusy || (droplet.sshTunnelOnly && droplet.sshFirewallGate)}
                      onClick={applyTunnelOnlyLockdown}>
                      {droplet.sshTunnelOnly
                        ? (droplet.sshFirewallGate ? 'Firewall gate active ✓' : 'Add firewall-level lockdown')
                        : droplet.sshLockedDown ? 'Switch to tunnel-only (recommended)' : 'Restrict to tunnel only (recommended)'}
                    </button>
                    <span className="hint" style={{ margin: 0 }}>
                      {tunnelLockMsg?.pending && <><span className="spin" />applying + verifying…</>}
                      {tunnelLockMsg && !tunnelLockMsg.pending && <span className={tunnelLockMsg.ok ? 'ok' : 'bad'}>{tunnelLockMsg.text}</span>}
                    </span>
                  </div>
                )}
                {tunnelLockOut && <pre className="mono" style={{ marginTop: 10 }}>{tunnelLockOut}</pre>}
              </div>

              {/* Option B: public-IP allow-list — the manually-managed alternative. */}
              <div className="notice">
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 8 }}>
                  <strong>Option B — Public-IP allow-list</strong>
                  {droplet.sshLockedDown && !droplet.sshTunnelOnly && <span className="ok" style={{ fontSize: 12.5 }}>● Active</span>}
                </div>
                <p style={{ margin: '8px 0 0' }}>
                  Restrict SSH to specific public IPs you list — everything else denied at the sshd level, no firewall rule
                  involved. Applying this also (re)asserts fail2ban's permanent-ban policy, since sshd-level filtering alone
                  just refuses a connection — fail2ban is what actually bans anyone who keeps trying. ProxyCTL's own
                  management access always rides its separate control tunnel, so it's unaffected if your IP changes — but{' '}
                  <strong>your own personal direct-SSH access isn't</strong>: if your IP ever changes (a router restart, a
                  DHCP lease renewal), you'll lose it until you come back and update this list yourself, the way the old
                  lockdown did before this tunnel existed. Option A doesn't have that problem — it authenticates by
                  WireGuard identity, not IP.
                  {droplet.sshTunnelOnly && ' Applying this switches AWAY from tunnel-only.'}
                </p>

                {droplet.sshLockedDown && !droplet.sshTunnelOnly && (
                  <div className="notice" style={{ marginTop: 10 }}>
                    Currently allowed:<br />
                    <span className="mono" style={{ userSelect: 'all', fontSize: 12.5 }}>{(droplet.sshAllowedIPs || []).join(', ')}</span>
                  </div>
                )}

                <div style={{ marginTop: 10 }}>
                  <button className="sm" onClick={() => setShowIPForm(v => !v)}>
                    {showIPForm ? 'Hide' : (droplet.sshLockedDown && !droplet.sshTunnelOnly ? 'Edit allow-list' : 'Use a public-IP allow-list instead')}
                  </button>
                  {showIPForm && (
                    <div style={{ marginTop: 14 }}>
                      <label>Allowed public IPs (comma-separated; CIDR OK)</label>
                      <input value={lockInput} onChange={e => setLockInput(e.target.value)} placeholder="203.0.113.42, 198.51.100.0/24" />
                      <div style={{ marginTop: 8, display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
                        <button className="sm" onClick={detectIP}>Detect ProxyCTL's public IP</button>
                        <span className="hint" style={{ margin: 0 }}>
                          {detectMsg?.pending && <><span className="spin" />asking ipify…</>}
                          {detectMsg && !detectMsg.pending && <span className={detectMsg.ok ? 'ok' : 'bad'}>{detectMsg.text}</span>}
                        </span>
                      </div>
                      <div className="warn" style={{ marginTop: 14 }}>
                        <strong>Important:</strong> include the IP ProxyCTL itself connects from (the "Detect" button finds it for you). If you leave it out, ProxyCTL itself gets locked out. ProxyCTL verifies its own access after applying and auto-reverts if it would be locked out — but you can also recover via your provider's web console at any time.
                      </div>
                      <div style={{ marginTop: 14, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                        <button className="primary" disabled={lockBusy} onClick={applyLockdown}>Apply lockdown</button>
                        <span className="hint" style={{ margin: 0 }}>
                          {lockMsg?.pending && <><span className="spin" />applying + verifying…</>}
                          {lockMsg && !lockMsg.pending && <span className={lockMsg.ok ? 'ok' : 'bad'}>{lockMsg.text}</span>}
                        </span>
                      </div>
                      {lockOut && <pre className="mono" style={{ marginTop: 10 }}>{lockOut}</pre>}
                    </div>
                  )}
                </div>
              </div>
            </div>
          )}

          {cur === 'cf' && (
            <div className="step on">
              <h2>Cloudflare DNS automation</h2>
              <p className="lede">Lets ProxyCTL create the grey-cloud A record for each tunnel entry automatically. Without this, you create the record by hand in your DNS provider. Skip if you don't use Cloudflare.</p>
              {cfState.configured ? (
                <div className="notice">
                  <strong>Cloudflare connected.</strong>{' '}
                  {cfState.verified
                    ? <>Status: <strong>{cfState.status || 'active'}</strong></>
                    : <>Verify failed: <span className="bad">{cfState.error || 'unknown'}</span></>}
                  {cfState.expiresOn && <> · expires <span className="mono">{cfState.expiresOn}</span></>}
                  <div style={{ marginTop: 6 }}>Accessible zones: <span className="mono" style={{ fontSize: 12.5 }}>{(cfState.zones || []).join(', ') || '(none — token may be misscoped)'}</span></div>
                  <div style={{ marginTop: 10, display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                    <button className="sm" disabled={cfCheckState === 'checking'} onClick={cfRetest}>{checkStateLabel(cfCheckState)}</button>
                    <button className="sm danger" onClick={cfRemove}>Remove token</button>
                  </div>
                </div>
              ) : (
                <>
                  <p className="lede" style={{ marginBottom: 6 }}><strong>Where do I get a token?</strong></p>
                  <ol style={{ margin: '0 0 14px 18px', color: 'var(--text-2)', fontSize: 13.5, lineHeight: 1.7 }}>
                    <li>Open <a href="https://dash.cloudflare.com/profile/api-tokens" target="_blank" rel="noopener" style={{ color: 'var(--link)' }}>dash.cloudflare.com/profile/api-tokens</a>.</li>
                    <li>Click <strong>Create Token</strong> → pick the <strong>"Edit zone DNS"</strong> template.</li>
                    <li>Under <strong>Zone Resources</strong>, restrict to the specific zone(s) ProxyCTL should manage.</li>
                    <li>Optional but recommended: lock the token to ProxyCTL's egress IP.</li>
                    <li>Click <strong>Continue to summary → Create Token</strong> and copy it <strong>immediately</strong>.</li>
                  </ol>
                  <p className="hint" style={{ margin: '0 0 14px' }}>Token requires <span className="mono">Zone:DNS:Edit</span> + <span className="mono">Zone:Zone:Read</span>. <strong>Do not use the Global API Key</strong>.</p>
                  <label>Cloudflare API token</label>
                  <input type="password" value={cfToken} onChange={e => setCfToken(e.target.value)} autoComplete="off" spellCheck="false" placeholder="paste your token here — never returned by the API after save" />
                  <div style={{ marginTop: 14, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                    <button className="primary" disabled={cfBusy} onClick={cfSave}>Verify &amp; save</button>
                    <span className="hint" style={{ margin: 0 }}>
                      {cfMsg?.pending && <><span className="spin" />verifying with Cloudflare…</>}
                      {cfMsg && !cfMsg.pending && <span className={cfMsg.ok ? 'ok' : 'bad'}>{cfMsg.text}</span>}
                    </span>
                  </div>
                  <p className="hint" style={{ marginTop: 8 }}>ProxyCTL verifies the token against Cloudflare before saving. Stored 0600 on the cluster PVC, never written to logs, never returned by any GET.</p>
                </>
              )}
            </div>
          )}

          {cur === 'tunnel' && (
            <div className="step on">
              <h2>Web Tunnel (Cloudflare)</h2>
              <p className="lede">Web apps (the <span className="mono">Web Apps</span> tab — <span className="mono">jellyfin.example.com</span> &rarr; a Service) are exposed through a <strong>Cloudflare Tunnel</strong>: a <span className="mono">cloudflared</span> connector runs in your cluster and dials out to Cloudflare's edge — no public IP, no certs, and Cloudflare's edge does TLS + WAF/DDoS. Skip this if you only run raw game-port tunnels.</p>
              {tunnel.cloudflaredReady ? (
                <div className="notice">
                  <strong>Cloudflare Tunnel connected.</strong>
                  <div style={{ marginTop: 6 }}>The <span className="mono">cloudflared</span> connector is running. Add web apps in the <strong>Web Apps</strong> tab — Apply publishes each one.</div>
                  <button className="sm" style={{ marginTop: 10 }} disabled={tunnelCheckState === 'checking'} onClick={refreshTunnel}>{checkStateLabel(tunnelCheckState)}</button>
                </div>
              ) : !tunnel.cfConfigured ? (
                <div className="warn">
                  Connect Cloudflare first — go back to the <strong>Cloudflare</strong> step and save an API token. For the tunnel the token also needs the <span className="mono">Account › Cloudflare Tunnel › Edit</span> permission (in addition to <span className="mono">Zone › DNS › Edit</span>).
                </div>
              ) : (
                <>
                  <p className="lede" style={{ marginBottom: 6 }}>ProxyCTL creates the tunnel via the Cloudflare API and deploys the <span className="mono">cloudflared</span> connector into its own namespace — fully automatic, no copy-paste, no extra RBAC.</p>
                  <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginTop: 8 }}>
                    <button className="primary" disabled={tunnelMsg?.pending} onClick={setupTunnel}>Set up Cloudflare Tunnel</button>
                    <span className="hint" style={{ margin: 0 }}>
                      {tunnelMsg?.pending && <><span className="spin" />creating tunnel + deploying connector…</>}
                      {tunnelMsg && !tunnelMsg.pending && <span className={tunnelMsg.ok ? 'ok' : 'bad'}>{tunnelMsg.text}</span>}
                    </span>
                  </div>
                  <p className="hint" style={{ marginTop: 10 }}>If this fails with a permissions error, add <span className="mono">Account › Cloudflare Tunnel › Edit</span> to your Cloudflare token and retry.</p>
                </>
              )}
            </div>
          )}

          {cur === 'storage' && storage?.mode === 'ephemeral' && (
            <div className="step on">
              <h2>Storage — where ProxyCTL lives</h2>
              <p className="lede">ProxyCTL is running on <strong>temporary storage</strong> right now — nothing survives a restart yet. Name the NFS share everything should live on (app data in <span className="mono">app/</span>, each tunnel's WireGuard keys in <span className="mono">Keys/</span>), test it, then move in. One directory to find, one to back up.</p>

              <label>NFS share</label>
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                <input className="mono" style={{ flex: '1 1 170px' }} spellCheck="false"
                  value={keysSrv} onChange={e => setKeysSrv(e.target.value)}
                  placeholder="NFS server, e.g. 10.0.0.5" />
                <input className="mono" style={{ flex: '2 1 220px' }} spellCheck="false"
                  value={keysExp} onChange={e => setKeysExp(e.target.value)}
                  placeholder="directory, e.g. /mnt/storage/ProxyCTL" />
              </div>
              <p className="hint" style={{ marginTop: 6 }}>
                Your NFS server's IP/hostname and the directory ProxyCTL should live in. If the
                directory doesn't exist yet, <strong>Test share</strong> creates it (its parent must exist).
              </p>
              <div className="notice" style={{ marginTop: 14 }}>
                <strong>Pick a secure location.</strong> Private keys will live here — ideally a directory
                only your cluster's nodes can mount. NFS mounts are made by the <em>nodes</em> (not pods),
                so restricting the export to the node IPs
                {shareSetup?.nodes?.length
                  ? <> — yours: <span className="mono">{shareSetup.nodes.map(n => n.ip).join(', ')}</span> —</>
                  : ' '}
                is enough. Not required: any share every node can reach works.
              </div>
              {shareSetup?.commands && (
                <details style={{ marginTop: 10 }}>
                  <summary className="hint" style={{ cursor: 'pointer', margin: 0 }}>Example: create an IP-restricted share (optional, run on the NFS server)</summary>
                  <pre className="mono" style={{ userSelect: 'all', whiteSpace: 'pre', overflowX: 'auto', marginTop: 8 }}>{shareSetup.commands}</pre>
                  <button className="sm" onClick={() => copyText(shareSetup.commands, toast)}>Copy commands</button>
                </details>
              )}
              <div style={{ marginTop: 14, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <button className="sm" disabled={probeMsg?.pending} onClick={probeShare}>Test share</button>
                <span className="hint" style={{ margin: 0 }}>
                  {probeMsg?.pending && <><span className="spin" />mounting from a probe pod (up to ~45s)…</>}
                  {probeMsg && !probeMsg.pending && <span className={probeMsg.ok ? 'ok' : 'bad'}>{probeMsg.text}</span>}
                </span>
              </div>
              {probeMsg?.detail && !probeMsg.pending && <pre className="mono" style={{ marginTop: 8 }}>{probeMsg.detail}</pre>}
              {probeMsg?.ok && probeMsg.existingData && (probeMsg.existingData.appFiles > 0 || probeMsg.existingData.keyDirs > 0) && (
                <div className="warn" style={{ marginTop: 8 }}>
                  This directory already has ProxyCTL data in it —{' '}
                  {probeMsg.existingData.appFiles > 0 && <><strong>{probeMsg.existingData.appFiles}</strong> existing app config file(s)</>}
                  {probeMsg.existingData.appFiles > 0 && probeMsg.existingData.keyDirs > 0 && ' and '}
                  {probeMsg.existingData.keyDirs > 0 && <><strong>{probeMsg.existingData.keyDirs}</strong> existing gateway key folder(s)</>}
                  . Moving in reuses it as-is — pick a different (empty) directory below if that's not what you want.
                </div>
              )}
              <div style={{ marginTop: 14, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <button className="primary"
                  disabled={adoptMsg?.pending || !(probeMsg?.ok && probeMsg.srv === keysSrv.trim() && probeMsg.exp === keysExp.trim())}
                  onClick={moveIntoShare}>
                  Save &amp; move in
                </button>
                <span className="hint" style={{ margin: 0 }}>
                  {adoptMsg?.pending && <><span className="spin" />{adoptMsg.text}</>}
                  {adoptMsg && !adoptMsg.pending && <span className={adoptMsg.ok === false ? 'bad' : 'ok'}>{adoptMsg.text}</span>}
                </span>
              </div>
              <p className="hint" style={{ marginTop: 8 }}>The test must pass before you can move in — adopting a share the nodes can't mount would leave ProxyCTL unable to start. Moving in restarts ProxyCTL onto the share; this page reconnects on its own (you may be asked to sign in again).</p>
              <details style={{ marginTop: 12 }}>
                <summary className="hint" style={{ cursor: 'pointer', margin: 0 }}>No NFS? Use cluster storage instead</summary>
                <p className="hint" style={{ margin: '8px 0 6px' }}>ProxyCTL creates a PVC on a StorageClass of your cluster. Keys then ride the same class via a derived per-gateway class. Must be a real StorageClass (<span className="mono">kubectl get sc</span>).</p>
                <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  <input className="mono" style={{ flex: '1 1 170px' }} spellCheck="false"
                    value={fallbackSC} onChange={e => setFallbackSC(e.target.value)}
                    placeholder={keysCfg?.baseStorageClass || 'e.g. local-path'} />
                  <button className="sm" disabled={adoptMsg?.pending || !fallbackSC.trim()}
                    onClick={() => adopt({ storageClass: fallbackSC.trim() })}>Use cluster storage</button>
                </div>
              </details>
            </div>
          )}

          {cur === 'storage' && storage?.mode !== 'ephemeral' && (
            <div className="step on">
              <h2>Storage — where ProxyCTL's keys live</h2>
              <p className="lede">Each tunnel ProxyCTL creates self-generates a <strong>WireGuard keypair</strong> that's persisted on NFS so the tunnel survives restarts. Point ProxyCTL at the share (and folder) the keys should live on — any share every cluster node can reach works.</p>

              <label>NFS share</label>
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                <input className="mono" style={{ flex: '1 1 170px' }} spellCheck="false"
                  value={keysSrv} onChange={e => setKeysSrv(e.target.value)}
                  placeholder={keysCfg?.discoveredNfsServer || 'e.g. 10.0.0.5'} />
                <input className="mono" style={{ flex: '2 1 220px' }} spellCheck="false"
                  value={keysExp} onChange={e => setKeysExp(e.target.value)}
                  placeholder={keysCfg?.discoveredNfsExport || 'e.g. /mnt/ssd'} />
              </div>
              <p className="hint" style={{ marginTop: 6 }}>
                Server and export path of the NFS share to keep keys on.
                {keysCfg?.discoveredNfsExport && (
                  <> Leave blank to use the one your install already uses
                    (<span className="mono">{keysCfg.discoveredNfsServer}:{keysCfg.discoveredNfsExport}</span>).</>
                )}
              </p>

              <label style={{ marginTop: 12 }}>Keys folder (under that share)</label>
              <input className="mono" value={keysVal} onChange={e => setKeysVal(e.target.value)}
                placeholder={keysCfg?.defaultBasePath || 'e.g. ProxyCTL/Keys'} spellCheck="false" />
              {keysCfg && (
                <p className="hint" style={{ marginTop: 8 }}>
                  {/* Typed share wins; otherwise the root discovered from this
                      cluster's provisioner. When neither is known, say only what
                      IS known rather than printing a made-up absolute path. */}
                  {(() => {
                    const srv = keysSrv.trim() || keysCfg.discoveredNfsServer
                    const exp = (keysExp.trim() || keysCfg.discoveredNfsExport || '').replace(/\/$/, '')
                    const folder = keysVal.trim() || keysCfg.defaultBasePath
                    return exp
                      ? <>Saves to <span className="mono">{exp}/{folder}</span>{srv && <> on <span className="mono">{srv}</span></>}</>
                      : <>Saves to <span className="mono">{folder}</span> under the volume root</>
                  })()}
                </p>
              )}
              <div className="notice" style={{ marginTop: 14 }}>
                <strong>Pick a secure location.</strong> These are private keys — ideally use a share that
                only your cluster's nodes can mount. NFS mounts are made by the <em>nodes</em> (not pods),
                so restricting the export to the node IPs
                {shareSetup?.nodes?.length
                  ? <> — yours: <span className="mono">{shareSetup.nodes.map(n => n.ip).join(', ')}</span> —</>
                  : ' '}
                is enough. Not required: an open share works too, it's just readable by anything that can
                mount it. If you do restrict by IP and add a node later, ProxyCTL warns you here and on the
                <strong> Gateway keys</strong> card with an updated exports line.
              </div>
              {shareSetup?.commands && (
                <details style={{ marginTop: 10 }}>
                  <summary className="hint" style={{ cursor: 'pointer', margin: 0 }}>Example: create an IP-restricted share (optional, run on the NFS server)</summary>
                  <pre className="mono" style={{ userSelect: 'all', whiteSpace: 'pre', overflowX: 'auto', marginTop: 8 }}>{shareSetup.commands}</pre>
                  <button className="sm" onClick={() => copyText(shareSetup.commands, toast)}>Copy commands</button>
                </details>
              )}
              {keysCfg?.uncoveredNodes?.length > 0 && (
                <div className="warn" style={{ marginTop: 14 }}>
                  <strong>Node{keysCfg.uncoveredNodes.length === 1 ? '' : 's'} added since the share was saved:</strong>{' '}
                  <span className="mono">{keysCfg.uncoveredNodes.map(n => `${n.name} (${n.ip})`).join(', ')}</span>
                  <div style={{ marginTop: 8 }}>If your share is restricted by IP, it doesn't cover {keysCfg.uncoveredNodes.length === 1 ? 'this node' : 'these nodes'} yet — replace its line in <span className="mono">/etc/exports</span> with:</div>
                  <pre className="mono" style={{ userSelect: 'all', whiteSpace: 'pre', overflowX: 'auto', marginTop: 6 }}>{keysCfg.updatedExportsLine}</pre>
                  <div className="row" style={{ gap: 8, marginTop: 6, flexWrap: 'wrap' }}>
                    <button className="sm" onClick={() => copyText(keysCfg.updatedExportsLine, toast)}>Copy updated line</button>
                    <button className="sm" onClick={async () => {
                      const { ok, data } = await postJSON('/api/storage/share-setup/ack', {})
                      if (!ok) { toast(data?.error || 'failed', true); return }
                      toast('✓ share marked as covering all current nodes')
                      loadKeys()
                    }}>I've updated the exports</button>
                    <span className="hint" style={{ margin: 0 }}>then <span className="mono">exportfs -ra</span></span>
                  </div>
                </div>
              )}
              <div style={{ marginTop: 14, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <button className="primary" disabled={keysBusy || !keysVal.trim()} onClick={saveKeys}>Save location</button>
                <span className="hint" style={{ margin: 0 }}>{keysBusy && <><span className="spin" />creating the key volume…</>}</span>
              </div>
              <p className="hint" style={{ marginTop: 8 }}>Optional — leave the share blank to keep keys on the StorageClass your install already uses; clicking <strong>Next</strong> also saves a changed folder. You can adjust this any time from the main screen's <strong>Gateway keys</strong> card (and move existing tunnels into the new folder).</p>
            </div>
          )}

          {cur === 'done' && (
            <div className="step on">
              <h2>All set 🎉</h2>
              <p className="lede">Your droplet is bootstrapped and ProxyCTL is connected. From here you can add a tunnel entry, point it at a Kubernetes Service, and Apply.</p>
              <p className="hint">A "Setup wizard" pill in the corner will let you re-open it if you need to.</p>
            </div>
          )}
        </div>

        <div className="nav">
          <button style={{ visibility: step === 0 ? 'hidden' : 'visible' }} onClick={back}>Back</button>
          <span className="hint grow" style={{ margin: 0, textAlign: 'right' }}>{nextBlocker()}</span>
          {/* Skipping while still on the setup-mode emptyDir would look fine
              until the first restart wiped everything — so it's disabled. */}
          <button className="skip" disabled={storage?.mode === 'ephemeral'}
            title={storage?.mode === 'ephemeral' ? 'Pick storage first — everything is temporary until then' : undefined}
            onClick={skip}>Skip wizard</button>
          <button className="primary" disabled={!!nextBlocker()} onClick={next}>{nextLabel()}</button>
        </div>
      </div>
    </div>
  )
}
