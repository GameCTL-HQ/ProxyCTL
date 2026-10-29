// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import MaintenanceUpdateBanner from './MaintenanceUpdateBanner.jsx'

const management = {
  configured: true,
  reachable: true,
  status: { pendingUpdates: 4, rebootRequired: false },
}

beforeEach(() => {
  localStorage.clear()
  vi.restoreAllMocks()
})
afterEach(cleanup)

describe('maintenance update notifications', () => {
  it('shows an in-app alert for droplet and WireGuard updates and opens management', async () => {
    vi.stubGlobal('fetch', vi.fn(async (path) => {
      const body = path === '/api/droplet/management'
        ? management
        : {
            updateAvailable: true,
            upstreamRef: 'ghcr.io/gamectl-hq/wireguard-kube@sha256:new',
            droplet: { version: 'v1.0.20210914', expected: 'v1.0.20260223', synced: false },
          }
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
    }))
    const open = vi.fn()

    render(<MaintenanceUpdateBanner onOpenDroplet={open} />)

    expect((await screen.findByRole('alert')).textContent).toContain('WireGuard update available')
    expect(screen.getByRole('alert').textContent).toContain('4 droplet package updates')
    fireEvent.click(screen.getByRole('button', { name: /Open Droplet Management/i }))
    expect(open).toHaveBeenCalledOnce()
  })

  it('stays hidden when the droplet and WireGuard are current', async () => {
    vi.stubGlobal('fetch', vi.fn(async (path) => {
      const body = path === '/api/droplet/management'
        ? { ...management, status: { pendingUpdates: 0, rebootRequired: false } }
        : { updateAvailable: false, droplet: { version: 'v1.0.20260223', expected: 'v1.0.20260223', synced: true } }
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
    }))

    render(<MaintenanceUpdateBanner onOpenDroplet={() => {}} />)

    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
  })

  it('dismisses only the current update fingerprint', async () => {
    vi.stubGlobal('fetch', vi.fn(async (path) => {
      const body = path === '/api/droplet/management'
        ? management
        : { updateAvailable: false, droplet: { synced: true, expected: 'v1.0.20260223' } }
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
    }))

    render(<MaintenanceUpdateBanner onOpenDroplet={() => {}} />)
    expect(await screen.findByRole('alert')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Later' }))
    expect(screen.queryByRole('alert')).toBeNull()
  })
})
