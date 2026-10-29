// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import { UIProvider, useUI } from './ui.jsx'

afterEach(cleanup)

function ConfirmationTrigger({ message }) {
  const { ask } = useUI()
  return <button onClick={() => { void ask(message) }}>Open confirmation</button>
}

describe('confirmation message HTML', () => {
  it('removes executable handlers, unsafe tags, URLs, styles, and arbitrary attributes', () => {
    render(
      <UIProvider>
        <ConfirmationTrigger message={'<img src="https://evil.example/x" onerror="alert(1)"><script>alert(2)</script><strong class="not-allowed-here">strong text</strong><span class="mono" onclick="alert(3)" style="color:red" data-secret="x">safe text</span><a href="javascript:alert(4)">link text</a>'} />
      </UIProvider>,
    )

    fireEvent.click(screen.getByText('Open confirmation'))

    const message = document.querySelector('#askbar .msg')
    expect(message.querySelector('img')).toBeNull()
    expect(message.querySelector('script')).toBeNull()
    expect(message.querySelector('a')).toBeNull()
    expect(message.querySelector('[onclick]')).toBeNull()
    expect(message.querySelector('[style]')).toBeNull()
    expect(message.querySelector('[data-secret]')).toBeNull()
    expect(message.querySelector('strong').hasAttribute('class')).toBe(false)
    expect(message.textContent).toContain('strong text')
    expect(message.textContent).toContain('safe text')
    expect(message.textContent).toContain('link text')
  })

  it('preserves only the approved confirmation formatting', () => {
    render(
      <UIProvider>
        <ConfirmationTrigger message={'<strong>bold</strong><em>emphasis</em><br><span class="mono extra">code</span>'} />
      </UIProvider>,
    )

    fireEvent.click(screen.getByText('Open confirmation'))

    const message = document.querySelector('#askbar .msg')
    expect(message.innerHTML).toBe('<strong>bold</strong><em>emphasis</em><br><span class="mono extra">code</span>')
  })
})
