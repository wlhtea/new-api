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
import { z } from 'zod'

import {
  openCodeGoRefreshSummarySchema,
  openCodeGoRiskRecheckSummarySchema,
  openCodeGoTaskProgressSchema,
  type OpenCodeGoIdentity,
  type OpenCodeGoPool,
  type OpenCodeGoQuotaWindow,
  type OpenCodeGoSystemTask,
  type OpenCodeGoWorkspace,
} from './opencode-go-schemas'

export const OPENCODE_GO_PROTOCOLS = ['chat', 'messages', 'responses'] as const
export type OpenCodeGoProtocol = (typeof OPENCODE_GO_PROTOCOLS)[number]

export const openCodeGoAccountGridClasses =
  'grid min-w-0 gap-3 md:grid-cols-2 xl:grid-cols-3'

export const openCodeGoQuotaLayoutClasses = {
  grid: 'grid min-w-0 gap-3 md:grid-cols-3',
  window:
    'border-border/70 flex min-h-44 min-w-0 flex-col gap-3 rounded-md border p-3',
} as const

const openCodeGoProtocolSchema = z.enum(OPENCODE_GO_PROTOCOLS)
const openCodeGoProtocolOverridesSchema = z.record(
  z.string(),
  openCodeGoProtocolSchema
)

export const openCodeGoPoolQueryKeys = {
  all: (channelId: number) =>
    [...(['channels', 'opencode-go'] as const), channelId] as const,
  pool: (channelId: number) =>
    [...openCodeGoPoolQueryKeys.all(channelId), 'pool'] as const,
  task: (channelId: number, kind: string, taskId: string) =>
    [...openCodeGoPoolQueryKeys.all(channelId), 'task', kind, taskId] as const,
}

export type OpenCodeGoWorkspaceRow = {
  identity: OpenCodeGoIdentity
  workspace: OpenCodeGoWorkspace
}

export type OpenCodeGoBulkResult = {
  key: string
  status: string
  error?: string
}

export type OpenCodeGoOrdinaryAction =
  | { kind: 'identity-label'; identityUid: string; label: string }
  | { kind: 'identity-toggle'; identityUid: string; enabled: boolean }
  | { kind: 'identity-refresh'; identityUid: string }
  | { kind: 'workspace-toggle'; workspaceUid: string; enabled: boolean }
  | { kind: 'workspace-refresh'; workspaceUid: string }
  | { kind: 'workspace-risk'; workspaceUid: string }
  | { kind: 'refresh-all' }
  | { kind: 'risk-all' }

export function getOpenCodeGoOrdinaryBusyKey(
  isPending: boolean,
  action: OpenCodeGoOrdinaryAction | undefined
): string | null {
  if (!isPending || !action) return null

  switch (action.kind) {
    case 'identity-label':
      return `identity:${action.identityUid}:label`
    case 'identity-toggle':
      return `identity:${action.identityUid}:toggle`
    case 'identity-refresh':
      return `identity:${action.identityUid}:refresh`
    case 'workspace-toggle':
      return `workspace:${action.workspaceUid}:toggle`
    case 'workspace-refresh':
      return `workspace:${action.workspaceUid}:refresh`
    case 'workspace-risk':
      return `workspace:${action.workspaceUid}:risk`
    case 'refresh-all':
      return 'bulk:refresh'
    case 'risk-all':
      return 'bulk:risk'
  }
}

const OPEN_CODE_GO_FAILED_BULK_STATUSES = new Set([
  'duplicate',
  'error',
  'failed',
  'not_recovered',
])

export function isOpenCodeGoBulkResultFailure(
  result: OpenCodeGoBulkResult
): boolean {
  return (
    OPEN_CODE_GO_FAILED_BULK_STATUSES.has(result.status) ||
    Boolean(result.error)
  )
}

export function parseOpenCodeGoProtocolOverrides(
  value: string | undefined
): Record<string, OpenCodeGoProtocol> {
  if (!value?.trim()) return {}

  const parsed: unknown = JSON.parse(value)
  const result = openCodeGoProtocolOverridesSchema.parse(parsed)
  const normalized: Record<string, OpenCodeGoProtocol> = {}

  for (const [rawPattern, rawProtocol] of Object.entries(result)) {
    const pattern = rawPattern.trim().toLowerCase()
    if (!pattern || pattern in normalized) {
      throw new Error('OpenCode Go model protocol patterns must be unique')
    }
    normalized[pattern] = rawProtocol
  }

  return normalized
}

export function stringifyOpenCodeGoProtocolOverrides(value: unknown): string {
  const parsed = openCodeGoProtocolOverridesSchema.safeParse(value)
  if (!parsed.success || Object.keys(parsed.data).length === 0) return ''
  return JSON.stringify(parsed.data, null, 2)
}

export function listOpenCodeGoWorkspaceRows(
  pool: OpenCodeGoPool,
  nonMembersOnly = false
): OpenCodeGoWorkspaceRow[] {
  const rows: OpenCodeGoWorkspaceRow[] = []
  for (const identity of pool.identities) {
    for (const workspace of identity.workspaces) {
      if (nonMembersOnly && workspace.membership_status === 'active') continue
      rows.push({ identity, workspace })
    }
  }
  return rows
}

export function getOpenCodeGoQuotaWindow(
  workspace: OpenCodeGoWorkspace,
  kind: OpenCodeGoQuotaWindow['kind']
): OpenCodeGoQuotaWindow | undefined {
  return workspace.quota_windows.find((window) => window.kind === kind)
}

export function isOpenCodeGoWorkspaceStale(
  workspace: OpenCodeGoWorkspace
): boolean {
  const quotaKinds = new Set(
    workspace.quota_windows.map((window) => window.kind)
  )
  return (
    workspace.quota_snapshot_status !== 'complete' ||
    workspace.quota_windows.length !== 3 ||
    quotaKinds.size !== 3 ||
    !(['rolling', 'weekly', 'monthly'] as const).every((kind) =>
      quotaKinds.has(kind)
    )
  )
}

export function isOpenCodeGoWorkspaceRecovered(
  workspace: OpenCodeGoWorkspace
): boolean {
  if (workspace.effective_state !== 'eligible') return false
  return [
    'risk_probe_succeeded',
    'model_probe_succeeded',
    'cooldown_expired',
  ].includes(workspace.health_observation)
}

export function formatOpenCodeGoResetCountdown(
  resetAt: number,
  nowSeconds: number,
  locale?: Intl.LocalesArgument
): string {
  const seconds = Math.max(0, Math.ceil(resetAt - nowSeconds))
  if (seconds === 0) return '0s'

  const formatter = new Intl.RelativeTimeFormat(locale, {
    numeric: 'always',
    style: 'short',
  })
  if (seconds < 60) return formatter.format(seconds, 'second')
  if (seconds < 3600) return formatter.format(Math.ceil(seconds / 60), 'minute')
  if (seconds < 86400) {
    return formatter.format(Math.ceil(seconds / 3600), 'hour')
  }
  return formatter.format(Math.ceil(seconds / 86400), 'day')
}

export function getOpenCodeGoTaskProgress(
  task: OpenCodeGoSystemTask | undefined
): { total: number; processed: number; progress: number } | null {
  if (!task) return null
  const parsed = openCodeGoTaskProgressSchema.safeParse(task.state)
  if (!parsed.success) return null
  const progress =
    parsed.data.progress ??
    (parsed.data.total === 0
      ? 100
      : (parsed.data.processed / parsed.data.total) * 100)
  return {
    total: parsed.data.total,
    processed: parsed.data.processed,
    progress: Math.min(100, Math.max(0, progress)),
  }
}

export function getOpenCodeGoTaskResults(
  task: OpenCodeGoSystemTask | undefined
): OpenCodeGoBulkResult[] {
  if (!task?.result) return []

  if (task.type === 'opencode_go_refresh') {
    const parsed = openCodeGoRefreshSummarySchema.safeParse(task.result)
    if (!parsed.success) return []
    return parsed.data.results.map((result, index) => ({
      key: `refresh-${index + 1}`,
      status: result.status,
      error: result.error,
    }))
  }

  if (task.type === 'opencode_go_risk_recheck') {
    const parsed = openCodeGoRiskRecheckSummarySchema.safeParse(task.result)
    if (!parsed.success) return []
    return parsed.data.results.map((result) => ({
      key: result.workspace_uid,
      status: result.status,
      error: result.error,
    }))
  }

  return []
}
