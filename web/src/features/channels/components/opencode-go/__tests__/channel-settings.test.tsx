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
const { OpenCodeAPIKeyChannelSettings } =
  await import('../../drawers/sections/opencode-api-key-channel-settings')
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

function getControlByLabel<T extends HTMLElement>(
  host: HTMLElement,
  label: string
): T {
  const labelElement = [
    ...host.querySelectorAll<HTMLLabelElement>('label'),
  ].find((candidate) => candidate.textContent?.trim() === label)
  assert.ok(labelElement?.htmlFor)
  const control = host.querySelector<T>(`[id="${labelElement.htmlFor}"]`)
  assert.ok(control)
  assert.equal(host.contains(control), true)
  return control
}

function setInputValue(input: HTMLInputElement, value: string): void {
  const setter = Object.getOwnPropertyDescriptor(
    HTMLInputElement.prototype,
    'value'
  )?.set
  assert.ok(setter)
  setter.call(input, value)
  input.dispatchEvent(new Event('input', { bubbles: true }))
}

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
    const loadAwareSwitch = switches.find(
      (control) =>
        control.getAttribute('aria-label') === 'Balance traffic by account load'
    )
    const identityProxySwitch = switches.find(
      (control) =>
        control.getAttribute('aria-label') ===
        'Use identity-scoped proxy sessions'
    )
    const usageConversionSwitch = switches.find(
      (control) =>
        control.getAttribute('aria-label') ===
        'Enable OpenAI-compatible Usage conversion'
    )
    const lifecycleSwitches = [
      'Enable China-deployed models',
      'Apply referral rewards',
      'Cancel subscription renewal',
    ].map((label) =>
      switches.find((control) => control.getAttribute('aria-label') === label)
    )
    const rewardLimit = getControlByLabel<HTMLInputElement>(
      host,
      'Referral rewards per run'
    )
    assert.ok(genericFailoverSwitch)
    assert.equal(genericFailoverSwitch.hasAttribute('disabled'), false)
    assert.notEqual(genericFailoverSwitch.getAttribute('aria-disabled'), 'true')
    assert.equal(genericFailoverSwitch.hasAttribute('data-disabled'), false)
    assert.ok(loadAwareSwitch)
    assert.equal(loadAwareSwitch.hasAttribute('disabled'), false)
    assert.notEqual(loadAwareSwitch.getAttribute('aria-disabled'), 'true')
    assert.equal(loadAwareSwitch.hasAttribute('data-disabled'), false)
    assert.ok(identityProxySwitch)
    assert.equal(identityProxySwitch.hasAttribute('disabled'), false)
    assert.notEqual(identityProxySwitch.getAttribute('aria-disabled'), 'true')
    assert.equal(identityProxySwitch.hasAttribute('data-disabled'), false)
    assert.ok(usageConversionSwitch)
    assert.equal(usageConversionSwitch.getAttribute('aria-checked'), 'true')
    assert.equal(usageConversionSwitch.hasAttribute('disabled'), false)
    assert.equal(lifecycleSwitches.length, 3)
    assert.equal(
      lifecycleSwitches.every(
        (control) =>
          control &&
          (control.disabled ||
            control.getAttribute('aria-disabled') === 'true' ||
            control.hasAttribute('data-disabled'))
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

  test('renders the shared Usage conversion switch for API Key channels', async () => {
    function Harness() {
      const form = useForm({
        defaultValues: {
          ...CHANNEL_FORM_DEFAULT_VALUES,
          opencode_go_billing_usage_conversion_enabled: false,
        },
      })
      return (
        <Form {...form}>
          <OpenCodeAPIKeyChannelSettings />
        </Form>
      )
    }

    const host = document.createElement('div')
    document.body.append(host)
    const root = createRoot(host)
    renderedSettings = { host, root }
    await act(async () => root.render(<Harness />))

    const usageConversionSwitch = host.querySelector<HTMLButtonElement>(
      '[role="switch"][aria-label="Enable OpenAI-compatible Usage conversion"]'
    )
    assert.ok(usageConversionSwitch)
    assert.equal(usageConversionSwitch.getAttribute('aria-checked'), 'false')
    assert.match(
      host.textContent || '',
      /Controls public Usage projection only; it does not change model pricing or internal settlement\./
    )

    await act(async () => usageConversionSwitch.click())
    assert.equal(usageConversionSwitch.getAttribute('aria-checked'), 'true')
  })

  test('infers the initial policy when identity proxy routing is enabled', async () => {
    function Harness() {
      const form = useForm({
        defaultValues: {
          ...CHANNEL_FORM_DEFAULT_VALUES,
          proxy:
            'http://account_custom_zone_GB_sid_template_time_20:secret@proxy.example:8080',
        },
      })
      return (
        <Form {...form}>
          <OpenCodeGoChannelSettings lifecyclePolicyReadOnly />
        </Form>
      )
    }

    const host = document.createElement('div')
    document.body.append(host)
    const root = createRoot(host)
    renderedSettings = { host, root }
    await act(async () => root.render(<Harness />))

    const identityProxySwitch = host.querySelector<HTMLButtonElement>(
      '[role="switch"][aria-label="Use identity-scoped proxy sessions"]'
    )
    const country = getControlByLabel<HTMLInputElement>(host, 'Proxy country')
    const rotation = getControlByLabel<HTMLInputElement>(
      host,
      'Rotation interval (minutes)'
    )
    assert.ok(identityProxySwitch)
    assert.equal(country.disabled, true)
    assert.equal(rotation.disabled, true)

    await act(async () => identityProxySwitch.click())

    assert.equal(identityProxySwitch.getAttribute('aria-checked'), 'true')
    assert.equal(country.disabled, false)
    assert.equal(rotation.disabled, false)
    assert.equal(country.value, 'GB')
    assert.equal(rotation.value, '20')

    await act(async () => {
      setInputValue(country, 'CA')
      setInputValue(rotation, '30')
    })
    assert.equal(country.value, 'CA')
    assert.equal(rotation.value, '30')

    await act(async () => setInputValue(country, 'u\u017f'))
    assert.equal(country.value, 'U\u017f')

    await act(async () => identityProxySwitch.click())
    await act(async () => identityProxySwitch.click())
    assert.equal(country.value, 'U\u017f')
    assert.equal(rotation.value, '30')
  })
})
