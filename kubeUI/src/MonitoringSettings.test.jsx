// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import MonitoringSettings from './MonitoringSettings.jsx'

const config = {
  discordWebhookUrl: '',
  enabled: false,
  retentionDays: 30,
  checkIntervalSeconds: 90,
  failureThreshold: 4,
  quotaBytes: 0,
  quotaNearPct: 85,
  quotaResetDay: 0,
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('Monitoring settings', () => {
  it('loads and saves configurable reachability cadence with a DNS warning', async () => {
    const fetchMock = vi.fn(async (path, init = {}) => {
      if (path === '/api/monitoring/summary') {
        return new Response(JSON.stringify({ instances: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      if (path === '/api/alerts/config' && (!init.method || init.method === 'GET')) {
        return new Response(JSON.stringify(config), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      if (path === '/api/alerts/config' && init.method === 'PUT') {
        return new Response(JSON.stringify({ ok: true }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      throw new Error(`unexpected request: ${path}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<MonitoringSettings />)

    const interval = await screen.findByLabelText('Check interval (seconds)')
    const failures = screen.getByLabelText('Consecutive failures before down')
    expect(interval.value).toBe('90')
    expect(failures.value).toBe('4')
    expect(screen.getByText(/DNS cache, propagation, or resolver hiccups/i)).toBeTruthy()
    expect(screen.getByText(/history is capped at 365 days/i)).toBeTruthy()

    const retention = screen.getByDisplayValue('30')
    fireEvent.change(interval, { target: { value: '10' } })
    fireEvent.change(failures, { target: { value: '3' } })
    fireEvent.change(retention, { target: { value: '365' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/alerts/config', expect.objectContaining({
      method: 'PUT',
      body: expect.stringContaining('"checkIntervalSeconds":10'),
    })))
    const saveCall = fetchMock.mock.calls.find(([path, init]) => path === '/api/alerts/config' && init?.method === 'PUT')
    const saved = JSON.parse(saveCall[1].body)
    expect(saved.failureThreshold).toBe(3)
    expect(saved.retentionDays).toBe(60)
    await waitFor(() => expect(retention.value).toBe('60'))
    expect(screen.getByRole('button', { name: 'Save' }).disabled).toBe(true)
  })
})
