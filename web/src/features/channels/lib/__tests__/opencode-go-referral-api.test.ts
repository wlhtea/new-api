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
import { afterEach, describe, test } from 'node:test'

import { api } from '@/lib/api'

import { applyOpenCodeGoReferralReward } from '../../api'
import { openCodeGoPoolSchema } from '../opencode-go-schemas'

type ApiMethod = (
  url: string,
  data?: unknown,
  config?: unknown
) => Promise<{ data: unknown }>

const apiClient = api as unknown as { post: ApiMethod }
const originalPost = apiClient.post

function poolPayload() {
  return openCodeGoPoolSchema.parse({
    channel_id: 62,
    eligible_workspace_count: 0,
    crypto_secret_configured: true,
    lifecycle_policy: {
      automation_enabled: false,
      auto_enable_china_models: true,
      auto_apply_referral_rewards: true,
      referral_rewards_max_per_run: 3,
      auto_cancel_subscription_renewal: false,
    },
    identities: [],
    operations: [],
  })
}

function rewardEnvelope(attempted: number, applied: number): unknown {
  return {
    success: true,
    data: {
      summary: { attempted, applied },
      pool: poolPayload(),
    },
  }
}

afterEach(() => {
  apiClient.post = originalPost
})

describe('OpenCode Go referral reward API', () => {
  test('accepts only the verified one-attempt one-apply result', async () => {
    const requests: string[] = []
    apiClient.post = async (url) => {
      requests.push(url)
      return { data: rewardEnvelope(1, 1) }
    }

    const result = await applyOpenCodeGoReferralReward(
      62,
      'workspace/test value'
    )

    assert.deepEqual(result.summary, { attempted: 1, applied: 1 })
    assert.deepEqual(requests, [
      '/api/channel/62/opencode-go/workspaces/workspace%2Ftest%20value/referral-rewards/apply',
    ])
  })

  test('rejects zero and malformed success summaries', async () => {
    const summaries: unknown[] = [
      { attempted: 0, applied: 0 },
      { attempted: 1, applied: 0 },
      { attempted: 0, applied: 1 },
      { attempted: 2, applied: 1 },
      { attempted: 1 },
      'invalid',
    ]

    for (const summary of summaries) {
      apiClient.post = async () => ({
        data: {
          success: true,
          data: { summary, pool: poolPayload() },
        },
      })

      await assert.rejects(applyOpenCodeGoReferralReward(62, 'workspace-test'))
    }
  })
})
