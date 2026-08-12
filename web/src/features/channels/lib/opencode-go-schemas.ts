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

const unixTimestampSchema = z.number().int().nonnegative()

export const openCodeGoLifecyclePolicySchema = z.object({
  automation_enabled: z.boolean(),
  auto_enable_china_models: z.boolean(),
  auto_apply_referral_rewards: z.boolean(),
  referral_rewards_max_per_run: z.number().int().min(0).max(20),
  auto_cancel_subscription_renewal: z.boolean(),
})

export const openCodeGoQuotaWindowSchema = z.object({
  kind: z.enum(['rolling', 'weekly', 'monthly']),
  source: z.literal('opencode_console_authoritative'),
  used_percent: z.number().finite().min(0).max(100),
  remaining_percent: z.number().finite().min(0).max(100),
  reset_seconds: z.number().int().nonnegative(),
  reset_at: unixTimestampSchema,
  fetched_at: unixTimestampSchema,
  amounts_authoritative: z.boolean(),
  calculated_limit_usd: z.number().finite().nonnegative(),
  calculated_used_usd: z.number().finite().nonnegative(),
  calculated_remaining_usd: z.number().finite().nonnegative(),
})

export const openCodeGoWorkspaceModelSchema = z.object({
  model: z.string(),
  discovered: z.boolean(),
  state: z.string(),
  disabled_until: unixTimestampSchema,
  last_error_code: z.string(),
  last_error: z.string(),
  health_observation: z.string(),
  health_observed_at: unixTimestampSchema,
  updated_at: unixTimestampSchema,
})

export const openCodeGoWorkspaceSchema = z.object({
  uid: z.string().min(1),
  name: z.string(),
  email: z.string(),
  has_api_key: z.boolean(),
  credential_status: z.string(),
  membership_status: z.string(),
  subscription_ends_at: unixTimestampSchema,
  renewal_cancelled_at: unixTimestampSchema,
  renewal_checked_at: unixTimestampSchema,
  renewal_cancel_error: z.string(),
  manual_enabled: z.boolean(),
  effective_state: z.string(),
  state_reason: z.string(),
  health_observation: z.string(),
  health_observed_at: unixTimestampSchema,
  cooldown_until: unixTimestampSchema,
  quota_snapshot_status: z.string(),
  quota_fetched_at: unixTimestampSchema,
  quota_next_refresh_at: unixTimestampSchema,
  quota_recovery_at: unixTimestampSchema,
  quota_parser_version: z.string(),
  quota_error: z.string(),
  quota_windows: z.array(openCodeGoQuotaWindowSchema),
  models: z.array(openCodeGoWorkspaceModelSchema),
  china_models_enabled: z.boolean().nullable(),
  china_models_checked_at: unixTimestampSchema,
  china_models_error: z.string(),
  referral_code: z.string(),
  available_referral_rewards: z.number().int().nonnegative(),
  used_referral_rewards: z.number().int().nonnegative(),
  referral_reward_eligible: z.boolean().default(false),
  referral_reward_applied_at: unixTimestampSchema,
  risk_detected_at: unixTimestampSchema,
  risk_last_checked_at: unixTimestampSchema,
  inflight: z.number().int().nonnegative().optional(),
  last_synced_at: unixTimestampSchema,
  last_error: z.string(),
  created_at: unixTimestampSchema,
  updated_at: unixTimestampSchema,
})

export const openCodeGoIdentitySchema = z.object({
  uid: z.string().min(1),
  label: z.string(),
  email: z.string(),
  status: z.string(),
  manual_enabled: z.boolean(),
  has_auth_cookie: z.boolean(),
  last_synced_at: unixTimestampSchema,
  last_error: z.string(),
  created_at: unixTimestampSchema,
  updated_at: unixTimestampSchema,
  workspaces: z.array(openCodeGoWorkspaceSchema),
})

export const openCodeGoOperationSchema = z.object({
  uid: z.string().min(1),
  workspace_uid: z.string(),
  action: z.string(),
  source: z.string(),
  status: z.string(),
  started_at: unixTimestampSchema,
  finished_at: unixTimestampSchema,
  result: z.string(),
  error: z.string(),
})

export const openCodeGoPoolSchema = z.object({
  channel_id: z.number().int().positive(),
  eligible_workspace_count: z.number().int().nonnegative(),
  crypto_secret_configured: z.boolean(),
  lifecycle_policy: openCodeGoLifecyclePolicySchema,
  identities: z.array(openCodeGoIdentitySchema),
  operations: z.array(openCodeGoOperationSchema),
})

export const openCodeGoImportResultSchema = z.object({
  index: z.number().int().positive(),
  status: z.string(),
  identity_uid: z.string().optional(),
  workspace_count: z.number().int().nonnegative().optional(),
  error: z.string().optional(),
})

export const openCodeGoRiskRecheckResultSchema = z.object({
  channel_id: z.number().int().positive(),
  workspace_uid: z.string().min(1),
  model: z.string().optional(),
  status: z.string(),
  blocked: z.boolean(),
  upstream_status: z.number().int().optional(),
  error_type: z.string().optional(),
  error: z.string().optional(),
})

export const openCodeGoSystemTaskSchema = z.object({
  id: z.number().int(),
  task_id: z.string().min(1),
  type: z.string(),
  status: z.enum(['pending', 'running', 'succeeded', 'failed']),
  active_key: z.string().optional(),
  payload: z.unknown().optional(),
  state: z.unknown().optional(),
  result: z.unknown().optional(),
  error: z.string(),
  locked_by: z.string(),
  created_at: unixTimestampSchema,
  updated_at: unixTimestampSchema,
})

export const openCodeGoTaskProgressSchema = z.object({
  total: z.number().int().nonnegative(),
  processed: z.number().int().nonnegative(),
  progress: z.number().finite().optional(),
})

export const openCodeGoRefreshSummarySchema = z.object({
  total: z.number().int().nonnegative(),
  processed: z.number().int().nonnegative(),
  succeeded: z.number().int().nonnegative(),
  failed: z.number().int().nonnegative(),
  results: z.array(
    z.object({
      channel_id: z.number().int().positive(),
      status: z.string(),
      error: z.string().optional(),
    })
  ),
})

export const openCodeGoRiskRecheckSummarySchema = z.object({
  total: z.number().int().nonnegative(),
  processed: z.number().int().nonnegative(),
  recovered: z.number().int().nonnegative(),
  blocked: z.number().int().nonnegative(),
  failed: z.number().int().nonnegative(),
  results: z.array(openCodeGoRiskRecheckResultSchema),
})

export const openCodeGoApiEnvelopeSchema = <T extends z.ZodType>(
  dataSchema: T
) =>
  z.object({
    success: z.boolean(),
    message: z.string().optional(),
    data: dataSchema.optional(),
  })

export type OpenCodeGoLifecyclePolicy = z.infer<
  typeof openCodeGoLifecyclePolicySchema
>
export type OpenCodeGoQuotaWindow = z.infer<typeof openCodeGoQuotaWindowSchema>
export type OpenCodeGoWorkspaceModel = z.infer<
  typeof openCodeGoWorkspaceModelSchema
>
export type OpenCodeGoWorkspace = z.infer<typeof openCodeGoWorkspaceSchema>
export type OpenCodeGoIdentity = z.infer<typeof openCodeGoIdentitySchema>
export type OpenCodeGoOperation = z.infer<typeof openCodeGoOperationSchema>
export type OpenCodeGoPool = z.infer<typeof openCodeGoPoolSchema>
export type OpenCodeGoImportResult = z.infer<
  typeof openCodeGoImportResultSchema
>
export type OpenCodeGoRiskRecheckResult = z.infer<
  typeof openCodeGoRiskRecheckResultSchema
>
export type OpenCodeGoSystemTask = z.infer<typeof openCodeGoSystemTaskSchema>
