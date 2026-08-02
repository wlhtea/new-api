/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import assert from 'node:assert/strict'
import { after, afterEach, describe, test } from 'node:test'

import { Window } from 'happy-dom'

import type { OpenCodeGoIdentity } from '../../../lib/opencode-go-schemas'

const domWindow = new Window({ url: 'http://localhost' })
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLButtonElement',
  'HTMLFormElement',
  'HTMLInputElement',
  'HTMLTextAreaElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'KeyboardEvent',
  'PointerEvent',
  'MouseEvent',
  'FocusEvent',
  'CustomEvent',
  'MutationObserver',
  'ResizeObserver',
  'requestAnimationFrame',
  'cancelAnimationFrame',
  'getComputedStyle',
] as const

for (const key of domGlobals) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

const { act, useState } = await import('react')
const { createRoot } = await import('react-dom/client')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { OpenCodeGoCookieDialog, OpenCodeGoImportDialog } =
  await import('../opencode-go-account-dialogs')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

type RenderedDialog = {
  host: HTMLDivElement
  root: ReturnType<typeof createRoot>
}

let renderedDialog: RenderedDialog | null = null

function identityFixture(): OpenCodeGoIdentity {
  return {
    uid: 'identity-test',
    label: 'Test identity',
    email: 'identity@example.test',
    status: 'active',
    manual_enabled: true,
    has_auth_cookie: true,
    last_synced_at: 1,
    last_error: '',
    created_at: 1,
    updated_at: 1,
    workspaces: [],
  }
}

async function renderDialog(element: React.ReactNode): Promise<void> {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  renderedDialog = { host, root }

  await act(async () => {
    root.render(<I18nextProvider i18n={i18n}>{element}</I18nextProvider>)
  })
}

async function changeTextarea(
  textarea: HTMLTextAreaElement,
  value: string
): Promise<void> {
  await act(async () => {
    const valueSetter = Object.getOwnPropertyDescriptor(
      domWindow.HTMLTextAreaElement.prototype,
      'value'
    )?.set
    assert.ok(valueSetter)
    valueSetter.call(textarea, value)
    textarea.dispatchEvent(
      new domWindow.Event('input', { bubbles: true }) as unknown as Event
    )
  })
}

async function submitForm(formId: string): Promise<void> {
  const button = document.querySelector<HTMLButtonElement>(
    `button[form="${formId}"]`
  )
  assert.ok(button)
  await act(async () => {
    button.click()
    await Promise.resolve()
  })
}

afterEach(async () => {
  if (renderedDialog) {
    await act(async () => renderedDialog?.root.unmount())
    renderedDialog.host.remove()
    renderedDialog = null
  }
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('OpenCode Go account Cookie dialogs', () => {
  test('clears imported Cookies before invoking the request callback', async () => {
    const syntheticCookie = 'synthetic-auth-cookie-one'
    let submittedCookie = ''
    let inputValueDuringSubmit = 'not-observed'

    function ImportHarness() {
      const [open, setOpen] = useState(true)
      return (
        <OpenCodeGoImportDialog
          open={open}
          onOpenChange={setOpen}
          isSubmitting={false}
          onSubmit={(values) => {
            submittedCookie = values.authCookies
            inputValueDuringSubmit =
              document.querySelector<HTMLTextAreaElement>(
                'textarea[name="authCookies"]'
              )?.value ?? ''
          }}
        />
      )
    }

    await renderDialog(<ImportHarness />)
    const textarea = document.querySelector<HTMLTextAreaElement>(
      'textarea[name="authCookies"]'
    )
    assert.ok(textarea)
    await changeTextarea(textarea, syntheticCookie)
    await submitForm('opencode-go-import-form')

    assert.equal(submittedCookie, syntheticCookie)
    assert.equal(inputValueDuringSubmit, '')
    assert.equal(document.querySelector('textarea[name="authCookies"]'), null)
    assert.equal(document.body.textContent?.includes(syntheticCookie), false)
  })

  test('clears a replacement Cookie before invoking the request callback', async () => {
    const syntheticCookie = 'synthetic-auth-cookie-replacement'
    let submittedCookie = ''
    let inputValueDuringSubmit = 'not-observed'
    const identity = identityFixture()

    function CookieHarness() {
      const [open, setOpen] = useState(true)
      return (
        <OpenCodeGoCookieDialog
          identity={identity}
          open={open}
          onOpenChange={setOpen}
          isSubmitting={false}
          onSubmit={(_identity, authCookie) => {
            submittedCookie = authCookie
            inputValueDuringSubmit =
              document.querySelector<HTMLTextAreaElement>(
                'textarea[name="authCookie"]'
              )?.value ?? ''
          }}
        />
      )
    }

    await renderDialog(<CookieHarness />)
    const textarea = document.querySelector<HTMLTextAreaElement>(
      'textarea[name="authCookie"]'
    )
    assert.ok(textarea)
    await changeTextarea(textarea, syntheticCookie)
    await submitForm('opencode-go-cookie-form')

    assert.equal(submittedCookie, syntheticCookie)
    assert.equal(inputValueDuringSubmit, '')
    assert.equal(document.querySelector('textarea[name="authCookie"]'), null)
    assert.equal(document.body.textContent?.includes(syntheticCookie), false)
  })
})
