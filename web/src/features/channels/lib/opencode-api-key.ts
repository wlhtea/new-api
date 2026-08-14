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
import { CHANNEL_TYPE_OPENCODE_API_KEY } from '../constants'
import type { ChannelCreateMode } from '../types'

export type ChannelAddMode = 'single' | 'batch' | 'multi_to_single'

export function resolveChannelCreateMode(
  type: number,
  mode: ChannelAddMode | undefined
): ChannelCreateMode {
  const requestedMode = mode || 'single'
  if (type !== CHANNEL_TYPE_OPENCODE_API_KEY) return requestedMode
  return requestedMode === 'batch' ? 'opencode_api_key_batch' : 'single'
}

export function isOpenCodeAPIKeyBatchForm(
  type: number,
  mode: ChannelAddMode | undefined,
  isEditing: boolean
): boolean {
  return (
    !isEditing && type === CHANNEL_TYPE_OPENCODE_API_KEY && mode === 'batch'
  )
}

export function shouldShowSharedChannelProxy(
  type: number,
  mode: ChannelAddMode | undefined,
  isEditing: boolean
): boolean {
  return !isOpenCodeAPIKeyBatchForm(type, mode, isEditing)
}

export function deduplicateOpenCodeAPIKeyBatchEntries(keysText: string): {
  deduplicatedText: string
  beforeCount: number
  afterCount: number
  removedCount: number
} {
  if (!keysText) {
    return {
      deduplicatedText: '',
      beforeCount: 0,
      afterCount: 0,
      removedCount: 0,
    }
  }

  const lines = keysText.replaceAll('\r\n', '\n').split('\n')
  const seenKeys = new Set<string>()
  const deduplicatedLines: string[] = []
  let beforeCount = 0
  let removedCount = 0

  for (const rawLine of lines) {
    const line = rawLine.trim()
    if (!line) {
      deduplicatedLines.push(line)
      continue
    }

    beforeCount += 1
    const delimiterIndex = line.indexOf('|')
    const key = (
      delimiterIndex < 0 ? line : line.slice(0, delimiterIndex)
    ).trim()
    if (!key || !seenKeys.has(key)) {
      if (key) seenKeys.add(key)
      deduplicatedLines.push(line)
      continue
    }

    removedCount += 1
  }

  return {
    deduplicatedText: deduplicatedLines.join('\n'),
    beforeCount,
    afterCount: beforeCount - removedCount,
    removedCount,
  }
}
