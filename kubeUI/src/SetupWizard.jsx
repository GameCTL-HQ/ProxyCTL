import { useCallback, useEffect, useState } from 'react'
import { apiJSON, postJSON, putJSON, del } from './api.js'
import { useUI } from './ui.jsx'
import { ProxyLogo } from './brand.jsx'

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

async function copyText(s, toast) {
  try {
    if (window.isSecureContext && navigator.clipboard) {
      await navigator.clipboard.writeText(s); toast('✓ copied'); return
    }
  } catch {}
  const ta = document.createElement('textarea')
  ta.value = s
  ta.style.cssText = 'position:fixed;left:-9999px;top:-9999px;opacity:0'
  ta.setAttribute('readonly', '')
  document.body.appendChild(ta)
  ta.select(); ta.setSelectionRange(0, s.length)
  let ok = false
  try { ok = document.execCommand('copy') } catch {}
  document.body.removeChild(ta)
  toast(ok ? '✓ copied' : "Couldn't copy automatically — select the text and press Ctrl-C", !ok)
}

export default function SetupWizard({ onFinish, onSignOut }) {
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

  const [cfToken, setCfToken] = useState('')
  const [cfMsg, setCfMsg] = useState(null)
  const [cfBusy, setCfBusy] = useState(false)

  const [tunnel, setTunnel] = useState({ cfConfigured: false, connectorPresent: false, cloudflaredReady: false })
  const [tunnelMsg, setTunnelMsg] = useState(null) // {ok, text} | {pending:true}

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

  const refreshCf = useCallback(async () => {
    const { ok, data } = await apiJSON('/api/cf/status')
    setCfState(ok && data ? data : { configured: false })
  }, [])

  const refreshTunnel = useCallback(async () => {
    const s = await apiJSON('/api/tunnel/status')
    setTunnel(s.ok && s.data ? s.data : { cfConfigured: false })
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
      if (st?.mode === 'ephemeral') setStep(STEPS.indexOf('storage'))
      else setStep(resumeStep(data || {}))
    })()
  }, [loadStorage])

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
    })
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
      if (r.ok) loadDroplet()
    } finally { setLockBusy(false) }
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
    const { data: r } = await postJSON('/api/cf/test', {})
    if (r?.ok) toast(`✓ token active${r.expiresOn ? (' · expires ' + r.expiresOn) : ''}`)
    else       toast(`✗ ${r?.error || ('status=' + r?.status)}`, true)
    refreshCf()
  }
  async function cfRemove() {
    if (!await ask('Remove the Cloudflare token from ProxyCTL? DNS automation stops working until you save a new one. Existing DNS records in Cloudflare are NOT touched.', { ok: 'Remove token' })) return
    await del('/api/cf/token')
    toast('Cloudflare token removed')
    refreshCf()
  }

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
            const state = idx < step ? 'done' : idx === step ? 'active' : ''
            return <span key={k} className={'crumb ' + state}><span className="dot" />{i + 1} {lab}</span>
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
                  <input value={ip} onChange={e => setIp(e.target.value)} placeholder="e.g. 203.0.113.10" />
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
              <p className="lede">Restrict the droplet's SSH to <strong>only the public IPs you list</strong>. Everything else gets denied at the sshd level (drop-in config, persists across reboots, no firewall changes).</p>
              {droplet.sshLockedDown ? (
                <div className="notice"><strong>SSH is locked down.</strong> Allow-listed IPs:<br />
                  <span className="mono" style={{ userSelect: 'all', fontSize: 12.5 }}>{(droplet.sshAllowedIPs || []).join(', ')}</span>
                </div>
              ) : (
                <>
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
                </>
              )}
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
                    <button className="sm" onClick={cfRetest}>Re-test</button>
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
                  <button className="sm" style={{ marginTop: 10 }} onClick={refreshTunnel}>Re-check</button>
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
              <div style={{ marginTop: 14, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <button className="primary"
                  disabled={adoptMsg?.pending || !(probeMsg?.ok && probeMsg.srv === keysSrv.trim() && probeMsg.exp === keysExp.trim())}
                  onClick={() => adopt({ nfsServer: keysSrv.trim(), nfsExport: keysExp.trim() })}>
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
