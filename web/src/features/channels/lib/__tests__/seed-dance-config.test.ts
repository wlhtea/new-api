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

import { CHANNEL_TYPES, CHANNEL_TYPE_OPTIONS } from '../../constants'
import { channelFormSchema } from '../channel-form'
import {
  getChannelTypeConfig,
  getChannelTypeCreateDefaults,
} from '../channel-type-config'
import { getChannelTypeIcon } from '../channel-utils'

const seedDanceBaseUrl =
  'http://alb-o13xqj8f2cpjsa67ym.ap-northeast-1.alb.aliyuncsslbintl.com/v1/public_api/m-predict/polar4ai-i2v'

describe('Seed Dance channel configuration', () => {
  test('publishes one ordered Type 59 option', () => {
    assert.equal(CHANNEL_TYPES[59], 'Uncensored Seed Dance')
    const ids = CHANNEL_TYPE_OPTIONS.map((item) => item.value)
    assert.equal(ids.filter((id) => id === 59).length, 1)
    assert.ok(ids.indexOf(54) < ids.indexOf(59))
    assert.ok(ids.indexOf(59) < ids.indexOf(55))
  })

  test('provides the create defaults and icon', () => {
    assert.deepEqual(getChannelTypeCreateDefaults(59), {
      baseUrl: seedDanceBaseUrl,
      models: 'seedance-uncensored',
    })
    assert.equal(getChannelTypeConfig(59).defaultBaseUrl, seedDanceBaseUrl)
    assert.deepEqual(getChannelTypeConfig(59).supportedModels, [
      'seedance-uncensored',
    ])
    assert.equal(getChannelTypeIcon(59), 'Doubao')
  })

  test('requires a non-blank Base URL', () => {
    const base = {
      name: 'Seed Dance',
      type: 59,
      key: 'TEST_KEY',
      models: 'seedance-uncensored',
      group: ['default'],
      status: 2,
    }
    assert.equal(
      channelFormSchema.safeParse({ ...base, base_url: '   ' }).success,
      false
    )
    assert.equal(
      channelFormSchema.safeParse({
        ...base,
        base_url: seedDanceBaseUrl,
      }).success,
      true
    )
  })
})
