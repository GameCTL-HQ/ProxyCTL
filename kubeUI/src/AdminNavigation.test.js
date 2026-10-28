import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

const adminSource = readFileSync(new URL('./Admin.jsx', import.meta.url), 'utf8')

describe('admin navigation', () => {
  it('exposes Droplet Management as a first-class tab', () => {
    expect(adminSource).toContain("'droplet'")
    expect(adminSource).toContain("['droplet', 'Droplet Management', 'health · updates · reboot']")
    expect(adminSource).toContain("tab === 'droplet' && <DropletManagement />")
  })
})
