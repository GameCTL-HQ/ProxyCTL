import { createContext, useCallback, useContext, useRef, useState } from 'react'
import DOMPurify from 'dompurify'

const CONFIRMATION_HTML = {
  ALLOWED_TAGS: ['strong', 'em', 'br', 'span'],
  ALLOWED_ATTR: ['class'],
  ALLOW_ARIA_ATTR: false,
  ALLOW_DATA_ATTR: false,
}

DOMPurify.addHook('uponSanitizeAttribute', (node, data) => {
  if (data.attrName === 'class' && node.nodeName !== 'SPAN') data.keepAttr = false
})

export const sanitizeConfirmationHtml = (html) =>
  DOMPurify.sanitize(html || '', CONFIRMATION_HTML)

// Imperative ask() + toast() API behind a React provider so any component
// can do `const { ask, toast } = useUI(); if (await ask('…')) …` — same
// shape as the original ask()/toast() globals in the embedded HTML, but
// state lives in React.

const Ctx = createContext(null)
export const useUI = () => useContext(Ctx)

export function UIProvider({ children }) {
  const [ask, setAsk] = useState(null)   // { html, ok, cancel, info, resolve }
  const [toast, setToast] = useState(null) // { msg, bad }
  const toastT = useRef(null)

  const askFn = useCallback((msg, opts = {}) => {
    return new Promise((resolve) => {
      setAsk({
        html: msg,
        ok: opts.ok || 'Confirm',
        cancel: opts.cancel || 'Cancel',
        info: !!opts.info,
        resolve,
      })
    })
  }, [])

  const toastFn = useCallback((msg, bad = false) => {
    setToast({ msg, bad })
    clearTimeout(toastT.current)
    toastT.current = setTimeout(() => setToast(null), 4500)
  }, [])

  return (
    <Ctx.Provider value={{ ask: askFn, toast: toastFn }}>
      {children}
      <div id="askbar" className={'askbar' + (ask ? ' on' : '') + (ask?.info ? ' info' : '')}>
        <span className="msg" dangerouslySetInnerHTML={{ __html: sanitizeConfirmationHtml(ask?.html) }} />
        <span className="acts">
          <button className="sm" onClick={() => { ask?.resolve(false); setAsk(null) }}>{ask?.cancel}</button>
          <button className="sm danger" onClick={() => { ask?.resolve(true); setAsk(null) }}>{ask?.ok}</button>
        </span>
      </div>
      <div id="toast" className={'toast' + (toast ? ' on' : '') + (toast?.bad ? ' bad' : '')}>
        {toast?.msg || ''}
      </div>
    </Ctx.Provider>
  )
}
