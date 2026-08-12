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
export const OPENCODE_GO_IDENTITY_PROXY_DEFAULT_COUNTRY = 'US'
export const OPENCODE_GO_IDENTITY_PROXY_DEFAULT_ROTATE_MINUTES = 10
export const OPENCODE_GO_IDENTITY_PROXY_MIN_ROTATE_MINUTES = 1
export const OPENCODE_GO_IDENTITY_PROXY_MAX_ROTATE_MINUTES = 180

export const OPENCODE_GO_IDENTITY_PROXY_TEMPLATE_ERROR =
  'Identity proxy routing requires a credentialed HTTP or HTTPS Proxy Address with exactly one valid sid component'
export const OPENCODE_GO_IDENTITY_PROXY_COUNTRY_ERROR =
  'Identity proxy country must contain exactly two ASCII letters'
export const OPENCODE_GO_IDENTITY_PROXY_ROTATION_ERROR =
  'Identity proxy rotation must be between 1 and 180 minutes'

export type OpenCodeGoIdentityProxyTemplate = {
  country?: string
  rotateMinutes?: number
}

type OpenCodeGoIdentityProxyPolicyPresence = {
  country: boolean
  rotateMinutes: boolean
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function isOpenCodeGoIdentityProxyCountry(value: string): boolean {
  return /^[A-Za-z]{2}$/.test(value.trim())
}

function isUsableUsernameValue(value: string): boolean {
  if (!value) return false
  return !['custom', 'zone', 'sid', 'time'].includes(value.toLowerCase())
}

function parseRotateMinutes(value: string): number | null {
  if (!/^[+-]?\d+$/.test(value)) return null
  const minutes = Number(value)
  if (
    !Number.isSafeInteger(minutes) ||
    minutes < OPENCODE_GO_IDENTITY_PROXY_MIN_ROTATE_MINUTES ||
    minutes > OPENCODE_GO_IDENTITY_PROXY_MAX_ROTATE_MINUTES
  ) {
    return null
  }
  return minutes
}

export function parseOpenCodeGoIdentityProxyTemplate(
  rawProxyURL: string | undefined
): OpenCodeGoIdentityProxyTemplate | null {
  const trimmedURL = rawProxyURL?.trim() || ''
  const schemeSeparatorIndex = trimmedURL.indexOf('://')
  if (schemeSeparatorIndex <= 0) return null
  const authorityAndSuffix = trimmedURL.slice(schemeSeparatorIndex + 3)
  const suffixIndex = authorityAndSuffix.search(/[/?#]/)
  if (suffixIndex >= 0 && authorityAndSuffix.slice(suffixIndex) !== '/') {
    return null
  }

  let parsedURL: URL
  let username: string
  try {
    parsedURL = new URL(trimmedURL)
    username = decodeURIComponent(parsedURL.username)
    decodeURIComponent(parsedURL.password)
  } catch {
    return null
  }

  if (
    (parsedURL.protocol !== 'http:' && parsedURL.protocol !== 'https:') ||
    !parsedURL.hostname ||
    parsedURL.port === '0' ||
    parsedURL.pathname !== '/' ||
    parsedURL.search ||
    parsedURL.hash ||
    !username.trim() ||
    !parsedURL.password
  ) {
    return null
  }

  const parts = username.split('_')
  let country: string | undefined
  let rotateMinutes: number | undefined
  let sidCount = 0
  let zoneCount = 0
  let timeCount = 0

  for (let index = 0; index < parts.length; index += 1) {
    const marker = parts[index].toLowerCase()
    if (marker === 'custom' && parts[index + 1]?.toLowerCase() === 'zone') {
      const value = parts[index + 2]
      if (!value || !isOpenCodeGoIdentityProxyCountry(value)) return null
      zoneCount += 1
      if (zoneCount > 1) return null
      country = value.toUpperCase()
      index += 2
      continue
    }
    if (marker === 'zone') {
      const value = parts[index + 1]
      if (!value || !isOpenCodeGoIdentityProxyCountry(value)) return null
      zoneCount += 1
      if (zoneCount > 1) return null
      country = value.toUpperCase()
      index += 1
      continue
    }
    if (marker === 'sid') {
      const value = parts[index + 1]
      if (!value || !isUsableUsernameValue(value)) return null
      sidCount += 1
      if (sidCount > 1) return null
      index += 1
      continue
    }
    if (marker === 'time') {
      const value = parts[index + 1]
      if (!value || !isUsableUsernameValue(value)) return null
      const minutes = parseRotateMinutes(value)
      if (minutes === null) return null
      timeCount += 1
      if (timeCount > 1) return null
      rotateMinutes = minutes
      index += 1
    }
  }

  if (sidCount !== 1) return null
  return { country, rotateMinutes }
}

function getExplicitPolicyPresence(
  rawSettings: string | undefined
): OpenCodeGoIdentityProxyPolicyPresence {
  try {
    const settings = JSON.parse(rawSettings || '{}')
    if (!isRecord(settings) || !isRecord(settings.opencode_go)) {
      return { country: false, rotateMinutes: false }
    }
    return {
      country: Object.hasOwn(settings.opencode_go, 'identity_proxy_country'),
      rotateMinutes: Object.hasOwn(
        settings.opencode_go,
        'identity_proxy_rotate_minutes'
      ),
    }
  } catch {
    return { country: false, rotateMinutes: false }
  }
}

export function inferOpenCodeGoIdentityProxyPolicyOnEnable(
  rawProxyURL: string | undefined,
  rawSettings: string | undefined
): OpenCodeGoIdentityProxyTemplate {
  const template = parseOpenCodeGoIdentityProxyTemplate(rawProxyURL)
  if (!template) return {}

  const explicitPolicy = getExplicitPolicyPresence(rawSettings)
  return {
    country: explicitPolicy.country ? undefined : template.country,
    rotateMinutes: explicitPolicy.rotateMinutes
      ? undefined
      : template.rotateMinutes,
  }
}
