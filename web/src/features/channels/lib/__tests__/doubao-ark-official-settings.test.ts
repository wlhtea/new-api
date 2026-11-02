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
import { describe, expect, test } from 'vitest'

import { channelSchema } from '../../types'
import {
  buildSettingJSON,
  CHANNEL_FORM_DEFAULT_VALUES,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
} from '../channel-form'

const OFFICIAL_PLUGIN_KEY = 'doubao_ark_official'

describe('Doubao Ark official task permissions', () => {
  test.each([false, true])(
    'round-trips enabled=%s through create, update and reload',
    (enabled) => {
      const channel = channelSchema.parse({
        id: 42,
        name: 'Doubao Ark Official',
        key: '',
        type: 61,
        status: 1,
        created_time: 0,
        test_time: 0,
        response_time: 0,
        balance_updated_time: 0,
        setting: JSON.stringify({
          task_plugin_key: OFFICIAL_PLUGIN_KEY,
          allow_official_task_list: enabled,
          allow_official_task_delete: enabled,
        }),
      })

      const values = transformChannelToFormDefaults(channel)
      expect(values).toMatchObject({
        allow_official_task_list: enabled,
        allow_official_task_delete: enabled,
      })

      const payloads = [
        transformFormDataToCreatePayload(values).channel,
        transformFormDataToUpdatePayload(values, channel.id),
      ]
      for (const payload of payloads) {
        expect(typeof payload.setting).toBe('string')
        const setting = JSON.parse(payload.setting as string)
        expect(setting).toMatchObject({
          task_plugin_key: OFFICIAL_PLUGIN_KEY,
          allow_official_task_list: enabled,
          allow_official_task_delete: enabled,
        })
        expect(
          transformChannelToFormDefaults({
            ...channel,
            setting: payload.setting,
          })
        ).toMatchObject({
          allow_official_task_list: enabled,
          allow_official_task_delete: enabled,
        })
      }
    }
  )

  test('defaults missing values to false and serializes both explicitly', () => {
    expect(CHANNEL_FORM_DEFAULT_VALUES).toMatchObject({
      allow_official_task_list: false,
      allow_official_task_delete: false,
    })

    const channel = channelSchema.parse({
      id: 42,
      name: 'Doubao Ark Official',
      key: '',
      type: 61,
      status: 1,
      created_time: 0,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      setting: JSON.stringify({ task_plugin_key: OFFICIAL_PLUGIN_KEY }),
    })
    expect(transformChannelToFormDefaults(channel)).toMatchObject({
      allow_official_task_list: false,
      allow_official_task_delete: false,
    })

    const setting = JSON.parse(
      buildSettingJSON({
        ...CHANNEL_FORM_DEFAULT_VALUES,
        type: 61,
        task_plugin_key: OFFICIAL_PLUGIN_KEY,
      })
    )
    expect(setting).toMatchObject({
      allow_official_task_list: false,
      allow_official_task_delete: false,
    })
  })

  test.each([
    { type: 60, taskPluginKey: OFFICIAL_PLUGIN_KEY },
    { type: 61, taskPluginKey: 'doubao' },
    { type: 61, taskPluginKey: `${OFFICIAL_PLUGIN_KEY} ` },
  ])(
    'excludes permissions for type=$type plugin=$taskPluginKey',
    ({ type, taskPluginKey }) => {
      const setting = JSON.parse(
        buildSettingJSON({
          ...CHANNEL_FORM_DEFAULT_VALUES,
          type,
          task_plugin_key: taskPluginKey,
          allow_official_task_list: true,
          allow_official_task_delete: true,
        })
      )
      expect(setting).not.toHaveProperty('allow_official_task_list')
      expect(setting).not.toHaveProperty('allow_official_task_delete')
    }
  )
})
