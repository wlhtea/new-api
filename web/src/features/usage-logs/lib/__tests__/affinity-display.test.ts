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

import { resolveUsageLogAffinity } from '../format'

describe('usage log affinity display', () => {
  test('groups every request fingerprint source while retaining its exact origin', () => {
    const cases = [
      ['claude-code-session', 'X-Claude-Code-Session-Id'],
      ['claude-metadata-session', 'metadata.user_id.session_id'],
      ['opencode-session', 'x-opencode-session'],
      ['prompt_cache_key', 'prompt_cache_key'],
    ] as const

    for (const [source, sourceDetail] of cases) {
      assert.deepEqual(
        resolveUsageLogAffinity({
          opencode_go_affinity_source: source,
          opencode_go_workspace_uid: '4f7c9a10-6b7e-4e26-91b4-a71687fb4c01',
        }),
        {
          method: 'fingerprint',
          source,
          sourceDetail,
          workspaceUid: '4f7c9a10-6b7e-4e26-91b4-a71687fb4c01',
          workspaceShortId: '4f7c9a10',
        }
      )
    }
  })

  test('maps token and no-affinity selection to distinct methods', () => {
    assert.equal(
      resolveUsageLogAffinity({ opencode_go_affinity_source: 'token' })?.method,
      'token'
    )
    assert.equal(
      resolveUsageLogAffinity({ opencode_go_affinity_source: 'none' })?.method,
      'round_robin'
    )
  })

  test('prefers public fields and falls back to nested admin fields', () => {
    assert.deepEqual(
      resolveUsageLogAffinity({
        opencode_go_affinity_source: 'token',
        opencode_go_workspace_uid: 'a1b2c3d4-6b7e-4e26-91b4-a71687fb4c01',
        admin_info: {
          opencode_go_affinity_source: 'prompt_cache_key',
          opencode_go_workspace_uid: 'deadbeef-6b7e-4e26-91b4-a71687fb4c01',
        },
      }),
      {
        method: 'token',
        source: 'token',
        sourceDetail: 'token',
        workspaceUid: 'a1b2c3d4-6b7e-4e26-91b4-a71687fb4c01',
        workspaceShortId: 'a1b2c3d4',
      }
    )
  })

  test('keeps UID-only history visible without inventing an affinity method', () => {
    assert.deepEqual(
      resolveUsageLogAffinity({
        admin_info: {
          opencode_go_workspace_uid: 'bc2d1198-6b7e-4e26-91b4-a71687fb4c01',
        },
      }),
      {
        workspaceUid: 'bc2d1198-6b7e-4e26-91b4-a71687fb4c01',
        workspaceShortId: 'bc2d1198',
      }
    )
  })

  test('uses a neutral fallback for a future admin source', () => {
    assert.deepEqual(
      resolveUsageLogAffinity({
        admin_info: { opencode_go_affinity_source: 'future-affinity-source' },
      }),
      {
        method: 'other',
        source: 'future-affinity-source',
        sourceDetail: 'future-affinity-source',
      }
    )
  })

  test('ignores invalid runtime values and returns null when nothing remains', () => {
    assert.equal(resolveUsageLogAffinity(null), null)
    assert.equal(resolveUsageLogAffinity({}), null)
    assert.equal(
      resolveUsageLogAffinity({
        opencode_go_affinity_source: 42,
        opencode_go_workspace_uid: ' '.repeat(4),
        admin_info: {
          opencode_go_affinity_source: null,
          opencode_go_workspace_uid: 'w'.repeat(65),
        },
      } as never),
      null
    )
  })
})
