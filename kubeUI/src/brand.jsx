// Shared brand mark — the ProxyCTL proxy tunnel (nested portal frames)
// leading into a Kubernetes ship's-helm wheel. Same motif as the logo /
// favicon (kubeUI/public/brand/), simplified to two frames so it stays
// legible at header size, drawn white for the orange brand chip.
export function ProxyLogo() {
  return (
    <span className="mark" aria-hidden="true">
      <svg viewBox="0 0 24 24" fill="none" stroke="#fff" strokeLinecap="round" strokeLinejoin="round">
        {/* proxy tunnel: two portal frames receding inward */}
        <rect x="2.5" y="2.9" width="19" height="19" rx="5.7" strokeWidth="1.5" />
        <rect x="6.25" y="5.85" width="11.5" height="11.5" rx="3.4" strokeWidth="1.4" />
        {/* kubernetes helm wheel: rim + 7 spokes + hub */}
        <circle cx="12" cy="11.4" r="3.5" strokeWidth="0.9" />
        <path strokeWidth="0.9" d="M12 11.4L12 7.9 M12 11.4L14.74 9.22 M12 11.4L15.41 12.18 M12 11.4L13.52 14.55 M12 11.4L10.48 14.55 M12 11.4L8.59 12.18 M12 11.4L9.26 9.22" />
        <circle cx="12" cy="11.4" r="0.95" fill="#fff" stroke="none" />
      </svg>
    </span>
  )
}
