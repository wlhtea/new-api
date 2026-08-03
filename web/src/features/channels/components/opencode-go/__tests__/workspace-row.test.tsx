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

import type { OpenCodeGoWorkspace } from '../../../lib/opencode-go-schemas'

const domWindow = new Window({ url: 'http://localhost' })
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLButtonElement',
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
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { TooltipProvider } = await import('@/components/ui/tooltip')
const { OpenCodeGoWorkspaceRow } = await import('../opencode-go-workspace-row')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

type RenderedRow = {
  host: HTMLDivElement
  root: ReturnType<typeof createRoot>
}

let renderedRow: RenderedRow | null = null

function workspaceFixture(
  overrides: Partial<OpenCodeGoWorkspace> = {}
): OpenCodeGoWorkspace {
  const now = 1_900_000_000
  return {
    uid: 'workspace-test',
    name: 'Test workspace',
    email: 'workspace@example.test',
    has_api_key: true,
    credential_status: 'ready',
    membership_status: 'active',
    subscription_ends_at: now + 86_400,
    renewal_cancelled_at: 0,
    renewal_checked_at: now,
    renewal_cancel_error: '',
    manual_enabled: true,
    effective_state: 'eligible',
    state_reason: '',
    health_observation: 'model_probe_succeeded',
    health_observed_at: now,
    cooldown_until: 0,
    quota_snapshot_status: 'complete',
    quota_fetched_at: now,
    quota_next_refresh_at: now + 3600,
    quota_recovery_at: now + 3600,
    quota_parser_version: 'console-v1',
    quota_error: '',
    quota_windows: [
      {
        kind: 'rolling',
        source: 'opencode_console_authoritative',
        used_percent: 25,
        remaining_percent: 75,
        reset_seconds: 3600,
        reset_at: now + 3600,
        fetched_at: now,
        amounts_authoritative: false,
        calculated_limit_usd: 12,
        calculated_used_usd: 3,
        calculated_remaining_usd: 9,
      },
      {
        kind: 'weekly',
        source: 'opencode_console_authoritative',
        used_percent: 50,
        remaining_percent: 50,
        reset_seconds: 7200,
        reset_at: now + 7200,
        fetched_at: now,
        amounts_authoritative: false,
        calculated_limit_usd: 30,
        calculated_used_usd: 15,
        calculated_remaining_usd: 15,
      },
      {
        kind: 'monthly',
        source: 'opencode_console_authoritative',
        used_percent: 75,
        remaining_percent: 25,
        reset_seconds: 10_800,
        reset_at: now + 10_800,
        fetched_at: now,
        amounts_authoritative: false,
        calculated_limit_usd: 60,
        calculated_used_usd: 45,
        calculated_remaining_usd: 15,
      },
    ],
    models: [],
    china_models_enabled: null,
    china_models_checked_at: now,
    china_models_error: '',
    referral_code: '',
    available_referral_rewards: 0,
    used_referral_rewards: 0,
    referral_reward_applied_at: 0,
    risk_detected_at: 0,
    risk_last_checked_at: now,
    last_synced_at: now,
    last_error: '',
    created_at: now - 3600,
    updated_at: now,
    ...overrides,
  }
}

async function renderWorkspace(
  workspace: OpenCodeGoWorkspace,
  canOperate = true,
  locale = 'en'
): Promise<HTMLDivElement> {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  renderedRow = { host, root }

  await act(async () => {
    root.render(
      <I18nextProvider i18n={i18n}>
        <TooltipProvider>
          <OpenCodeGoWorkspaceRow
            workspace={workspace}
            nowSeconds={1_900_000_000}
            locale={locale}
            canOperate={canOperate}
            canSensitiveWrite={false}
            busyKey={null}
            onRefresh={() => undefined}
            onRiskRecheck={() => undefined}
            onToggle={() => undefined}
            onSensitiveAction={() => undefined}
          />
        </TooltipProvider>
      </I18nextProvider>
    )
  })

  return host
}

afterEach(async () => {
  if (renderedRow) {
    await act(async () => renderedRow?.root.unmount())
    renderedRow.host.remove()
    renderedRow = null
  }
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('OpenCode Go workspace status row', () => {
  test('shows authoritative windows with calculated amounts and snapshot times', async () => {
    const host = await renderWorkspace(workspaceFixture())

    assert.equal(host.querySelectorAll('[data-state="complete"]').length, 3)
    assert.equal(host.textContent?.includes('Console %'), true)
    assert.equal(host.textContent?.includes('Calculated used'), true)
    assert.equal(host.textContent?.includes('$3.00'), true)
    assert.equal(host.textContent?.includes('$45.00'), true)
    assert.equal(host.querySelectorAll('time[dateTime]').length, 3)
    assert.equal(host.textContent?.includes('Snapshot time'), true)
  })

  test('accepts the Simplified Chinese interface locale when formatting dates', async () => {
    const host = await renderWorkspace(workspaceFixture(), true, 'zhCN')

    assert.equal(host.querySelectorAll('time[dateTime]').length, 3)
    assert.equal(host.querySelectorAll('[data-state="complete"]').length, 3)
  })

  test('renders a manually disabled workspace with an enable action', async () => {
    const host = await renderWorkspace(
      workspaceFixture({
        manual_enabled: false,
        effective_state: 'manual_disabled',
        health_observation: '',
      })
    )
    const section = host.querySelector<HTMLElement>('[data-workspace-state]')
    const enableButton = host.querySelector<HTMLButtonElement>(
      'button[aria-label="Enable"]'
    )

    assert.ok(section)
    assert.equal(section.dataset.workspaceState, 'manual_disabled')
    assert.equal(section.textContent?.includes('Manual disabled'), true)
    assert.ok(enableButton)
    assert.equal(enableButton.disabled, false)
  })

  test('shows a cooldown deadline without presenting recovery', async () => {
    const host = await renderWorkspace(
      workspaceFixture({
        effective_state: 'cooldown',
        state_reason: 'RPM limit reached',
        health_observation: 'rpm_limited',
        cooldown_until: 1_900_000_600,
      })
    )

    assert.equal(host.textContent?.includes('Cooling down'), true)
    assert.equal(host.textContent?.includes('Cooldown until'), true)
    assert.equal(host.textContent?.includes('RPM limit reached'), true)
    assert.equal(host.textContent?.includes('Recovered'), false)
  })

  test('marks an eligible cooldown-expired workspace as recovered', async () => {
    const host = await renderWorkspace(
      workspaceFixture({ health_observation: 'cooldown_expired' })
    )

    assert.equal(host.textContent?.includes('Eligible'), true)
    assert.equal(host.textContent?.includes('Recovered'), true)
  })

  test('disables ordinary controls without operate permission', async () => {
    const host = await renderWorkspace(workspaceFixture(), false)
    const ordinaryButtons = [
      'Refresh workspace',
      'Recheck risk',
      'Disable',
    ].map((label) =>
      host.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`)
    )

    for (const button of ordinaryButtons) {
      assert.ok(button)
      assert.equal(button.disabled, true)
    }
  })
})
