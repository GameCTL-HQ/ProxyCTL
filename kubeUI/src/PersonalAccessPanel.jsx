import { useCallback, useEffect, useState } from 'react'
import QRCode from 'qrcode'
import { apiJSON, postJSON } from './api.js'
import { useUI } from './ui.jsx'
import { copyText } from './clipboard.js'

// Ongoing management of who can SSH into the droplet over the control
// tunnel — deliberately NOT part of the Setup wizard (that's onboarding;
// adding a new phone six months later shouldn't mean re-entering Setup).
// Lives in the main app's "SSH Security" tab, self-contained like
// Fail2banPanel: fetches its own droplet state, no wizard plumbing needed.
//
// Each named device is its own WireGuard peer (own keypair, own address
// out of the reserved range — see personalAccessIPLow/High in
// server/render.go) — see server/personalaccess.go for the full model.
// --- Command snippet rendering ---------------------------------------
// Every snippet in this panel is meant to be pasted whole, so CmdBlock
// pairs lightweight syntax highlighting (comments, quoted strings,
// $variables / ~paths, leading command word) with its own Copy button.
// The highlighting is display-only — Copy always takes the raw text,
// comments included (they're shell-safe on every platform shown).

const CMD_TOKEN_RE = /("[^"]*"|'[^']*'|\$env:[A-Za-z_]+|\$[A-Za-z_][A-Za-z0-9_]*|~\/[^\s"']*)/g

function renderCmdLine(line, key) {
  if (/^\s*#/.test(line)) {
    return <span key={key} style={{ color: 'var(--muted-2)' }}>{line + '\n'}</span>
  }
  let head = null, rest = line
  const lead = /^(\s*)([A-Za-z][\w.-]*)/.exec(line)
  if (lead) {
    head = <span style={{ color: 'var(--link)' }}>{lead[0]}</span>
    rest = line.slice(lead[0].length)
  }
  const parts = rest.split(CMD_TOKEN_RE)
  return (
    <span key={key}>
      {head}
      {parts.map((p, i) => {
        if (!p) return null
        if (/^["']/.test(p)) return <span key={i} style={{ color: 'var(--ok-tx)' }}>{p}</span>
        if (/^[$~]/.test(p)) return <span key={i} style={{ color: 'var(--warn-tx)' }}>{p}</span>
        return <span key={i}>{p}</span>
      })}
      {'\n'}
    </span>
  )
}

function CmdBlock({ text, toast }) {
  return (
    <div style={{ position: 'relative' }}>
      <pre className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', margin: 0, paddingRight: 70 }}>
        {text.split('\n').map((line, i) => renderCmdLine(line, i))}
      </pre>
      <button className="sm" style={{ position: 'absolute', top: 8, right: 8 }}
        onClick={() => copyText(text, toast)}>Copy</button>
    </div>
  )
}

export default function PersonalAccessPanel({ onOpenSetup }) {
  const { ask, toast } = useUI()
  const [droplet, setDroplet] = useState(null)
  const [name, setName] = useState('')
  const [sshPubKey, setSSHPubKey] = useState('') // BYOK — the operator's own public key, pasted in; ProxyCTL never generates or sees a private key for this half
  const [keyGenOS, setKeyGenOS] = useState('linux') // which ssh-keygen snippet to show — cosmetic only
  const [wgUpOS, setWgUpOS] = useState('linux') // which wg-quick activation snippet to show — cosmetic only
  const [busy, setBusy] = useState(false)
  const [revokeBusyKey, setRevokeBusyKey] = useState('')
  const [msg, setMsg] = useState(null)
  const [out, setOut] = useState('')
  const [config, setConfig] = useState(null) // { name, text } — the WireGuard config, shown ONCE right after generate; never re-fetchable
  const [qrDataUrl, setQrDataUrl] = useState('') // generated client-side, on demand — the config text never leaves the browser for this

  const load = useCallback(() => {
    apiJSON('/api/droplet')
      .then(({ ok, data }) => { if (ok) setDroplet(data) })
      .catch(() => { /* soft-fail */ })
  }, [])

  useEffect(() => { load() }, [load])

  const generate = async () => {
    const n = name.trim()
    const pub = sshPubKey.trim()
    if (!n) { toast('Name this device first (e.g. "Laptop")', true); return }
    if (!pub) { toast("Paste this device's SSH public key first — see the commands above if you need to generate one", true); return }
    if (!await ask(`Create personal access for "${n}"?<br><br>You'll get a WireGuard config to import on that device. Its SSH key is the public key you pasted — the matching private key stays wherever you generated it, ProxyCTL never sees it.`, { ok: 'Generate' })) return
    setBusy(true)
    setMsg({ pending: true })
    setOut('')
    setConfig(null)
    setQrDataUrl('')
    try {
      const { data: r } = await postJSON('/api/droplet/personal-access/generate', { name: n, sshPubKey: pub })
      // r.config may be present even when r.ok is false (e.g. the sshd
      // allow-list update failed) — the WireGuard peer was still created
      // and this is the only chance to save its config, so show it
      // regardless of the overall step's success.
      if (r.config) {
        setConfig({ name: r.name, text: r.config })
        setName('')
        setSSHPubKey('')
        load()
      }
      if (r.ok && !r.warning) {
        setMsg({ ok: true, text: `✓ ${r.name || n} added — save the config below now, it won't be shown again` })
      } else {
        setMsg({ ok: !!r.ok, text: (r.ok ? '⚠ ' : '✗ ') + (r.warning || r.error || `${r.step || 'generate'} failed`) })
        const parts = []
        if (r.stdout) parts.push(r.stdout)
        if (r.stderr) parts.push('--- stderr ---\n' + r.stderr)
        setOut(parts.join('\n').trim())
      }
    } finally { setBusy(false) }
  }

  const revoke = async (pubKey, devName) => {
    if (!await ask(`Revoke ${devName}'s personal access peer? That device will no longer be able to reach the droplet over the tunnel.`, { ok: 'Revoke' })) return
    setRevokeBusyKey(pubKey)
    try {
      const { data: r } = await postJSON('/api/droplet/personal-access/revoke', { pubKey })
      setMsg({ ok: !!r.ok, text: r.ok ? `✓ ${devName} revoked` : '✗ ' + (r.error || 'revoke failed') })
      if (r.ok) load()
    } finally { setRevokeBusyKey('') }
  }

  const downloadBlob = (text, filename) => {
    const blob = new Blob([text], { type: 'text/plain' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = filename
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    URL.revokeObjectURL(url)
  }

  const download = () => downloadBlob(config.text, `proxyctl-${config.name.toLowerCase().replace(/[^a-z0-9]+/g, '-')}.conf`)

  // Generated client-side (the "qrcode" package, no network round-trip) —
  // the config text, private key included, never leaves the browser for
  // this. The official WireGuard mobile app's own "Scan from QR code"
  // import just wants the raw wg-quick config text encoded as-is.
  const toggleQR = async () => {
    if (qrDataUrl) { setQrDataUrl(''); return }
    try {
      setQrDataUrl(await QRCode.toDataURL(config.text, { width: 280, margin: 1 }))
    } catch {
      toast("Couldn't generate a QR code — download the .conf instead", true)
    }
  }

  if (!droplet?.configured) return null // nothing to show before the droplet exists

  const peers = droplet.personalAccessPeers || []

  return (
    <div className="card" style={{ marginBottom: 18 }}>
      <h3 style={{ margin: '0 0 6px', display: 'flex', alignItems: 'center', gap: 8 }}>
        Personal SSH access
        <span className="badge" style={{ margin: 0, border: '1px solid var(--warn)', background: 'var(--warn-bg)', color: 'var(--warn-tx)' }}>optional</span>
      </h3>
      <div className="warn" style={{ marginTop: 0, marginBottom: 14 }}>
        <p style={{ margin: '0 0 6px' }}>
          {droplet.sshTunnelOnly
            ? 'SSH is restricted to the control tunnel — devices below are the only other way in.'
            : droplet.controlTunnelReady
              ? "Optional — only needed if you want to SSH in yourself; ProxyCTL's own tunnel doesn't use this. Add a device now so it's ready when you restrict SSH."
              : 'Set up and verify the control tunnel above first before adding devices.'}
        </p>
        <p style={{ margin: 0 }}>
          Don't have WireGuard installed yet? <a href="https://www.wireguard.com/install/" target="_blank" rel="noopener noreferrer">Get it for Windows, macOS, Linux, Android, or iOS →</a>
        </p>
      </div>

      {peers.length > 0 && (
        <table className="mono" style={{ width: '100%', fontSize: 12.5, borderCollapse: 'collapse', marginBottom: 14 }}>
          <thead>
            <tr style={{ textAlign: 'left', opacity: 0.7 }}>
              <th style={{ padding: '4px 8px 4px 0' }}>Name</th>
              <th style={{ padding: '4px 8px' }}>Address</th>
              <th style={{ padding: '4px 8px' }}>Added</th>
              <th style={{ padding: '4px 0' }}></th>
            </tr>
          </thead>
          <tbody>
            {peers.map(p => (
              <tr key={p.pubKey}>
                <td style={{ padding: '4px 8px 4px 0' }}>{p.name}</td>
                <td style={{ padding: '4px 8px' }}>{p.ip}</td>
                <td style={{ padding: '4px 8px' }}>{p.createdAt ? new Date(p.createdAt * 1000).toLocaleDateString() : '—'}</td>
                <td style={{ padding: '4px 0' }}>
                  <button className="sm" disabled={revokeBusyKey === p.pubKey} onClick={() => revoke(p.pubKey, p.name)}>
                    {revokeBusyKey === p.pubKey ? 'revoking…' : 'Revoke'}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <div style={{ fontSize: 13.5, fontWeight: 700, margin: '4px 0 2px' }}>Add a new device</div>
      <p className="hint" style={{ margin: '0 0 12px' }}>
        Each device (laptop, desktop, phone) gets its own name, its own WireGuard config, and its own SSH key — so any one of them can be revoked later without touching the rest.
      </p>

      <div className="wstep">
        <p className="st">1 · Name the device</p>
        <input value={name} onChange={e => setName(e.target.value)} placeholder="Device name (e.g. Laptop)" style={{ maxWidth: 220 }}
          disabled={!droplet.controlTunnelReady} />
      </div>

      <div className="wstep" style={{ marginTop: 16 }}>
        <p className="st">2 · Its SSH public key</p>
        {droplet.controlTunnelReady && (() => {
          const slug = (name.trim() || 'mypc').toLowerCase().replace(/[^a-z0-9]+/g, '-')
          const linuxCmd = `# 1. Create the key — skip if this device already has one (ssh-keygen asks before overwriting).\n` +
            `ssh-keygen -t ed25519 -f ~/.ssh/proxyctl-${slug} -C "proxyctl-${slug}"\n\n` +
            `# 2. View the public half — paste its one line below.\n` +
            `cat ~/.ssh/proxyctl-${slug}.pub`
          const winCmd = `# 1. Make sure the .ssh folder exists (Windows' ssh-keygen won't create it itself).\n` +
            `if (!(Test-Path "$env:USERPROFILE\\.ssh")) { New-Item -ItemType Directory -Path "$env:USERPROFILE\\.ssh" | Out-Null }\n\n` +
            `# 2. Create the key — skip if this device already has one (ssh-keygen asks before overwriting).\n` +
            `ssh-keygen -t ed25519 -f "$env:USERPROFILE\\.ssh\\proxyctl-${slug}" -C "proxyctl-${slug}"\n\n` +
            `# 3. View the public half — paste its one line below.\n` +
            `Get-Content "$env:USERPROFILE\\.ssh\\proxyctl-${slug}.pub"`
          return (
            <div style={{ marginBottom: 10 }}>
              <div className="hint" style={{ margin: '0 0 6px' }}>
                Don't have a key for this device yet? Generate one locally — the private half never leaves this device.
              </div>
              <div style={{ display: 'flex', gap: 6, marginBottom: 6 }}>
                <button className={'sm' + (keyGenOS === 'linux' ? ' primary' : '')} onClick={() => setKeyGenOS('linux')}>Linux / macOS</button>
                <button className={'sm' + (keyGenOS === 'windows' ? ' primary' : '')} onClick={() => setKeyGenOS('windows')}>Windows (PowerShell)</button>
              </div>
              <CmdBlock text={keyGenOS === 'linux' ? linuxCmd : winCmd} toast={toast} />
            </div>
          )
        })()}
        <textarea value={sshPubKey} onChange={e => setSSHPubKey(e.target.value)} rows={2}
          placeholder="ssh-ed25519 AAAA... (paste this device's public key)"
          style={{ width: '100%', fontFamily: 'monospace', fontSize: 12.5, resize: 'vertical' }}
          disabled={!droplet.controlTunnelReady} />
      </div>

      <div className="wstep" style={{ marginTop: 16 }}>
        <p className="st">3 · Generate its access</p>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
          <button className="sm" disabled={busy || !droplet.controlTunnelReady} onClick={generate}>Add device</button>
          {!droplet.controlTunnelReady && (
            <button className="sm" onClick={() => onOpenSetup?.('lockdown')}>Set up control tunnel</button>
          )}
          <span className="hint" style={{ margin: 0 }}>
            {msg?.pending && <><span className="spin" />working…</>}
            {msg && !msg.pending && <span className={msg.ok ? 'ok' : 'bad'}>{msg.text}</span>}
          </span>
        </div>
      </div>
      {out && <pre className="mono" style={{ marginTop: 10 }}>{out}</pre>}
      {config && (
        <div style={{ marginTop: 18 }}>
          <div className="warn" style={{ marginBottom: 12 }}>
            <strong>{config.name} is added — two steps left, on that device.</strong> Save the config below now: it isn't shown again.
          </div>

          <div className="wstep">
            <p className="st">1 · Save the WireGuard config onto {config.name}</p>
            <pre className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{config.text}</pre>
            <div style={{ marginTop: 8, display: 'flex', gap: 8, flexWrap: 'wrap' }}>
              <button className="sm" onClick={download}>Download .conf</button>
              <button className="sm" onClick={() => copyText(config.text, toast)}>Copy</button>
              <button className="sm" onClick={toggleQR}>{qrDataUrl ? 'Hide QR code' : 'Show QR code'}</button>
            </div>
            <p className="hint" style={{ margin: '6px 0 0' }}>
              No cert on this ProxyCTL instance yet? Some browsers flag a .conf downloaded over plain HTTP as
              unrecognized/dangerous by default — click <strong>Keep</strong> / <strong>Allow</strong> if prompted, it's
              just the file type + unencrypted connection, nothing wrong with the file itself.
            </p>
            {qrDataUrl && (
              <div style={{ marginTop: 10 }}>
                <img src={qrDataUrl} alt={`WireGuard config QR code for ${config.name}`} width={280} height={280}
                  style={{ borderRadius: 8, background: '#fff', padding: 8 }} />
                <p className="hint" style={{ margin: '6px 0 0' }}>
                  In the WireGuard app on {config.name}: <strong>+</strong> → <strong>Scan from QR code</strong>.
                </p>
              </div>
            )}
          </div>

          {(() => {
            const confSlug = config.name.toLowerCase().replace(/[^a-z0-9]+/g, '-')
            const confFile = `proxyctl-${confSlug}.conf`
            // down+rm first, unconditionally: makes this the SAME block for
            // a brand-new setup (nothing to tear down, both no-op harmlessly)
            // and for replacing an existing one after revoke+re-add (a
            // stale local interface/config left over from an earlier
            // generation won't match the new peer — see the WireGuard
            // authentication-by-public-key note above).
            const sshCmd = `\n\n# Then SSH in — 10.8.0.1 is the droplet's address ON THE TUNNEL, not its public IP.\n` +
              `# (assumes the key from the "generate one locally" step above, at this path)\n` +
              `ssh -i ~/.ssh/proxyctl-${confSlug} root@10.8.0.1`
            const linuxCmd = `sudo wg-quick down proxyctl 2>/dev/null; sudo rm -f /etc/wireguard/proxyctl.conf\n` +
              `sudo install -m 600 ~/Downloads/${confFile} /etc/wireguard/proxyctl.conf\n` +
              `sudo wg-quick up proxyctl\n` +
              `sudo wg show proxyctl` + sshCmd
            const macCmd = `sudo wg-quick down proxyctl 2>/dev/null; sudo rm -f "$(brew --prefix)/etc/wireguard/proxyctl.conf"\n` +
              `sudo install -m 600 ~/Downloads/${confFile} "$(brew --prefix)/etc/wireguard/proxyctl.conf"\n` +
              `sudo wg-quick up proxyctl\n` +
              `sudo wg show proxyctl` + sshCmd
            // Windows: the official WireGuard app is GUI-import, so the
            // tunnel half is two comment lines, not commands — but the SSH
            // half still needs the Windows key path spelled out. -i points
            // at the PRIVATE key (no .pub): ssh sometimes tolerates the
            // .pub path when an agent holds the key, which makes it look
            // right until the one machine where it isn't.
            const winCmd = `# 1. Import the downloaded .conf in the WireGuard app first:\n` +
              `#    WireGuard → Add Tunnel → Import tunnel(s) from file… → ${confFile} → Activate\n\n` +
              `# 2. Then SSH in — 10.8.0.1 is the droplet's address ON THE TUNNEL, not its public IP.\n` +
              `#    (assumes the key from the "generate one locally" step above — the key file itself, NOT the .pub)\n` +
              `ssh -i "$env:USERPROFILE\\.ssh\\proxyctl-${confSlug}" root@10.8.0.1`
            const cmdFor = { linux: linuxCmd, mac: macCmd, windows: winCmd }
            return (
              <div className="wstep" style={{ marginTop: 16 }}>
                <p className="st">2 · Activate the tunnel on {config.name}, then connect</p>
                <div style={{ display: 'flex', gap: 6, marginBottom: 6 }}>
                  <button className={'sm' + (wgUpOS === 'linux' ? ' primary' : '')} onClick={() => setWgUpOS('linux')}>Linux</button>
                  <button className={'sm' + (wgUpOS === 'mac' ? ' primary' : '')} onClick={() => setWgUpOS('mac')}>macOS (Homebrew)</button>
                  <button className={'sm' + (wgUpOS === 'windows' ? ' primary' : '')} onClick={() => setWgUpOS('windows')}>Windows (PowerShell)</button>
                </div>
                <CmdBlock text={cmdFor[wgUpOS] || linuxCmd} toast={toast} />
                <p className="hint" style={{ margin: '6px 0 0' }}>
                  Same block whether this is a fresh setup or replacing an earlier one — tearing down an interface/file
                  that doesn't exist yet is a harmless no-op. Linux/macOS assume wireguard-tools is installed and the .conf
                  above was downloaded to <span className="mono">~/Downloads</span> (adjust the path otherwise).
                  Windows and the official WireGuard app (macOS/mobile) import the tunnel via the GUI, per the Windows tab.
                </p>
              </div>
            )
          })()}
        </div>
      )}
    </div>
  )
}
