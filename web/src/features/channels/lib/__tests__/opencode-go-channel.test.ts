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
import { channelSchema } from '../../types'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
} from '../channel-form'
import {
  OPENCODE_GO_BASE_URL,
  OPENCODE_GO_MODELS,
  getChannelTypeConfig,
  getChannelTypeCreateDefaults,
  hasFixedBaseUrl,
  shouldWarnAboutV1BaseUrl,
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
    assert.equal(
      shouldWarnAboutV1BaseUrl(CHANNEL_TYPE_OPENCODE_GO, OPENCODE_GO_BASE_URL),
      false
    )
    assert.equal(shouldWarnAboutV1BaseUrl(8, 'https://proxy.example/v1'), true)
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

  test('preserves an admin-curated model subset on create payload', () => {
    // The admin removed the expensive models from the account pool list; the
    // payload must carry exactly the curated subset, not the full pool set.
    const curated = 'glm-5.2,glm-5.1,kimi-k2.7-code'
    const result = transformFormDataToCreatePayload({
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'OpenCode Go pool',
      type: CHANNEL_TYPE_OPENCODE_GO,
      base_url: OPENCODE_GO_BASE_URL,
      key: '',
      models: curated,
    })

    assert.equal(result.channel.models, curated)
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

  test('serializes protocol routing and the full backend lifecycle range', () => {
    const result = transformFormDataToCreatePayload({
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'OpenCode Go pool',
      type: CHANNEL_TYPE_OPENCODE_GO,
      base_url: OPENCODE_GO_BASE_URL,
      key: '',
      models: OPENCODE_GO_MODELS.join(','),
      opencode_go_default_protocol: 'responses',
      opencode_go_model_protocols: '{"GLM-*":"messages","kimi-k3":"chat"}',
      opencode_go_generic_failover_enabled: true,
      opencode_go_generic_failover_threshold: 2,
      opencode_go_generic_failover_window_seconds: 30,
      opencode_go_generic_failover_max_backups: 1,
      opencode_go_generic_failover_lease_seconds: 1800,
      opencode_go_affinity_fallback: 'token',
      opencode_go_load_aware_enabled: true,
      opencode_go_auto_enable_china_models: false,
      opencode_go_auto_apply_referral_rewards: true,
      opencode_go_referral_rewards_max_per_run: 0,
      opencode_go_auto_cancel_subscription_renewal: false,
    })
    const settings = JSON.parse(String(result.channel.settings))

    assert.deepEqual(settings.opencode_go, {
      model_protocols: {
        'glm-*': 'messages',
        'kimi-k3': 'chat',
      },
      default_protocol: 'responses',
      generic_failover_enabled: true,
      generic_failover_threshold: 2,
      generic_failover_window_seconds: 30,
      generic_failover_max_backups: 1,
      generic_failover_lease_seconds: 1800,
      affinity_fallback: 'token',
      load_aware_enabled: true,
      auto_enable_china_models: false,
      auto_apply_referral_rewards: true,
      referral_rewards_max_per_run: 0,
      auto_cancel_subscription_renewal: false,
    })
    assert.equal(
      channelFormSchema.safeParse({
        ...CHANNEL_FORM_DEFAULT_VALUES,
        name: 'OpenCode Go pool',
        type: CHANNEL_TYPE_OPENCODE_GO,
        models: 'glm-5.2',
        opencode_go_referral_rewards_max_per_run: 0,
      }).success,
      true
    )
  })

  test('round-trips an empty-pool channel without exposing a key', () => {
    const channel = channelSchema.parse({
      id: 62,
      type: CHANNEL_TYPE_OPENCODE_GO,
      key: '',
      status: 1,
      name: 'Empty OpenCode Go pool',
      created_time: 1,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      base_url: OPENCODE_GO_BASE_URL,
      models: 'glm-5.2',
      settings: JSON.stringify({
        retained_setting: 'keep-me',
        opencode_go: {
          default_protocol: 'messages',
          model_protocols: { 'glm-*': 'messages' },
          generic_failover_enabled: true,
          generic_failover_threshold: 3,
          generic_failover_window_seconds: 45,
          generic_failover_max_backups: 1,
          generic_failover_lease_seconds: 600,
          affinity_fallback: 'token',
          load_aware_enabled: true,
          auto_enable_china_models: true,
          auto_apply_referral_rewards: false,
          referral_rewards_max_per_run: 0,
          auto_cancel_subscription_renewal: false,
        },
      }),
    })
    const defaults = transformChannelToFormDefaults(channel)

    assert.equal(defaults.key, '')
    assert.equal(defaults.models, 'glm-5.2')
    assert.equal(defaults.opencode_go_default_protocol, 'messages')
    assert.equal(defaults.opencode_go_generic_failover_enabled, true)
    assert.equal(defaults.opencode_go_generic_failover_threshold, 3)
    assert.equal(defaults.opencode_go_generic_failover_window_seconds, 45)
    assert.equal(defaults.opencode_go_generic_failover_max_backups, 1)
    assert.equal(defaults.opencode_go_generic_failover_lease_seconds, 600)
    assert.equal(defaults.opencode_go_affinity_fallback, 'token')
    assert.equal(defaults.opencode_go_load_aware_enabled, true)
    assert.equal(defaults.opencode_go_referral_rewards_max_per_run, 0)
    const protocolOverrides = defaults.opencode_go_model_protocols
    assert.ok(protocolOverrides)
    assert.deepEqual(JSON.parse(protocolOverrides), {
      'glm-*': 'messages',
    })
    assert.equal(channelFormSchema.safeParse(defaults).success, true)

    const update = transformFormDataToUpdatePayload(defaults, channel.id)
    const settings = JSON.parse(String(update.settings))
    assert.equal('key' in update, false)
    assert.equal(update.models, 'glm-5.2')
    assert.equal(settings.retained_setting, 'keep-me')
    assert.equal(settings.opencode_go.referral_rewards_max_per_run, 0)
    assert.equal(settings.opencode_go.generic_failover_enabled, true)
    assert.equal(settings.opencode_go.generic_failover_threshold, 3)
    assert.equal(settings.opencode_go.generic_failover_window_seconds, 45)
    assert.equal(settings.opencode_go.generic_failover_max_backups, 1)
    assert.equal(settings.opencode_go.generic_failover_lease_seconds, 600)
  })

  test('rejects failover values outside the bounded first-release policy', () => {
    for (const invalid of [
      { opencode_go_generic_failover_threshold: 1 },
      { opencode_go_generic_failover_window_seconds: 301 },
      { opencode_go_generic_failover_max_backups: 2 },
      { opencode_go_generic_failover_lease_seconds: 86401 },
      { opencode_go_affinity_fallback: 'apikey' },
    ]) {
      const parsed = channelFormSchema.safeParse({
        ...CHANNEL_FORM_DEFAULT_VALUES,
        ...invalid,
        name: 'OpenCode Go pool',
        type: CHANNEL_TYPE_OPENCODE_GO,
        models: '',
      })
      assert.equal(parsed.success, false)
    }
  })
})
