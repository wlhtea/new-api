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
import { describe, test } from 'node:test'

import {
  getOpenCodeGoOrdinaryBusyKey,
  getOpenCodeGoQuotaWindow,
  getOpenCodeGoTaskProgress,
  getOpenCodeGoTaskResults,
  isOpenCodeGoWorkspaceRecovered,
  isOpenCodeGoWorkspaceStale,
  isOpenCodeGoBulkResultFailure,
  listOpenCodeGoWorkspaceRows,
} from '../opencode-go-pool'
import {
  openCodeGoImportResultSchema,
  openCodeGoLifecyclePolicySchema,
  openCodeGoPoolSchema,
  type OpenCodeGoPool,
  type OpenCodeGoQuotaWindow,
  type OpenCodeGoSystemTask,
  type OpenCodeGoWorkspace,
} from '../opencode-go-schemas'

function quotaWindow(
  kind: OpenCodeGoQuotaWindow['kind'],
  usedPercent: number,
  limit: number
): OpenCodeGoQuotaWindow {
  return {
    kind,
    source: 'opencode_console_authoritative',
    used_percent: usedPercent,
    remaining_percent: 100 - usedPercent,
    reset_seconds: 3600,
    reset_at: 2_000_000_000,
    fetched_at: 1_900_000_000,
    amounts_authoritative: false,
    calculated_limit_usd: limit,
    calculated_used_usd: (limit * usedPercent) / 100,
    calculated_remaining_usd: (limit * (100 - usedPercent)) / 100,
  }
}

function workspace(
  overrides: Partial<OpenCodeGoWorkspace> = {}
): OpenCodeGoWorkspace {
  return {
    uid: 'workspace-member',
    name: 'Primary workspace',
    email: 'member@example.test',
    has_api_key: true,
    credential_status: 'ready',
    membership_status: 'active',
    subscription_ends_at: 2_100_000_000,
    renewal_cancelled_at: 0,
    renewal_checked_at: 1_900_000_000,
    renewal_cancel_error: '',
    manual_enabled: true,
    effective_state: 'eligible',
    state_reason: '',
    health_observation: 'model_probe_succeeded',
    health_observed_at: 1_900_000_000,
    cooldown_until: 0,
    quota_snapshot_status: 'complete',
    quota_fetched_at: 1_900_000_000,
    quota_next_refresh_at: 1_900_003_600,
    quota_recovery_at: 2_000_000_000,
    quota_parser_version: 'console-v1',
    quota_error: '',
    quota_windows: [
      quotaWindow('rolling', 25, 12),
      quotaWindow('weekly', 50, 30),
      quotaWindow('monthly', 75, 60),
    ],
    models: [
      {
        model: 'glm-5.2',
        discovered: true,
        state: 'available',
        disabled_until: 0,
        last_error_code: '',
        last_error: '',
        health_observation: 'model_probe_succeeded',
        health_observed_at: 1_900_000_000,
        updated_at: 1_900_000_000,
      },
    ],
    china_models_enabled: true,
    china_models_checked_at: 1_900_000_000,
    china_models_error: '',
    referral_code: 'TEST-CODE',
    available_referral_rewards: 2,
    used_referral_rewards: 1,
    referral_reward_applied_at: 0,
    risk_detected_at: 0,
    risk_last_checked_at: 1_900_000_000,
    last_synced_at: 1_900_000_000,
    last_error: '',
    created_at: 1_800_000_000,
    updated_at: 1_900_000_000,
    ...overrides,
  }
}

function poolFixture(): OpenCodeGoPool {
  return {
    channel_id: 62,
    eligible_workspace_count: 1,
    crypto_secret_configured: true,
    lifecycle_policy: {
      automation_enabled: true,
      auto_enable_china_models: true,
      auto_apply_referral_rewards: true,
      referral_rewards_max_per_run: 3,
      auto_cancel_subscription_renewal: false,
    },
    identities: [
      {
        uid: 'identity-one',
        label: 'Primary',
        email: 'member@example.test',
        status: 'active',
        manual_enabled: true,
        has_auth_cookie: true,
        last_synced_at: 1_900_000_000,
        last_error: '',
        created_at: 1_800_000_000,
        updated_at: 1_900_000_000,
        workspaces: [
          workspace(),
          workspace({
            uid: 'workspace-non-member',
            membership_status: 'inactive',
            effective_state: 'membership_expired',
            health_observation: '',
          }),
        ],
      },
    ],
    operations: [],
  }
}

function systemTask(
  overrides: Partial<OpenCodeGoSystemTask>
): OpenCodeGoSystemTask {
  return {
    id: 1,
    task_id: 'task-one',
    type: 'opencode_go_refresh',
    status: 'running',
    error: '',
    locked_by: 'worker-one',
    created_at: 1_900_000_000,
    updated_at: 1_900_000_001,
    ...overrides,
  }
}

describe('OpenCode Go pool contracts', () => {
  test('parses authoritative rolling, weekly, and monthly quota windows', () => {
    const parsed = openCodeGoPoolSchema.parse(poolFixture())
    const primary = parsed.identities[0]?.workspaces[0]

    assert.ok(primary)
    assert.equal(isOpenCodeGoWorkspaceStale(primary), false)
    assert.deepEqual(
      primary.quota_windows.map((window) => window.kind),
      ['rolling', 'weekly', 'monthly']
    )
    assert.equal(
      getOpenCodeGoQuotaWindow(primary, 'monthly')?.calculated_limit_usd,
      60
    )
    assert.equal(primary.quota_windows[0]?.amounts_authoritative, false)
  })

  test('marks missing, duplicate, and explicitly incomplete windows as stale', () => {
    const complete = workspace()
    const missing = workspace({
      quota_windows: complete.quota_windows.slice(0, 2),
    })
    const duplicate = workspace({
      quota_windows: [
        quotaWindow('rolling', 10, 12),
        quotaWindow('rolling', 20, 12),
        quotaWindow('monthly', 30, 60),
      ],
    })
    const incomplete = workspace({ quota_snapshot_status: 'partial' })

    assert.equal(isOpenCodeGoWorkspaceStale(missing), true)
    assert.equal(isOpenCodeGoWorkspaceStale(duplicate), true)
    assert.equal(isOpenCodeGoWorkspaceStale(incomplete), true)
  })

  test('recognizes only eligible workspaces with recovery observations', () => {
    assert.equal(isOpenCodeGoWorkspaceRecovered(workspace()), true)
    assert.equal(
      isOpenCodeGoWorkspaceRecovered(
        workspace({ health_observation: 'cooldown_expired' })
      ),
      true
    )
    assert.equal(
      isOpenCodeGoWorkspaceRecovered(
        workspace({
          effective_state: 'cooldown',
          cooldown_until: 2_000_000_000,
        })
      ),
      false
    )
    assert.equal(
      isOpenCodeGoWorkspaceRecovered(
        workspace({ health_observation: 'quota_snapshot_refreshed' })
      ),
      false
    )
  })

  test('filters non-members without losing their owning identity', () => {
    const pool = poolFixture()

    assert.equal(listOpenCodeGoWorkspaceRows(pool).length, 2)
    assert.deepEqual(
      listOpenCodeGoWorkspaceRows(pool, true).map((row) => ({
        identity: row.identity.uid,
        workspace: row.workspace.uid,
      })),
      [
        {
          identity: 'identity-one',
          workspace: 'workspace-non-member',
        },
      ]
    )
    assert.deepEqual(
      listOpenCodeGoWorkspaceRows({ ...pool, identities: [] }),
      []
    )
  })

  test('keeps an empty eligible set and partial import outcomes explicit', () => {
    const noEligiblePool = openCodeGoPoolSchema.parse({
      ...poolFixture(),
      eligible_workspace_count: 0,
    })
    const importResults = [
      openCodeGoImportResultSchema.parse({
        index: 1,
        status: 'imported',
        identity_uid: 'identity-ok',
        workspace_count: 2,
      }),
      openCodeGoImportResultSchema.parse({
        index: 2,
        status: 'error',
        error: 'Cookie was rejected',
      }),
    ]

    assert.equal(noEligiblePool.eligible_workspace_count, 0)
    assert.deepEqual(importResults, [
      {
        index: 1,
        status: 'imported',
        identity_uid: 'identity-ok',
        workspace_count: 2,
      },
      {
        index: 2,
        status: 'error',
        error: 'Cookie was rejected',
      },
    ])
  })

  test('normalizes task progress and preserves partial refresh results', () => {
    const task = systemTask({
      status: 'succeeded',
      state: { total: 2, processed: 2, progress: 140 },
      result: {
        total: 2,
        processed: 2,
        succeeded: 1,
        failed: 1,
        results: [
          {
            channel_id: 62,
            identity_uid: 'identity-ok',
            status: 'refreshed',
          },
          {
            channel_id: 62,
            identity_uid: 'identity-failed',
            status: 'error',
            error: 'upstream unavailable',
          },
        ],
      },
    })

    assert.deepEqual(getOpenCodeGoTaskProgress(task), {
      total: 2,
      processed: 2,
      progress: 100,
    })
    assert.deepEqual(getOpenCodeGoTaskResults(task), [
      { key: 'identity-ok', status: 'refreshed', error: undefined },
      {
        key: 'identity-failed',
        status: 'error',
        error: 'upstream unavailable',
      },
    ])
    assert.equal(
      isOpenCodeGoBulkResultFailure({
        key: 'workspace-still-blocked',
        status: 'not_recovered',
      }),
      true
    )
    assert.equal(
      isOpenCodeGoBulkResultFailure({
        key: 'workspace-blocked',
        status: 'blocked',
      }),
      false
    )
  })

  test('accepts the backend policy range from zero through twenty', () => {
    const base = poolFixture().lifecycle_policy

    assert.equal(
      openCodeGoLifecyclePolicySchema.safeParse({
        ...base,
        referral_rewards_max_per_run: 0,
      }).success,
      true
    )
    assert.equal(
      openCodeGoLifecyclePolicySchema.safeParse({
        ...base,
        referral_rewards_max_per_run: 20,
      }).success,
      true
    )
    assert.equal(
      openCodeGoLifecyclePolicySchema.safeParse({
        ...base,
        referral_rewards_max_per_run: 21,
      }).success,
      false
    )
  })

  test('clears ordinary busy state as soon as a mutation is terminal', () => {
    const action = {
      kind: 'workspace-refresh' as const,
      workspaceUid: 'workspace-a',
    }

    assert.equal(
      getOpenCodeGoOrdinaryBusyKey(true, action),
      'workspace:workspace-a:refresh'
    )
    assert.equal(getOpenCodeGoOrdinaryBusyKey(false, action), null)
    assert.equal(getOpenCodeGoOrdinaryBusyKey(true, undefined), null)
  })
})
