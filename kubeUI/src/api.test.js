// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from 'vitest'

const TOKEN_KEY = 'proxyctl_token'

describe('JWT storage', () => {
  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    vi.resetModules()
  })

  it('deletes a legacy localStorage token without reusing it', async () => {
    localStorage.setItem(TOKEN_KEY, 'legacy-token')

    const { getToken } = await import('./api.js')

    expect(localStorage.getItem(TOKEN_KEY)).toBeNull()
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull()
    expect(getToken()).toBeNull()
  })

  it('persists new tokens only for the current browser session', async () => {
    const { getToken, setToken } = await import('./api.js')

    setToken('session-token')

    expect(getToken()).toBe('session-token')
    expect(sessionStorage.getItem(TOKEN_KEY)).toBe('session-token')
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull()

    setToken(null)
    expect(getToken()).toBeNull()
  })
})
