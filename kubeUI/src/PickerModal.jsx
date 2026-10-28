import { useEffect, useState } from 'react'
import { apiJSON } from './api.js'

// Read-only browse of cluster Services. Behind the same JWT-gated /api/
// routes as everything else; selecting a Service hands its ClusterIP +
// port shape back to the parent via onPick(svc).
export default function PickerModal({ open, onClose, onPick }) {
  const [namespaces, setNamespaces] = useState(null) // null = loading
  const [ns, setNs] = useState('')
  const [services, setServices] = useState(null)
  const [selected, setSelected] = useState(null)
  const [warning, setWarning] = useState('')

  useEffect(() => {
    if (!open) return
    setSelected(null); setWarning('')
    ;(async () => {
      const { ok, data } = await apiJSON('/api/kube/namespaces')
      if (!ok || data?.error) {
        setNamespaces([])
        setWarning(data?.error || 'Cluster not reachable from here — internal access required.')
        return
      }
      setNamespaces(data.namespaces || [])
    })()
  }, [open])

  useEffect(() => {
    if (!open || !ns) { setServices(null); return }
    setServices(null); setSelected(null)
    ;(async () => {
      const { ok, data } = await apiJSON('/api/kube/services?ns=' + encodeURIComponent(ns))
      if (!ok || data?.error) {
        setWarning(data?.error || 'Cluster not reachable.')
        setServices([])
        return
      }
      setServices(data.services || [])
    })()
  }, [open, ns])

  // Close when clicking the backdrop.
  function onBackdrop(e) { if (e.target.dataset.backdrop === '1') onClose() }

  if (!open) return null
  return (
    <div className="overlay open" data-backdrop="1" onClick={onBackdrop} id="pickerOverlay">
      <div className="modal">
        <div className="row" style={{ justifyContent: 'space-between' }}>
          <div>
            <h3>Pick a target Service</h3>
            <p className="sub" style={{ margin: 0 }}>Read-only browse of the live cluster using your ambient kubeconfig. Selecting a Service fills the target ClusterIP and offers its ports.</p>
          </div>
          <button className="x" onClick={onClose}>×</button>
        </div>
        <div>
          <div className="picker-grid">
            <div>
              <label style={{ marginBottom: 6 }}>Namespace
                <select value={ns} onChange={e => setNs(e.target.value)}>
                  {namespaces === null && <option>loading…</option>}
                  {namespaces !== null && <option value="">— select namespace —</option>}
                  {namespaces?.map(n => <option key={n} value={n}>{n}</option>)}
                </select>
              </label>
            </div>
            <div>
              <div className="list">
                {!ns && <div className="item">Choose a namespace…</div>}
                {ns && services === null && <div className="item">loading services…</div>}
                {services && services.length === 0 && <div className="item">No Services in this namespace.</div>}
                {services?.map((s, i) => {
                  const ports = (s.ports || []).map(p => `${p.port}/${p.protocol}${p.name ? (' ' + p.name) : ''}`).join(', ') || 'no ports'
                  const readyCls = s.ready === 'no selector' ? '' : (s.readyOK ? 'ready' : 'notready')
                  return (
                    <div key={s.name} className={'item' + (selected === i ? ' sel' : '')} onClick={() => { setSelected(i); setWarning(s.clusterIP && s.clusterIP !== 'None' ? '' : `${s.name} is headless (no ClusterIP). The tunnel needs a routable ClusterIP — pick a non-headless Service or set the target manually.`) }}>
                      <div className="svc-name">{s.name} <span className="pill">{s.type}</span> <span className={'pill ' + readyCls}>{s.ready}</span></div>
                      <div className="svc-meta">ClusterIP <span className="mono">{s.clusterIP || '(none)'}</span> · {ports}</div>
                    </div>
                  )
                })}
              </div>
            </div>
          </div>
          {warning && <div className="warn">{warning}</div>}
          {selected !== null && services && (
            <div className="row" style={{ marginTop: 10 }}>
              <button className="primary sm" onClick={() => onPick(services[selected])}>Use this Service</button>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
