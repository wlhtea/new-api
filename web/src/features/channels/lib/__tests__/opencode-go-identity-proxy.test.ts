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
  inferOpenCodeGoIdentityProxyPolicyOnEnable,
  parseOpenCodeGoIdentityProxyTemplate,
} from '../opencode-go-identity-proxy'

describe('OpenCode Go identity proxy template', () => {
  test('infers optional policy values from a compatible encoded username', () => {
    const template = parseOpenCodeGoIdentityProxyTemplate(
      'https://account%5Fcustom%5Fzone%5Fgb%5Fsid%5F123%5Ftime%5F20:secret@proxy.example:8443'
    )

    assert.deepEqual(template, { country: 'GB', rotateMinutes: 20 })
    assert.ok(template)
    assert.equal('sid' in template, false)
  })

  test('accepts a template that omits optional zone and time components', () => {
    assert.deepEqual(
      parseOpenCodeGoIdentityProxyTemplate(
        'http://account_plan_premium_sid_1:secret@proxy.example:8080/'
      ),
      { country: undefined, rotateMinutes: undefined }
    )
  })

  test('rejects incompatible credentials and ambiguous IPWO components', () => {
    const invalidTemplates = [
      '',
      'socks5://account_sid_1:secret@proxy.example:1080',
      'http://account_sid_1@proxy.example:8080',
      'http://account_zone_US_time_10:secret@proxy.example:8080',
      'http://account_sid_1_sid_2:secret@proxy.example:8080',
      'http://account_zone_US_custom_zone_GB_sid_1:secret@proxy.example:8080',
      'http://account_sid_1_time_10_time_20:secret@proxy.example:8080',
      'http://account_sid_time_10:secret@proxy.example:8080',
      'http://account_zone_USA_sid_1:secret@proxy.example:8080',
      'http://account_sid_1_time_181:secret@proxy.example:8080',
      'http://account%ZZ_sid_1:secret@proxy.example:8080',
      'http://account_sid_1:secret@proxy.example:8080/path',
      'http://account_sid_1:secret@proxy.example:8080/?query',
      'http://account_sid_1:secret@proxy.example:8080/#fragment',
    ]

    for (const proxy of invalidTemplates) {
      assert.equal(parseOpenCodeGoIdentityProxyTemplate(proxy), null, proxy)
    }
  })

  test('infers only fields absent from the persisted policy object', () => {
    const proxy =
      'http://account_zone_GB_sid_1_time_20:secret@proxy.example:8080'

    assert.deepEqual(inferOpenCodeGoIdentityProxyPolicyOnEnable(proxy, '{}'), {
      country: 'GB',
      rotateMinutes: 20,
    })
    assert.deepEqual(
      inferOpenCodeGoIdentityProxyPolicyOnEnable(
        proxy,
        JSON.stringify({
          opencode_go: {
            identity_proxy_country: 'US',
            identity_proxy_rotate_minutes: 10,
          },
        })
      ),
      { country: undefined, rotateMinutes: undefined }
    )
    assert.deepEqual(
      inferOpenCodeGoIdentityProxyPolicyOnEnable(
        proxy,
        JSON.stringify({
          opencode_go: { identity_proxy_country: 'US' },
        })
      ),
      { country: undefined, rotateMinutes: 20 }
    )
  })
})
