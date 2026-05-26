// Shared brand mark (tunnel → kube-wheel SVG). Used in the header and on
// the auth gate panel.
export function ProxyLogo() {
  return (
    <span className="mark" aria-hidden="true">
      <svg viewBox="0 0 24 24" fill="none" stroke="#fff" strokeLinecap="round" strokeLinejoin="round">
        <path d="M2.5 20.5V12a9.5 9.5 0 0 1 19 0v8.5" strokeWidth="1.5" strokeOpacity=".5" />
        <path d="M6.5 20.5V12.5a5.5 5.5 0 0 1 11 0v8" strokeWidth="1.5" strokeOpacity=".8" />
        <circle cx="12" cy="13" r="3.2" strokeWidth="1.5" />
        <g strokeWidth="1.25">
          <path d="M12 13V9.5" />
          <path d="M12 13l3.05 1.9" />
          <path d="M12 13l-3.05 1.9" />
          <path d="M12 13l3.3-.7" />
          <path d="M12 13l-3.3-.7" />
          <path d="M12 13l1.2 3.3" />
          <path d="M12 13l-1.2 3.3" />
        </g>
        <circle cx="12" cy="13" r="1" fill="#fff" stroke="none" />
      </svg>
    </span>
  )
}
