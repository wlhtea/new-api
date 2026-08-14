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
import type { KeySource } from './types'

export const KEY_SOURCE_TYPES = [
  'context_int',
  'context_string',
  'request_header',
  'gjson',
  'opencode_identity',
] as const satisfies readonly KeySource['type'][]

export function normalizeKeySource(src: Partial<KeySource>): KeySource {
  const type = (src.type || 'gjson') as KeySource['type']
  if (type === 'opencode_identity') return { type }
  if (type === 'gjson') return { type, key: '', path: src.path || '' }
  return { type, key: src.key || '', path: '' }
}

export function isValidKeySource(src: KeySource): boolean {
  if (src.type === 'opencode_identity') return true
  return src.type === 'gjson' ? !!src.path : !!src.key
}

export function normalizeValidKeySources(
  sources: Partial<KeySource>[]
): KeySource[] {
  return sources.map(normalizeKeySource).filter(isValidKeySource)
}

export function formatKeySource(src: KeySource): string {
  if (src.type === 'opencode_identity') {
    return 'OpenCode identity (token_id fallback)'
  }
  return `${src.type}:${src.type === 'gjson' ? src.path || '' : src.key || ''}`
}
