// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import DropletManagement from './DropletManagement.jsx'
import { UIProvider } from './ui.jsx'

const statusPayload = {
  configured: true,
  reachable: true,
  checkedAt: 1793024000,
  ip: '147.182.135.128',
  status: {
    hostname: 'game-edge-1',
    os: 'Ubuntu 24.04.3 LTS',
    kernel: '6.8.0-88-generic',
    uptimeSeconds: 90061,
    load1: 0.12,
    load5: 0.34,
    load15: 0.56,
    cpuCores: 2,
    cpuUsagePct: 17.5,
    memoryTotal: 4294967296,
    memoryUsed: 1073741824,
    memoryAvailable: 3221225472,
    diskTotal: 53687091200,
    diskUsed: 10737418240,
    diskAvailable: 42949672960,
    wireguardActive: true,
    sshActive: true,
    unattendedActive: true,
    rebootRequired: true,
    pendingUpdates: 3,
    wgVersion: 'wireguard-tools v1.0.20210914',
    cloudflaredActive: true,
    cloudflaredInstalled: true,
    cloudflaredVersion: 'cloudflared version 2026.10.4 (built 2026-10-26)',
  },
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('Droplet Management', () => {
  it('shows services and updates without duplicating graphed resource counters', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(statusPayload), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })))

    render(<UIProvider><DropletManagement /></UIProvider>)

    expect(await screen.findByText('game-edge-1')).toBeTruthy()
    // CPU, RAM, and Disk current values belong to the time-series graphs;
    // do not duplicate them as standalone cards above the graphs.
    expect(screen.queryByText('17.5%')).toBeNull()
    expect(screen.queryByText('1 GB / 4 GB')).toBeNull()
    expect(screen.queryByText('10 GB / 50 GB')).toBeNull()
    expect(screen.getByText('● WireGuard')).toBeTruthy()
    expect(screen.getByText(/SSH/)).toBeTruthy()
    expect(screen.getByText(/Automatic updates/)).toBeTruthy()
    expect(screen.getByText(/Native cloudflared/)).toBeTruthy()
    expect(screen.getByText(/2026.10.4/)).toBeTruthy()
    expect(screen.getByText(/Reboot required/i)).toBeTruthy()
    expect(screen.getByRole('button', { name: /Update droplet/i })).toBeTruthy()
    expect(screen.getByRole('button', { name: /Reboot droplet/i })).toBeTruthy()
    await waitFor(() => expect(fetch).toHaveBeenCalledWith('/api/droplet/management', expect.anything()))
  })

  it('surfaces parser warnings and disables reboot while maintenance is busy', async () => {
    const partial = { ...statusPayload, complete: false, warning: 'Some droplet signals could not be parsed.', busy: 'update' }
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(partial), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })))

    render(<UIProvider><DropletManagement /></UIProvider>)
    expect(await screen.findByText(/Some droplet signals/i)).toBeTruthy()
    expect(screen.getByRole('button', { name: /Reboot droplet/i }).disabled).toBe(true)
  })

  it('shows an absent optional native connector as neutral instead of unhealthy', async () => {
    const noNative = { ...statusPayload, status: { ...statusPayload.status, cloudflaredInstalled: false, cloudflaredActive: false, cloudflaredVersion: '' } }
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(noNative), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })))

    render(<UIProvider><DropletManagement /></UIProvider>)
    const pill = await screen.findByText(/Native cloudflared · Not installed/i)
    expect(pill.className).toBe('pill')
  })

  it('does not call an installed connector not installed when its version is unavailable', async () => {
    const noVersion = { ...statusPayload, status: { ...statusPayload.status, cloudflaredInstalled: true, cloudflaredActive: true, cloudflaredVersion: '' } }
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(noVersion), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })))

    render(<UIProvider><DropletManagement /></UIProvider>)
    expect(await screen.findByText('Version unavailable')).toBeTruthy()
    expect(screen.queryByText(/Native cloudflared · Not installed/i)).toBeNull()
  })

  it('restores the WireGuard update spinner from server busy state after a refresh', async () => {
    const busyStatus = { ...statusPayload, busy: 'wireguard-image' }
    const fetchMock = vi.fn(async (path) => {
      if (path === '/api/wireguard/image') {
        return new Response(JSON.stringify({
          currentDigest: 'ghcr.io/gamectl-hq/wireguard-kube@sha256:3333333333333333333333333333333333333333333333333333333333333333',
        }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      return new Response(JSON.stringify(busyStatus), { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
    vi.stubGlobal('fetch', fetchMock)

    const { container } = render(<UIProvider><DropletManagement /></UIProvider>)
    expect(await screen.findByText(/cluster pods recreate per gateway/i)).toBeTruthy()
    expect(container.querySelector('.spin')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Updating…' }).disabled).toBe(true)
  })

  it('restores the droplet maintenance spinner from server busy state after a refresh', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ ...statusPayload, busy: 'update' }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })))

    const { container } = render(<UIProvider><DropletManagement /></UIProvider>)
    expect(await screen.findByText(/Updating droplet packages and Cloudflare connectors/i)).toBeTruthy()
    expect(container.querySelector('.spin')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Updating…' }).disabled).toBe(true)
  })

  it('restores the reboot view from server busy state even when the droplet is unreachable', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({
      configured: true,
      reachable: false,
      busy: 'reboot',
      error: 'ssh timed out',
    }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })))

    const { container } = render(<UIProvider><DropletManagement /></UIProvider>)
    expect((await screen.findAllByText(/Rebooting \/ reconnecting/i)).length).toBeGreaterThan(0)
    expect(container.querySelector('.spin')).toBeTruthy()
    expect(screen.queryByText(/Droplet unreachable/i)).toBeNull()
  })

  it('requires confirmation and runs the droplet update with package upgrades enabled', async () => {
    const fetchMock = vi.fn(async (path, init = {}) => {
      if (path === '/api/droplet/bootstrap') {
        return new Response(JSON.stringify({ ok: true, stdout: 'PROXYCTL_REBOOT_REQUIRED=true', stderr: '' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      return new Response(JSON.stringify(statusPayload), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<UIProvider><DropletManagement /></UIProvider>)
    fireEvent.click(await screen.findByRole('button', { name: /Update droplet/i }))
    expect(screen.getByText(/newest stable release/i)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Update all' }))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/droplet/bootstrap', expect.objectContaining({
      method: 'POST',
      body: JSON.stringify({ upgrade: true }),
    })))
    expect((await screen.findAllByText(/Cloudflare updates completed/i)).length).toBeGreaterThan(0)
  })

  it('requires explicit confirmation before scheduling a remote reboot', async () => {
    const fetchMock = vi.fn(async (path) => {
      if (path === '/api/droplet/reboot') {
        return new Response(JSON.stringify({ ok: true, scheduled: true }), {
          status: 202,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      return new Response(JSON.stringify(statusPayload), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<UIProvider><DropletManagement /></UIProvider>)
    fireEvent.click(await screen.findByRole('button', { name: /Reboot droplet/i }))
    expect(screen.getByText(/disconnect active players/i)).toBeTruthy()
    // The reboot-required notice carries its own 'Reboot now' button; the
    // confirm dialog's is the last one rendered.
    fireEvent.click(screen.getAllByRole('button', { name: 'Reboot now' }).at(-1))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/droplet/reboot', expect.objectContaining({ method: 'POST' })))
    expect(await screen.findByText(/Rebooting \/ reconnecting/i)).toBeTruthy()
    expect((await screen.findAllByText(/Reboot scheduled/i)).length).toBeGreaterThan(0)
  })

  it('shows the cluster WireGuard image digests and flags a newer upstream build', async () => {
    const wgStatus = {
      currentDigest: 'ghcr.io/gamectl-hq/wireguard-kube@sha256:1111111111111111111111111111111111111111111111111111111111111111',
      upstreamRef: 'ghcr.io/gamectl-hq/wireguard-kube@sha256:2222222222222222222222222222222222222222222222222222222222222222',
      upstreamChecked: 1793024000,
      updateAvailable: true,
      droplet: { version: 'v1.0.20260223', path: '/usr/local/bin/wg', expected: 'v1.0.20260223', synced: true },
    }
    const fetchMock = vi.fn(async (path) => {
      if (path === '/api/wireguard/image') {
        return new Response(JSON.stringify(wgStatus), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      return new Response(JSON.stringify(statusPayload), { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<UIProvider><DropletManagement /></UIProvider>)
    expect(await screen.findByText('WireGuard')).toBeTruthy()
    expect(screen.getByText('sha256:111111111111…')).toBeTruthy()
    expect(screen.getByText('sha256:222222222222…')).toBeTruthy()
    expect(screen.getByText(/update applies this build/i)).toBeTruthy()
    expect(screen.getByText('Droplet wireguard')).toBeTruthy()
    expect(screen.getByText('synced with upstream source')).toBeTruthy()
    expect(screen.getByRole('button', { name: /Update WireGuard/i })).toBeTruthy()
  })

  it('shows a behind droplet wireguard version neutrally', async () => {
    const fetchMock = vi.fn(async (path) => {
      if (path === '/api/wireguard/image') {
        return new Response(JSON.stringify({ currentDigest: 'ghcr.io/gamectl-hq/wireguard-kube@sha256:3333333333333333333333333333333333333333333333333333333333333333', droplet: { version: 'v1.0.20210914', expected: 'v1.0.20260223', synced: false } }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      return new Response(JSON.stringify(statusPayload), { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<UIProvider><DropletManagement /></UIProvider>)
    expect(await screen.findByText('Droplet wireguard')).toBeTruthy()
    expect(screen.getByText('behind upstream v1.0.20260223')).toBeTruthy()
  })

  it('shows an unreachable upstream as a neutral state, not an error banner', async () => {
    const fetchMock = vi.fn(async (path) => {
      if (path === '/api/wireguard/image') {
        return new Response(JSON.stringify({ currentDigest: 'ghcr.io/gamectl-hq/wireguard-kube@sha256:3333333333333333333333333333333333333333333333333333333333333333', lookupError: 'ghcr manifest status 503' }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      return new Response(JSON.stringify(statusPayload), { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<UIProvider><DropletManagement /></UIProvider>)
    expect(await screen.findByText('WireGuard')).toBeTruthy()
    expect(screen.getByText('Unreachable')).toBeTruthy()
    expect(screen.getByText(/best-effort check failed/i)).toBeTruthy()
    expect(screen.getByRole('button', { name: /Update WireGuard/i }).disabled).toBe(false)
  })

  it('requires confirmation and posts the WireGuard image update on its own endpoint', async () => {
    const fetchMock = vi.fn(async (path, init = {}) => {
      if (path === '/api/wireguard/image/update') {
        return new Response(JSON.stringify({ ok: true, image: 'ghcr.io/gamectl-hq/wireguard-kube@sha256:1111111111111111111111111111111111111111111111111111111111111111', result: 'updated wireguard gateways: wg-gw-a, wg-gw-b; droplet wireguard v1.0.20260223 (synced)' }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      return new Response(JSON.stringify(statusPayload), { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<UIProvider><DropletManagement /></UIProvider>)
    fireEvent.click(await screen.findByRole('button', { name: /Update WireGuard/i }))
    expect(screen.getByText(/WireGuard keys and all configuration are preserved/i)).toBeTruthy()
    expect(screen.getByText(/remote droplet’s wireguard tools/i)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/wireguard/image/update', expect.objectContaining({
      method: 'POST',
      body: JSON.stringify({}),
    })))
    expect((await screen.findAllByText(/WireGuard updated/i)).length).toBeGreaterThan(0)
    await waitFor(() => expect(fetchMock.mock.calls.filter(([path]) => path === '/api/droplet/management').length).toBeGreaterThanOrEqual(2))
  })

  it('exposes the auto-update interval and persists the choice to the backend', async () => {
    let everyDays = 0
    const lastRun = 1793000000
    const fetchMock = vi.fn(async (path, init = {}) => {
      if (path === '/api/droplet/autoupdate') {
        if (init.method === 'PUT') {
          everyDays = JSON.parse(init.body).everyDays
        }
        return new Response(JSON.stringify({
          everyDays,
          lastRun: everyDays ? lastRun : 0,
          nextRun: everyDays ? lastRun + everyDays * 86400 : 0,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      return new Response(JSON.stringify(statusPayload), { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<UIProvider><DropletManagement /></UIProvider>)
    const select = await screen.findByRole('combobox', { name: /Auto-update droplet interval/i })
    expect(select.value).toBe('0')
    expect(screen.getByText(/only ever scheduled by you/i)).toBeTruthy()

    fireEvent.change(select, { target: { value: '7' } })
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/droplet/autoupdate', expect.objectContaining({
      method: 'PUT',
      body: JSON.stringify({ everyDays: 7 }),
    })))
    expect(await screen.findByText(/next Nov 2/i)).toBeTruthy()
  })

  it('routes the reboot-required notice into the existing reboot flow', async () => {
    const fetchMock = vi.fn(async (path) => {
      if (path === '/api/droplet/reboot') {
        return new Response(JSON.stringify({ ok: true, scheduled: true }), { status: 202, headers: { 'Content-Type': 'application/json' } })
      }
      return new Response(JSON.stringify(statusPayload), { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<UIProvider><DropletManagement /></UIProvider>)
    // The notice's inline button goes through the same confirmation gate as
    // the main Reboot droplet button — no silent reboots.
    fireEvent.click(await screen.findByRole('button', { name: 'Reboot now' }))
    expect(screen.getByText(/disconnect active players/i)).toBeTruthy()
    fireEvent.click(screen.getAllByRole('button', { name: 'Reboot now' }).at(-1))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/droplet/reboot', expect.objectContaining({ method: 'POST' })))
  })
})
