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
  CHANNEL_TYPE_OPENCODE_API_KEY,
  CHANNEL_TYPE_OPENCODE_GO,
  CHANNEL_TYPE_OPTIONS,
  MODEL_FETCHABLE_TYPES,
  OPENCODE_API_KEY_ADD_MODE_OPTIONS,
  OPENCODE_API_KEY_BATCH_FORMAT,
} from '../../constants'
import { channelSchema } from '../../types'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
} from '../channel-form'
import {
  OPENCODE_API_KEY_BASE_URL,
  OPENCODE_API_KEY_MODELS,
  getChannelTypeConfig,
  getChannelTypeCreateDefaults,
  hasFixedBaseUrl,
  isOpenCodeAPIKeyChannel,
  usesLegacyChannelKey,
  usesOpenCodeProtocolSettings,
} from '../channel-type-config'
import { getChannelTypeIcon, getKeyPromptForType } from '../channel-utils'
import {
  deduplicateOpenCodeAPIKeyBatchEntries,
  isOpenCodeAPIKeyBatchForm,
  resolveChannelCreateMode,
  shouldShowSharedChannelProxy,
} from '../opencode-api-key'

function openCodeAPIKeyForm() {
  return {
    ...CHANNEL_FORM_DEFAULT_VALUES,
    name: 'Inference key pool',
    type: CHANNEL_TYPE_OPENCODE_API_KEY,
    base_url: OPENCODE_API_KEY_BASE_URL,
    key: 'api-key-a',
    models: OPENCODE_API_KEY_MODELS.join(','),
    group: ['opencode-key-pool'],
    tag: 'opencode-key-pool',
  }
}

describe('OpenCode API Key channel', () => {
  test('registers Type 63 with the fixed OpenCode endpoint and model defaults', () => {
    const options = CHANNEL_TYPE_OPTIONS.filter(
      (item) => item.value === CHANNEL_TYPE_OPENCODE_API_KEY
    )

    assert.deepEqual(options, [
      {
        value: CHANNEL_TYPE_OPENCODE_API_KEY,
        label: 'OpenCode API Key',
      },
    ])
    assert.deepEqual(
      getChannelTypeCreateDefaults(CHANNEL_TYPE_OPENCODE_API_KEY),
      {
        baseUrl: OPENCODE_API_KEY_BASE_URL,
        models: OPENCODE_API_KEY_MODELS.join(','),
      }
    )
    assert.equal(hasFixedBaseUrl(CHANNEL_TYPE_OPENCODE_API_KEY), true)
    assert.equal(usesLegacyChannelKey(CHANNEL_TYPE_OPENCODE_API_KEY), true)
    assert.equal(isOpenCodeAPIKeyChannel(CHANNEL_TYPE_OPENCODE_API_KEY), true)
    assert.equal(usesOpenCodeProtocolSettings(CHANNEL_TYPE_OPENCODE_GO), true)
    assert.equal(
      usesOpenCodeProtocolSettings(CHANNEL_TYPE_OPENCODE_API_KEY),
      true
    )
    assert.equal(MODEL_FETCHABLE_TYPES.has(CHANNEL_TYPE_OPENCODE_API_KEY), true)
    assert.equal(getChannelTypeIcon(CHANNEL_TYPE_OPENCODE_API_KEY), 'OpenCode')
    assert.equal(
      getKeyPromptForType(CHANNEL_TYPE_OPENCODE_API_KEY),
      'Enter OpenCode API key'
    )
    assert.equal(
      getChannelTypeConfig(CHANNEL_TYPE_OPENCODE_API_KEY).icon,
      'OpenCode'
    )
    assert.equal(OPENCODE_API_KEY_MODELS.includes('qwen3.8-max'), true)

    assert.equal(usesLegacyChannelKey(CHANNEL_TYPE_OPENCODE_GO), false)
  })

  test('offers only single and pair-batch create modes', () => {
    assert.deepEqual(
      OPENCODE_API_KEY_ADD_MODE_OPTIONS.map((option) => option.value),
      ['single', 'batch']
    )
    assert.equal(OPENCODE_API_KEY_BATCH_FORMAT, 'API_KEY | PROXY_URL')
    assert.equal(
      resolveChannelCreateMode(CHANNEL_TYPE_OPENCODE_API_KEY, 'single'),
      'single'
    )
    assert.equal(
      resolveChannelCreateMode(CHANNEL_TYPE_OPENCODE_API_KEY, 'batch'),
      'opencode_api_key_batch'
    )
    assert.equal(
      resolveChannelCreateMode(
        CHANNEL_TYPE_OPENCODE_API_KEY,
        'multi_to_single'
      ),
      'single'
    )
    assert.equal(resolveChannelCreateMode(1, 'batch'), 'batch')
  })

  test('hides the shared proxy only for Type 63 pair-batch creation', () => {
    assert.equal(
      isOpenCodeAPIKeyBatchForm(CHANNEL_TYPE_OPENCODE_API_KEY, 'batch', false),
      true
    )
    assert.equal(
      shouldShowSharedChannelProxy(
        CHANNEL_TYPE_OPENCODE_API_KEY,
        'batch',
        false
      ),
      false
    )
    assert.equal(
      shouldShowSharedChannelProxy(
        CHANNEL_TYPE_OPENCODE_API_KEY,
        'single',
        false
      ),
      true
    )
    assert.equal(
      shouldShowSharedChannelProxy(
        CHANNEL_TYPE_OPENCODE_API_KEY,
        'batch',
        true
      ),
      true
    )
    assert.equal(
      shouldShowSharedChannelProxy(CHANNEL_TYPE_OPENCODE_GO, 'batch', false),
      true
    )
  })

  test('rejects generic multi-key mode for Type 63', () => {
    const parsed = channelFormSchema.safeParse({
      ...openCodeAPIKeyForm(),
      multi_key_mode: 'multi_to_single',
    })

    assert.equal(parsed.success, false)
    if (!parsed.success) {
      assert.equal(
        parsed.error.issues.some(
          (issue) =>
            issue.path[0] === 'multi_key_mode' &&
            issue.message ===
              'OpenCode API Key channels do not support multi-key mode'
        ),
        true
      )
    }
  })

  test('creates a single ordinary-key channel with its static proxy', () => {
    const result = transformFormDataToCreatePayload({
      ...openCodeAPIKeyForm(),
      priority: 20,
      weight: 10,
      proxy: 'socks5h://proxy.invalid:1080',
      opencode_go_default_protocol: 'responses',
      opencode_go_model_protocols: '{"GLM-*":"messages"}',
      opencode_go_identity_proxy_enabled: true,
      opencode_go_generic_failover_enabled: true,
      opencode_go_affinity_fallback: 'token',
      opencode_go_load_aware_enabled: true,
      pass_through_body_enabled: true,
      settings: JSON.stringify({
        retained_setting: 'keep-me',
        opencode_go: {
          retained_pool_setting: 'remove-me',
        },
      }),
    })
    const setting = JSON.parse(String(result.channel.setting))
    const settings = JSON.parse(String(result.channel.settings))

    assert.equal(result.mode, 'single')
    assert.equal(result.channel.key, 'api-key-a')
    assert.equal(result.channel.base_url, OPENCODE_API_KEY_BASE_URL)
    assert.equal(result.channel.group, 'opencode-key-pool')
    assert.equal(result.channel.tag, 'opencode-key-pool')
    assert.equal(result.channel.priority, 20)
    assert.equal(result.channel.weight, 10)
    assert.equal(setting.proxy, 'socks5h://proxy.invalid:1080')
    assert.equal(setting.pass_through_body_enabled, false)
    assert.equal(settings.retained_setting, 'keep-me')
    assert.deepEqual(settings.opencode_go, {
      model_protocols: { 'glm-*': 'messages' },
      default_protocol: 'responses',
    })
  })

  test('creates a pair batch without serializing a stale shared proxy', () => {
    const batchInput = [
      'api-key-a | socks5://proxy-a.invalid:1080',
      'api-key-b',
    ].join('\n')
    const result = transformFormDataToCreatePayload({
      ...openCodeAPIKeyForm(),
      key: batchInput,
      multi_key_mode: 'batch',
      proxy: 'https://stale-proxy.invalid:8443',
      batch_add_set_key_prefix_2_name: true,
      opencode_go_default_protocol: 'messages',
      opencode_go_model_protocols: '{"qwen*":"messages"}',
    })
    const setting = JSON.parse(String(result.channel.setting))
    const settings = JSON.parse(String(result.channel.settings))

    assert.equal(result.mode, 'opencode_api_key_batch')
    assert.equal(result.channel.key, batchInput)
    assert.equal(setting.proxy, '')
    assert.equal(result.batch_add_set_key_prefix_2_name, undefined)
    assert.deepEqual(settings.opencode_go, {
      model_protocols: { 'qwen*': 'messages' },
      default_protocol: 'messages',
    })
  })

  test('edits an imported row as one ordinary key and removes pool-only settings', () => {
    const channel = channelSchema.parse({
      id: 6301,
      type: CHANNEL_TYPE_OPENCODE_API_KEY,
      key: '',
      status: 1,
      name: 'Imported account 001',
      created_time: 1,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      base_url: OPENCODE_API_KEY_BASE_URL,
      models: 'glm-5.2',
      group: 'opencode-key-pool',
      setting: JSON.stringify({
        proxy: 'https://proxy.invalid:8443',
      }),
      settings: JSON.stringify({
        retained_setting: 'keep-me',
        opencode_go: {
          default_protocol: 'chat',
          model_protocols: { 'glm-*': 'messages' },
          generic_failover_enabled: true,
          identity_proxy_enabled: true,
          load_aware_enabled: true,
        },
      }),
    })
    const defaults = transformChannelToFormDefaults(channel)

    assert.equal(defaults.key, '')
    assert.equal(defaults.proxy, 'https://proxy.invalid:8443')
    assert.equal(defaults.opencode_go_default_protocol, 'chat')

    defaults.key = 'replacement-key'
    const update = transformFormDataToUpdatePayload(defaults, channel.id)
    const settings = JSON.parse(String(update.settings))

    assert.equal(update.key, 'replacement-key')
    assert.equal(
      JSON.parse(String(update.setting)).proxy,
      'https://proxy.invalid:8443'
    )
    assert.equal(settings.retained_setting, 'keep-me')
    assert.deepEqual(settings.opencode_go, {
      model_protocols: { 'glm-*': 'messages' },
      default_protocol: 'chat',
    })
  })

  test('deduplicates pair lines by key while preserving the first proxy value', () => {
    const result = deduplicateOpenCodeAPIKeyBatchEntries(
      [
        'api-key-a | socks5://user|segment@proxy.invalid:1080',
        'api-key-b',
        'api-key-a | https://second-proxy.invalid:8443',
      ].join('\r\n')
    )

    assert.deepEqual(result, {
      deduplicatedText: [
        'api-key-a | socks5://user|segment@proxy.invalid:1080',
        'api-key-b',
      ].join('\n'),
      beforeCount: 3,
      afterCount: 2,
      removedCount: 1,
    })
  })
})
