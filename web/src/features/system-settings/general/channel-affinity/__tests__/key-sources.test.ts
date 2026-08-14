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
  formatKeySource,
  KEY_SOURCE_TYPES,
  normalizeKeySource,
  normalizeValidKeySources,
} from '../key-sources.ts'

describe('OpenCode affinity identity key source', () => {
  test('is available in the visual editor', () => {
    assert.equal(KEY_SOURCE_TYPES.includes('opencode_identity'), true)
  })

  test('requires neither key nor path', () => {
    assert.deepEqual(
      normalizeKeySource({
        type: 'opencode_identity',
        key: 'unused',
        path: 'unused',
      }),
      { type: 'opencode_identity' }
    )
    assert.deepEqual(
      normalizeValidKeySources([{ type: 'opencode_identity' }]),
      [{ type: 'opencode_identity' }]
    )
  })

  test('survives the editor load-save normalization round trip', () => {
    const persisted = structuredClone([{ type: 'opencode_identity' as const }])

    assert.deepEqual(normalizeValidKeySources(persisted), [
      { type: 'opencode_identity' },
    ])
    assert.equal(
      formatKeySource(persisted[0]),
      'OpenCode identity (token_id fallback)'
    )
  })
})
