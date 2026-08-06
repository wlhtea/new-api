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
import type { OpenCodeGoQuotaWindow } from '../../lib/opencode-go-schemas'

export const OPENCODE_GO_IDENTITY_STATUS_LABELS: Record<string, string> = {
  pending: 'Pending',
  active: 'Active',
  stale: 'Stale',
  auth_error: 'Authentication error',
  manual_disabled: 'Manual disabled',
}

export const OPENCODE_GO_WORKSPACE_STATE_LABELS: Record<string, string> = {
  pending: 'Pending',
  eligible: 'Eligible',
  manual_disabled: 'Manual disabled',
  stale: 'Stale snapshot',
  auth_error: 'Authentication error',
  key_error: 'Credential error',
  membership_expired: 'Membership inactive',
  quota_exhausted: 'Quota exhausted',
  risk_blocked: 'Risk blocked',
  bulk_disabled: 'Bulk failures (awaiting verification)',
  cooldown: 'Cooling down',
}

export const OPENCODE_GO_QUOTA_WINDOW_LABELS: Record<
  OpenCodeGoQuotaWindow['kind'],
  string
> = {
  rolling: 'Rolling window',
  weekly: 'Weekly window',
  monthly: 'Monthly window',
}

type BadgeVariant =
  | 'default'
  | 'secondary'
  | 'destructive'
  | 'warning'
  | 'outline'

export function openCodeGoIdentityBadgeVariant(status: string): BadgeVariant {
  if (status === 'active') return 'default'
  if (status === 'auth_error') return 'destructive'
  if (status === 'stale') return 'warning'
  if (status === 'manual_disabled') return 'secondary'
  return 'outline'
}

export function openCodeGoWorkspaceBadgeVariant(state: string): BadgeVariant {
  if (state === 'eligible') return 'default'
  if (
    state === 'risk_blocked' ||
    state === 'auth_error' ||
    state === 'bulk_disabled'
  )
    return 'destructive'
  if (state === 'quota_exhausted' || state === 'cooldown') return 'warning'
  if (state === 'manual_disabled') return 'secondary'
  return 'outline'
}
