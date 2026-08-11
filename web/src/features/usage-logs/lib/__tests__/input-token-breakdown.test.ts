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

import { getInputTokenBreakdown } from '../format'

describe('usage log input token breakdown', () => {
  test('shows uncached input while retaining explicit total and cache values', () => {
    assert.deepEqual(
      getInputTokenBreakdown(30_289, {
        input_tokens_total: 30_289,
        cache_tokens: 29_952,
      }),
      {
        totalInputTokens: 30_289,
        uncachedInputTokens: 337,
        cacheReadTokens: 29_952,
        hasExplicitTotal: true,
      }
    )
  })

  test('does not double-subtract cache from logs without a reliable total', () => {
    assert.deepEqual(getInputTokenBreakdown(900, { cache_tokens: 256 }), {
      totalInputTokens: 900,
      uncachedInputTokens: 900,
      cacheReadTokens: 256,
      hasExplicitTotal: false,
    })
  })

  test('keeps Messages cache writes inside explicit total while separating cache read', () => {
    assert.deepEqual(
      getInputTokenBreakdown(100, {
        input_tokens_total: 210,
        cache_tokens: 80,
        cache_creation_tokens_5m: 20,
        cache_creation_tokens_1h: 10,
      }),
      {
        totalInputTokens: 210,
        uncachedInputTokens: 130,
        cacheReadTokens: 80,
        hasExplicitTotal: true,
      }
    )
  })

  test('never displays negative uncached input for inconsistent upstream usage', () => {
    assert.deepEqual(
      getInputTokenBreakdown(100, {
        input_tokens_total: 100,
        cache_tokens: 120,
      }),
      {
        totalInputTokens: 100,
        uncachedInputTokens: 0,
        cacheReadTokens: 120,
        hasExplicitTotal: true,
      }
    )
  })
})
