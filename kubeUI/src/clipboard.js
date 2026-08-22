// Shared "copy to clipboard" helper — Clipboard API where available
// (requires a secure context), falling back to the classic hidden-textarea
// + execCommand('copy') trick everywhere else (older browsers, non-HTTPS).
export async function copyText(s, toast) {
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
