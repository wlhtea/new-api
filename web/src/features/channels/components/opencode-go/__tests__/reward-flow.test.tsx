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
  OpenCodeGoPool,
  OpenCodeGoWorkspace,
} from '../../../lib/opencode-go-schemas'
import type { Channel } from '../../../types'

const domWindow = new Window({ url: 'http://localhost' })
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLButtonElement',
  'HTMLInputElement',
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
const { QueryClient, QueryClientProvider } =
  await import('@tanstack/react-query')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { toast } = await import('sonner')
const { TooltipProvider } = await import('@/components/ui/tooltip')
const { api } = await import('@/lib/api')
const { useAuthStore } = await import('@/stores/auth-store')
const { openCodeGoPoolQueryKeys } =
  await import('../../../lib/opencode-go-pool')
const { OpenCodeGoPoolDrawer } =
  await import('../../drawers/opencode-go-pool-drawer')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

type ApiMethod = (
  url: string,
  data?: unknown,
  config?: unknown
) => Promise<{ data: unknown }>

type MockableApi = {
  get: ApiMethod
  post: ApiMethod
}

type RenderedDrawer = {
  host: HTMLDivElement
  queryClient: InstanceType<typeof QueryClient>
  root: ReturnType<typeof createRoot>
}

const apiClient = api as unknown as MockableApi
const originalGet = apiClient.get
const originalPost = apiClient.post
const toastClient = toast as unknown as {
  success: (message: React.ReactNode) => string | number
  error: (message: React.ReactNode) => string | number
}
const originalToastSuccess = toastClient.success
const originalToastError = toastClient.error
let renderedDrawer: RenderedDrawer | null = null

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((promiseResolve) => {
    resolve = promiseResolve
  })
  return { promise, resolve }
}

function workspaceFixture(
  overrides: Partial<OpenCodeGoWorkspace> = {}
): OpenCodeGoWorkspace {
  const now = 1_900_000_000
  return {
    uid: 'workspace-reward',
    name: 'Reward workspace',
    email: 'reward@example.test',
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
        used_percent: 31,
        remaining_percent: 69,
        reset_seconds: 3600,
        reset_at: now + 3600,
        fetched_at: now,
        amounts_authoritative: false,
        calculated_limit_usd: 12,
        calculated_used_usd: 3.72,
        calculated_remaining_usd: 8.28,
      },
      {
        kind: 'weekly',
        source: 'opencode_console_authoritative',
        used_percent: 31,
        remaining_percent: 69,
        reset_seconds: 7200,
        reset_at: now + 7200,
        fetched_at: now,
        amounts_authoritative: false,
        calculated_limit_usd: 30,
        calculated_used_usd: 9.3,
        calculated_remaining_usd: 20.7,
      },
      {
        kind: 'monthly',
        source: 'opencode_console_authoritative',
        used_percent: 31,
        remaining_percent: 69,
        reset_seconds: 10_800,
        reset_at: now + 10_800,
        fetched_at: now,
        amounts_authoritative: false,
        calculated_limit_usd: 60,
        calculated_used_usd: 18.6,
        calculated_remaining_usd: 41.4,
      },
    ],
    models: [],
    china_models_enabled: true,
    china_models_checked_at: now,
    china_models_error: '',
    referral_code: 'REWARD',
    available_referral_rewards: 1,
    used_referral_rewards: 0,
    referral_reward_eligible: true,
    referral_reward_applied_at: 0,
    risk_detected_at: 0,
    risk_last_checked_at: now,
    inflight: 0,
    last_synced_at: now,
    last_error: '',
    created_at: now - 3600,
    updated_at: now,
    ...overrides,
  }
}

function poolFixture(workspace = workspaceFixture()): OpenCodeGoPool {
  return {
    channel_id: 62,
    eligible_workspace_count: 1,
    crypto_secret_configured: true,
    lifecycle_policy: {
      automation_enabled: false,
      auto_enable_china_models: true,
      auto_apply_referral_rewards: true,
      referral_rewards_max_per_run: 3,
      auto_cancel_subscription_renewal: false,
    },
    identities: [
      {
        uid: 'identity-reward',
        label: 'Reward account',
        email: 'reward@example.test',
        status: 'active',
        manual_enabled: true,
        has_auth_cookie: true,
        last_synced_at: 1_900_000_000,
        last_error: '',
        created_at: 1_800_000_000,
        updated_at: 1_900_000_000,
        workspaces: [workspace],
      },
    ],
    operations: [],
  }
}

function channelFixture(): Channel {
  return {
    id: 62,
    type: 62,
    key: '',
    status: 1,
    name: 'OpenCode Go pool',
    created_time: 1,
    test_time: 0,
    response_time: 0,
    base_url: 'https://opencode.ai/zen/go/v1',
    other: '',
    balance: 0,
    balance_updated_time: 0,
    models: 'glm-5.2',
    group: 'default',
    used_quota: 0,
    other_info: '',
    remark: '',
    max_input_tokens: 0,
    channel_info: {
      is_multi_key: false,
      multi_key_size: 0,
      multi_key_polling_index: 0,
      multi_key_mode: 'random',
    },
    settings: '{}',
  }
}

function rewardEnvelope(
  pool: OpenCodeGoPool,
  attempted: number,
  applied: number
): unknown {
  return {
    success: true,
    data: { summary: { attempted, applied }, pool },
  }
}

function setSuperAdmin(): void {
  useAuthStore.getState().auth.setBundle({
    access_token: 'test-access-token',
    token_type: 'Bearer',
    access_expires_at: 2_000_000_000,
    user: { id: 1, username: 'root', role: 100 },
    session: {
      sid: 'test-session',
      current: true,
      login_method: 'password',
      ip: '127.0.0.1',
      user_agent: 'test',
      created_at: 1,
      last_active_at: 1,
      expires_at: 2_000_000_000,
    },
  })
}

async function waitForCondition(
  condition: () => boolean,
  failureMessage: string
): Promise<void> {
  if (condition()) return

  await new Promise<void>((resolve, reject) => {
    const observer = new MutationObserver(() => {
      if (!condition()) return
      clearTimeout(timeoutId)
      observer.disconnect()
      resolve()
    })
    const timeoutId = setTimeout(() => {
      observer.disconnect()
      reject(new Error(`${failureMessage}: ${document.body.textContent}`))
    }, 1500)

    observer.observe(document, {
      attributes: true,
      childList: true,
      characterData: true,
      subtree: true,
    })
  })
}

async function renderDrawer(initialPool: OpenCodeGoPool): Promise<void> {
  setSuperAdmin()
  apiClient.get = async (url) => {
    throw new Error(`Unexpected GET ${url}`)
  }

  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  queryClient.setQueryData(openCodeGoPoolQueryKeys.pool(62), initialPool, {
    updatedAt: Date.now() + 60_000,
  })
  renderedDrawer = { host, queryClient, root }

  await act(async () => {
    root.render(
      <QueryClientProvider client={queryClient}>
        <I18nextProvider i18n={i18n}>
          <TooltipProvider>
            <OpenCodeGoPoolDrawer
              open
              onOpenChange={() => undefined}
              channel={channelFixture()}
            />
          </TooltipProvider>
        </I18nextProvider>
      </QueryClientProvider>
    )
  })

  await act(async () =>
    waitForCondition(
      () => document.querySelector('button[aria-label^="Details:"]') !== null,
      'account pool did not load'
    )
  )
}

async function openRewardConfirmation(): Promise<void> {
  const detailsButton = document.querySelector<HTMLButtonElement>(
    'button[aria-label^="Details:"]'
  )
  assert.ok(detailsButton)
  await act(async () => detailsButton.click())

  await act(async () =>
    waitForCondition(
      () => document.querySelector('[data-account-details]') !== null,
      'account details did not open'
    )
  )

  const actionButton = document.querySelector<HTMLButtonElement>(
    'button[aria-label="Workspace actions"]'
  )
  assert.ok(actionButton)
  await act(async () => actionButton.click())
  await act(async () =>
    waitForCondition(
      () =>
        [...document.querySelectorAll<HTMLElement>('[role="menuitem"]')].some(
          (item) => item.textContent?.includes('Apply one referral reward')
        ),
      'workspace actions did not open'
    )
  )

  const rewardItem = [
    ...document.querySelectorAll<HTMLElement>('[role="menuitem"]'),
  ].find((item) => item.textContent?.includes('Apply one referral reward'))
  assert.ok(rewardItem)
  assert.notEqual(rewardItem.getAttribute('aria-disabled'), 'true')
  assert.equal(rewardItem.hasAttribute('data-disabled'), false)
  await act(async () => rewardItem.click())

  await act(async () =>
    waitForCondition(
      () =>
        document.querySelector('[data-slot="alert-dialog-content"]') !== null,
      'reward confirmation did not open'
    )
  )
}

function findConfirmButton(): HTMLButtonElement {
  const button = [
    ...document.querySelectorAll<HTMLButtonElement>('button'),
  ].find((candidate) => candidate.textContent?.trim() === 'Confirm')
  assert.ok(button)
  return button
}

afterEach(async () => {
  apiClient.get = originalGet
  apiClient.post = originalPost
  toastClient.success = originalToastSuccess
  toastClient.error = originalToastError
  if (renderedDrawer) {
    await act(async () => renderedDrawer?.root.unmount())
    renderedDrawer.queryClient.clear()
    renderedDrawer.host.remove()
    renderedDrawer = null
  }
  useAuthStore.getState().auth.reset()
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('OpenCode Go referral reward flow', () => {
  test('warns about immediate use and shows one success only for exact 1/1', async () => {
    const initialPool = poolFixture()
    const appliedPool = poolFixture(
      workspaceFixture({
        available_referral_rewards: 0,
        used_referral_rewards: 1,
        referral_reward_eligible: false,
        referral_reward_applied_at: 1_900_000_001,
      })
    )
    let postCount = 0
    const request = deferred<{ data: unknown }>()
    apiClient.post = () => {
      postCount += 1
      return request.promise
    }
    const successMessages: React.ReactNode[] = []
    const errorMessages: React.ReactNode[] = []
    toastClient.success = (message) => successMessages.push(message)
    toastClient.error = (message) => errorMessages.push(message)

    await renderDrawer(initialPool)
    await openRewardConfirmation()

    const description = document.querySelector<HTMLElement>(
      '[data-slot="alert-dialog-description"]'
    )
    assert.ok(description)
    assert.equal(
      description.textContent?.includes(
        'immediately offsets current usage for workspace "Reward workspace"'
      ),
      true
    )
    assert.equal(
      description.textContent?.includes('unused value does not carry over'),
      true
    )

    await act(async () => findConfirmButton().click())
    assert.equal(postCount, 1)
    assert.deepEqual(successMessages, [])
    assert.deepEqual(errorMessages, [])

    await act(async () => {
      request.resolve({ data: rewardEnvelope(appliedPool, 1, 1) })
      await request.promise
      await Promise.resolve()
    })

    assert.deepEqual(successMessages, ['Applied 1 referral rewards'])
    assert.deepEqual(errorMessages, [])
    assert.deepEqual(
      renderedDrawer?.queryClient.getQueryData(
        openCodeGoPoolQueryKeys.pool(62)
      ),
      appliedPool
    )
  })

  test('routes a zero summary through error behavior without replacing the pool', async () => {
    const initialPool = poolFixture()
    let postCount = 0
    const request = deferred<{ data: unknown }>()
    apiClient.post = () => {
      postCount += 1
      return request.promise
    }
    const successMessages: React.ReactNode[] = []
    const errorMessages: React.ReactNode[] = []
    toastClient.success = (message) => successMessages.push(message)
    toastClient.error = (message) => errorMessages.push(message)

    await renderDrawer(initialPool)
    await openRewardConfirmation()

    await act(async () => findConfirmButton().click())
    assert.equal(postCount, 1)
    assert.deepEqual(successMessages, [])
    assert.deepEqual(errorMessages, [])

    await act(async () => {
      request.resolve({ data: rewardEnvelope(initialPool, 0, 0) })
      await request.promise
      await Promise.resolve()
    })

    assert.deepEqual(successMessages, [])
    assert.equal(errorMessages.length, 1)
    assert.deepEqual(
      renderedDrawer?.queryClient.getQueryData(
        openCodeGoPoolQueryKeys.pool(62)
      ),
      initialPool
    )
  })
})
