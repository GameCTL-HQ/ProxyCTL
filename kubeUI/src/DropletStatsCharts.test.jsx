// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import DropletStatsCharts from './DropletStatsCharts.jsx'

const NOW = Math.floor(Date.now() / 1000)

// 48h of 60s samples with a 30-minute unreachable gap in the middle.
function makeSamples() {
  const samples = []
  for (let i = 48 * 60; i >= 1; i--) {
    const t = NOW - i * 60
    const inGap = t > NOW - 2 * 3600 && t < NOW - 1.5 * 3600
    samples.push(inGap
      ? { t, reachable: false, cpuPct: 0, load1: 0, memPct: 0, diskPct: 0 }
      : { t, reachable: true, cpuPct: 10 + (i % 30), load1: 0.2, memPct: 40 + (i % 10), diskPct: 62 })
  }
  return samples
}

function stubFetch(payload) {
  const fetchMock = vi.fn(async (url) => new Response(JSON.stringify(payload(url)), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  }))
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('DropletStatsCharts', () => {
  it('renders one trend per resource with range toggles and live values', async () => {
    const fetchMock = stubFetch(url => {
      if (String(url).includes('/api/droplet/stats/history')) {
        return { configured: true, samples: makeSamples(), sampleSeconds: 60, retentionSeconds: 604800 }
      }
      return {}
    })

    render(<DropletStatsCharts />)

    expect(await screen.findByText(/CPU — now/)).toBeTruthy()
    expect(screen.getByText(/RAM — now/)).toBeTruthy()
    expect(screen.getByText(/Disk \/ — now/)).toBeTruthy()
    // three SVG trend charts
    const svgs = document.querySelectorAll('svg')
    expect(svgs.length).toBe(3)
    // all seven range toggles, 24h selected by default
    for (const label of ['1h', '3h', '6h', '12h', '24h', '2d', '3d']) {
      expect(screen.getByRole('button', { name: `Show last ${label}` })).toBeTruthy()
    }
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/droplet/stats/history', expect.anything()))
  })

  it('switches the visible window without refetching (client-side filter)', async () => {
    const fetchMock = stubFetch(url => ({ configured: true, samples: makeSamples(), sampleSeconds: 60, retentionSeconds: 604800 }))
    render(<DropletStatsCharts />)
    await screen.findByText(/CPU — now/)

    const callsBefore = fetchMock.mock.calls.length
    fireEvent.click(screen.getByRole('button', { name: 'Show last 3d' }))
    // refilter, not refetch: the chart still renders for the new window
    expect(screen.getByText(/CPU — now/)).toBeTruthy()
    expect(fetchMock.mock.calls.length).toBe(callsBefore)
  })

  it('shows a collecting notice until at least two samples exist in the window', async () => {
    stubFetch(() => ({ configured: true, samples: [], sampleSeconds: 60, retentionSeconds: 604800 }))
    render(<DropletStatsCharts />)
    expect(await screen.findByText(/Collecting samples/)).toBeTruthy()
    expect(document.querySelectorAll('svg').length).toBe(0)
  })

  it('breaks the line at unreachable gaps when the window widens', async () => {
    stubFetch(() => ({ configured: true, samples: makeSamples(), sampleSeconds: 60, retentionSeconds: 604800 }))
    render(<DropletStatsCharts />)
    await screen.findByText(/CPU — now/)
    // the gap sits ~2-2.5h back: 1h excludes it (one continuous run per
    // metric), so widening to 3d must add path segments for the broken runs
    fireEvent.click(screen.getByRole('button', { name: 'Show last 1h' }))
    const oneH = document.querySelectorAll('svg path').length
    fireEvent.click(screen.getByRole('button', { name: 'Show last 3d' }))
    expect(screen.getByText(/CPU — now/)).toBeTruthy()
    const threeD = document.querySelectorAll('svg path').length
    expect(threeD).toBeGreaterThan(oneH)
  })
})
