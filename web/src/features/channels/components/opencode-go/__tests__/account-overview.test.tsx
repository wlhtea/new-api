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

import type {
  OpenCodeGoIdentity,
  OpenCodeGoQuotaWindow,
  OpenCodeGoWorkspace,
} from '../../../lib/opencode-go-schemas'

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
const { OpenCodeGoIdentitySection } =
  await import('../opencode-go-identity-section')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

type RenderedOverview = {
  host: HTMLDivElement
  root: ReturnType<typeof createRoot>
}

let renderedOverview: RenderedOverview | null = null

function quotaWindow(
  kind: OpenCodeGoQuotaWindow['kind'],
  usedPercent: number
): OpenCodeGoQuotaWindow {
  const limits = { rolling: 12, weekly: 30, monthly: 60 }
  const limit = limits[kind]
  return {
    kind,
    source: 'opencode_console_authoritative',
    used_percent: usedPercent,
    remaining_percent: 100 - usedPercent,
    reset_seconds: 3600,
    reset_at: 1_900_003_600,
    fetched_at: 1_900_000_000,
    amounts_authoritative: false,
    calculated_limit_usd: limit,
    calculated_used_usd: (limit * usedPercent) / 100,
    calculated_remaining_usd: (limit * (100 - usedPercent)) / 100,
  }
}

function workspaceFixture(uid: string, usedOffset = 0): OpenCodeGoWorkspace {
  const now = 1_900_000_000
  return {
    uid,
    name: `Workspace ${uid}`,
    email: `${uid}@example.test`,
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
      quotaWindow('rolling', 20 + usedOffset),
      quotaWindow('weekly', 40 + usedOffset),
      quotaWindow('monthly', 60 + usedOffset),
    ],
    models: [],
    china_models_enabled: true,
    china_models_checked_at: now,
    china_models_error: '',
    referral_code: '',
    available_referral_rewards: 1,
    used_referral_rewards: 0,
    referral_reward_applied_at: 0,
    risk_detected_at: 0,
    risk_last_checked_at: now,
    last_synced_at: now,
    last_error: '',
    created_at: now - 3600,
    updated_at: now,
  }
}

function identityFixture(
  workspaces: OpenCodeGoWorkspace[]
): OpenCodeGoIdentity {
  return {
    uid: 'identity-test',
    label: 'Test account',
    email: 'account@example.test',
    status: 'active',
    manual_enabled: true,
    has_auth_cookie: true,
    last_synced_at: 1_900_000_000,
    last_error: '',
    created_at: 1_800_000_000,
    updated_at: 1_900_000_000,
    workspaces,
  }
}

async function renderOverview(
  identity: OpenCodeGoIdentity
): Promise<HTMLDivElement> {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  renderedOverview = { host, root }

  await act(async () => {
    root.render(
      <I18nextProvider i18n={i18n}>
        <TooltipProvider>
          <OpenCodeGoIdentitySection
            identity={identity}
            nowSeconds={1_900_000_000}
            locale='en'
            canOperate
            canSensitiveWrite
            busyKey={null}
            onEditLabel={() => undefined}
            onReplaceCookie={() => undefined}
            onRefreshIdentity={() => undefined}
            onToggleIdentity={() => undefined}
            onDeleteIdentity={() => undefined}
            onRefreshWorkspace={() => undefined}
            onRiskRecheckWorkspace={() => undefined}
            onToggleWorkspace={() => undefined}
            onWorkspaceSensitiveAction={() => undefined}
          />
        </TooltipProvider>
      </I18nextProvider>
    )
  })

  return host
}

afterEach(async () => {
  if (renderedOverview) {
    await act(async () => renderedOverview?.root.unmount())
    renderedOverview.host.remove()
    renderedOverview = null
  }
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('OpenCode Go account overview', () => {
  test('shows every workspace as compact three-window quota bars', async () => {
    const host = await renderOverview(
      identityFixture([
        workspaceFixture('primary'),
        workspaceFixture('secondary', 5),
      ])
    )

    assert.ok(host.querySelector('[data-account-card]'))
    assert.equal(
      host.querySelectorAll('[data-account-workspace-summary]').length,
      2
    )
    assert.equal(host.querySelectorAll('[data-slot="progress"]').length, 6)
    assert.equal(host.textContent?.includes('Calculated limit'), false)
    assert.equal(host.textContent?.includes('Snapshot time'), false)
    assert.equal(host.textContent?.includes('Referral rewards'), false)
    assert.equal(host.textContent?.includes('Models'), false)
  })

  test('opens low-frequency metadata and actions from the account card', async () => {
    const host = await renderOverview(
      identityFixture([workspaceFixture('primary')])
    )
    const detailsButton = host.querySelector<HTMLButtonElement>(
      'button[aria-label^="Details:"]'
    )
    assert.ok(detailsButton)

    await act(async () => detailsButton.click())

    assert.ok(document.querySelector('[data-account-details]'))
    assert.equal(document.body.textContent?.includes('Snapshot time'), true)
    assert.equal(document.body.textContent?.includes('Referral rewards'), true)
    assert.equal(document.body.textContent?.includes('Replace Cookie'), true)
  })
})
