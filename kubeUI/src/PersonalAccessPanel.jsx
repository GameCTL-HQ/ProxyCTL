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

      <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <input value={name} onChange={e => setName(e.target.value)} placeholder="Device name (e.g. Laptop)" style={{ maxWidth: 220 }}
          disabled={!droplet.controlTunnelReady} />
      </div>

      {droplet.controlTunnelReady && (() => {
        const slug = (name.trim() || 'mypc').toLowerCase().replace(/[^a-z0-9]+/g, '-')
        const linuxCmd = `# 1. Create the key — skip this if you already have one for this device.\n` +
          `#    (If a key already exists at this path, ssh-keygen will ask before overwriting it — it won't just clobber it silently.)\n` +
          `ssh-keygen -t ed25519 -f ~/.ssh/proxyctl-${slug} -C "proxyctl-${slug}"\n\n` +
          `# 2. View it to copy.\n` +
          `cat ~/.ssh/proxyctl-${slug}.pub`
        const winCmd = `# 1. Create the key — skip this if you already have one for this device.\n` +
          `#    (If a key already exists at this path, ssh-keygen will ask before overwriting it — it won't just clobber it silently.)\n` +
          `ssh-keygen -t ed25519 -f $env:USERPROFILE\\.ssh\\proxyctl-${slug} -C "proxyctl-${slug}"\n\n` +
          `# 2. View it to copy.\n` +
          `Get-Content $env:USERPROFILE\\.ssh\\proxyctl-${slug}.pub`
        return (
          <div style={{ marginTop: 10 }}>
            <div className="hint" style={{ margin: '0 0 6px' }}>
              Don't have a key for this device yet? Generate one locally — the private half never leaves this device — then paste the public half below.
            </div>
            <div style={{ display: 'flex', gap: 6, marginBottom: 6 }}>
              <button className={'sm' + (keyGenOS === 'linux' ? ' primary' : '')} onClick={() => setKeyGenOS('linux')}>Linux / macOS</button>
              <button className={'sm' + (keyGenOS === 'windows' ? ' primary' : '')} onClick={() => setKeyGenOS('windows')}>Windows (PowerShell)</button>
            </div>
            <pre className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{keyGenOS === 'linux' ? linuxCmd : winCmd}</pre>
            <p className="hint" style={{ margin: '6px 0 0' }}>Paste the output of step 2 — the one-line <span className="mono">.pub</span> file contents — into the field below.</p>
          </div>
        )
      })()}

      <div style={{ marginTop: 10 }}>
        <textarea value={sshPubKey} onChange={e => setSSHPubKey(e.target.value)} rows={2}
          placeholder="ssh-ed25519 AAAA... (paste this device's public key)"
          style={{ width: '100%', fontFamily: 'monospace', fontSize: 12.5, resize: 'vertical' }}
          disabled={!droplet.controlTunnelReady} />
      </div>

      <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginTop: 10 }}>
        <button className="sm" disabled={busy || !droplet.controlTunnelReady} onClick={generate}>Add device</button>
        {!droplet.controlTunnelReady && (
          <button className="sm" onClick={() => onOpenSetup?.('lockdown')}>Set up control tunnel</button>
        )}
        <span className="hint" style={{ margin: 0 }}>
          {msg?.pending && <><span className="spin" />working…</>}
          {msg && !msg.pending && <span className={msg.ok ? 'ok' : 'bad'}>{msg.text}</span>}
        </span>
      </div>
      {out && <pre className="mono" style={{ marginTop: 10 }}>{out}</pre>}
      {config && (
        <div style={{ marginTop: 14 }}>
          <div className="warn" style={{ marginBottom: 10 }}>
            <strong>Save this now</strong> — it isn't shown again. Import it into WireGuard on {config.name}, then use the
            commands below to activate the tunnel and connect.
          </div>

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
            return (
              <div style={{ marginTop: 14 }}>
                <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 6 }}>Activate it from the command line</div>
                <div style={{ display: 'flex', gap: 6, marginBottom: 6 }}>
                  <button className={'sm' + (wgUpOS === 'linux' ? ' primary' : '')} onClick={() => setWgUpOS('linux')}>Linux</button>
                  <button className={'sm' + (wgUpOS === 'mac' ? ' primary' : '')} onClick={() => setWgUpOS('mac')}>macOS (Homebrew)</button>
                </div>
                <pre className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{wgUpOS === 'linux' ? linuxCmd : macCmd}</pre>
                <p className="hint" style={{ margin: '6px 0 0' }}>
                  Same block whether this is a fresh setup or replacing an earlier one — tearing down an interface/file
                  that doesn't exist yet is a harmless no-op. Assumes wireguard-tools is already installed and the .conf
                  above was downloaded to <span className="mono">~/Downloads</span> (adjust the path otherwise).
                  Windows and the official WireGuard app (macOS/mobile) are GUI-import only — use
                  "Import tunnel(s) from file..." instead.
                </p>
              </div>
            )
          })()}
        </div>
      )}
    </div>
  )
}
