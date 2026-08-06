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

const domWindow = new Window({ url: 'http://localhost' })
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLButtonElement',
  'HTMLFieldSetElement',
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

const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { createInstance } = await import('i18next')
const { initReactI18next } = await import('react-i18next')
const { useForm } = await import('react-hook-form')
const { Form } = await import('@/components/ui/form')
const { CHANNEL_FORM_DEFAULT_VALUES } =
  await import('../../../lib/channel-form')
const { OpenCodeGoChannelSettings } =
  await import('../../drawers/sections/opencode-go-channel-settings')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

type RenderedSettings = {
  host: HTMLDivElement
  root: ReturnType<typeof createRoot>
}

let renderedSettings: RenderedSettings | null = null

afterEach(async () => {
  if (renderedSettings) {
    await act(async () => renderedSettings?.root.unmount())
    renderedSettings.host.remove()
    renderedSettings = null
  }
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('OpenCode Go channel settings', () => {
  test('keeps protocol routing editable while lifecycle policy is pool-managed', async () => {
    let openPolicyCount = 0

    function Harness() {
      const form = useForm({ defaultValues: CHANNEL_FORM_DEFAULT_VALUES })
      return (
        <Form {...form}>
          <OpenCodeGoChannelSettings
            lifecyclePolicyReadOnly
            onOpenLifecyclePolicy={() => {
              openPolicyCount += 1
            }}
          />
        </Form>
      )
    }

    const host = document.createElement('div')
    document.body.append(host)
    const root = createRoot(host)
    renderedSettings = { host, root }
    await act(async () => root.render(<Harness />))

    const protocolTextarea = host.querySelector<HTMLTextAreaElement>('textarea')
    const protocolSelect =
      host.querySelector<HTMLButtonElement>('[role="combobox"]')
    assert.ok(protocolTextarea)
    assert.ok(protocolSelect)
    assert.equal(protocolTextarea.disabled, false)
    assert.equal(protocolSelect.disabled, false)

    const switches = [
      ...host.querySelectorAll<HTMLButtonElement>('[role="switch"]'),
    ]
    const genericFailoverSwitch = switches.find(
      (control) =>
        control.getAttribute('aria-label') === 'Generic upstream failover'
    )
    const lifecycleSwitches = switches.filter(
      (control) => control !== genericFailoverSwitch
    )
    const rewardLimit = host.querySelector<HTMLInputElement>(
      'input[type="number"]'
    )
    assert.ok(genericFailoverSwitch)
    assert.equal(genericFailoverSwitch.hasAttribute('disabled'), false)
    assert.notEqual(genericFailoverSwitch.getAttribute('aria-disabled'), 'true')
    assert.equal(genericFailoverSwitch.hasAttribute('data-disabled'), false)
    assert.equal(lifecycleSwitches.length, 3)
    assert.equal(
      lifecycleSwitches.every(
        (control) =>
          control.disabled ||
          control.getAttribute('aria-disabled') === 'true' ||
          control.hasAttribute('data-disabled')
      ),
      true
    )
    assert.ok(rewardLimit)
    assert.equal(rewardLimit.disabled, true)

    const openPoolButton = [
      ...host.querySelectorAll<HTMLButtonElement>('button'),
    ].find((button) => button.textContent?.includes('Open account pool'))
    assert.ok(openPoolButton)
    await act(async () => openPoolButton.click())
    assert.equal(openPolicyCount, 1)
  })
})
