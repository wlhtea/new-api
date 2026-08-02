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

import { CHANNEL_TYPE_OPENCODE_GO, CHANNEL_TYPE_OPTIONS } from '../../constants'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  channelFormSchema,
  transformFormDataToCreatePayload,
} from '../channel-form'
import {
  OPENCODE_GO_BASE_URL,
  OPENCODE_GO_MODELS,
  getChannelTypeConfig,
  getChannelTypeCreateDefaults,
  hasFixedBaseUrl,
  usesLegacyChannelKey,
} from '../channel-type-config'
import { getChannelTypeIcon } from '../channel-utils'

describe('OpenCode Go channel configuration', () => {
  test('registers one selectable Type 62 option', () => {
    const options = CHANNEL_TYPE_OPTIONS.filter(
      (item) => item.value === CHANNEL_TYPE_OPENCODE_GO
    )

    assert.deepEqual(options, [
      { value: CHANNEL_TYPE_OPENCODE_GO, label: 'OpenCode Go' },
    ])
  })

  test('uses the fixed inference root and official model defaults', () => {
    assert.deepEqual(getChannelTypeCreateDefaults(CHANNEL_TYPE_OPENCODE_GO), {
      baseUrl: OPENCODE_GO_BASE_URL,
      models: OPENCODE_GO_MODELS.join(','),
    })
    assert.equal(hasFixedBaseUrl(CHANNEL_TYPE_OPENCODE_GO), true)
    assert.equal(usesLegacyChannelKey(CHANNEL_TYPE_OPENCODE_GO), false)
    assert.equal(
      getChannelTypeConfig(CHANNEL_TYPE_OPENCODE_GO).supportedModels?.length,
      18
    )
    assert.equal(getChannelTypeIcon(CHANNEL_TYPE_OPENCODE_GO), 'OpenCode')
  })

  test('allows creation without a legacy channel key', () => {
    const form = {
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'OpenCode Go pool',
      type: CHANNEL_TYPE_OPENCODE_GO,
      base_url: OPENCODE_GO_BASE_URL,
      key: '',
      models: OPENCODE_GO_MODELS.join(','),
    }

    assert.equal(channelFormSchema.safeParse(form).success, true)
  })

  test('never serializes a legacy key into the channel payload', () => {
    const result = transformFormDataToCreatePayload({
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'OpenCode Go pool',
      type: CHANNEL_TYPE_OPENCODE_GO,
      base_url: OPENCODE_GO_BASE_URL,
      key: 'must-not-be-persisted',
      models: OPENCODE_GO_MODELS.join(','),
    })

    assert.equal(result.channel.key, null)
  })
})
