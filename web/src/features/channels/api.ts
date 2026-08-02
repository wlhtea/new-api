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

import { getGroups as getUserGroups } from '@/features/users/api'
import { api, type ApiRequestConfig } from '@/lib/api'

import {
  openCodeGoApiEnvelopeSchema,
  openCodeGoImportResultSchema,
  openCodeGoLifecyclePolicySchema,
  openCodeGoOperationSchema,
  openCodeGoPoolSchema,
  openCodeGoRiskRecheckResultSchema,
  openCodeGoSystemTaskSchema,
  type OpenCodeGoLifecyclePolicy,
  type OpenCodeGoPool,
  type OpenCodeGoSystemTask,
} from './lib/opencode-go-schemas'
import type {
  AddChannelRequest,
  BatchDeleteParams,
  BatchSetTagParams,
  Channel,
  ChannelBalanceResponse,
  ChannelOpsResponse,
  ChannelTestResponse,
  CopyChannelParams,
  CopyChannelResponse,
  FetchModelsResponse,
  GetChannelResponse,
  GetChannelsParams,
  GetChannelsResponse,
  MultiKeyManageParams,
  MultiKeyStatusResponse,
  SearchChannelsParams,
  SearchChannelsResponse,
  TagOperationParams,
} from './types'

const channelActionConfig = (
  config: ApiRequestConfig = {}
): ApiRequestConfig => ({
  ...config,
  skipBusinessError: true,
  skipErrorHandler: true,
})

const openCodeGoImportMutationSchema = z.object({
  results: z.array(openCodeGoImportResultSchema),
  pool: openCodeGoPoolSchema,
})
const openCodeGoTaskStartSchema = z.object({
  task: openCodeGoSystemTaskSchema,
  created: z.boolean(),
})
const openCodeGoRiskMutationSchema = z.object({
  result: openCodeGoRiskRecheckResultSchema,
  pool: openCodeGoPoolSchema,
})
const openCodeGoDeleteNonMembersSchema = z.object({
  deleted_count: z.number().int().nonnegative(),
  pool: openCodeGoPoolSchema,
})
const openCodeGoChinaModelsMutationSchema = z.object({
  operation: openCodeGoOperationSchema,
  pool: openCodeGoPoolSchema,
})
const openCodeGoReferralMutationSchema = z.object({
  summary: z.object({
    attempted: z.number().int().nonnegative(),
    applied: z.number().int().nonnegative(),
  }),
  pool: openCodeGoPoolSchema,
})
const openCodeGoCancellationMutationSchema = z.object({
  operation: openCodeGoOperationSchema,
  cancellation: z.object({
    already_cancelled: z.boolean(),
    current_period_end: z.number().int().nonnegative(),
  }),
  pool: openCodeGoPoolSchema,
})

function openCodeGoActionConfig(proofToken?: string): ApiRequestConfig {
  return channelActionConfig({
    headers: proofToken ? { 'X-Security-Proof': proofToken } : undefined,
  })
}

function requireOpenCodeGoData<T>(
  schema: z.ZodType<T>,
  payload: unknown,
  fallbackMessage: string
): T {
  const response = openCodeGoApiEnvelopeSchema(schema).parse(payload)
  if (!response.success || response.data === undefined) {
    throw new Error(response.message || fallbackMessage)
  }
  return response.data
}

export type CodexUsageResponse = {
  success: boolean
  message?: string
  upstream_status?: number
  data?: Record<string, unknown>
}

export type CodexResetCreditsResponse = CodexUsageResponse

export type CodexUsageResetResponse = CodexUsageResponse

export type CodexCredentialRefreshResponse = {
  success: boolean
  message?: string
  data?: {
    expires_at?: string
    last_refresh?: string
    account_id?: string
    email?: string
    channel_id?: number
    channel_type?: number
    channel_name?: string
  }
}

// ============================================================================
// Base Channel CRUD Operations
// ============================================================================

/**
 * Get paginated list of channels
 */
export async function getChannels(
  params: GetChannelsParams = {}
): Promise<GetChannelsResponse> {
  const res = await api.get('/api/channel', { params })
  return res.data
}

/**
 * Search channels with filters
 */
export async function searchChannels(
  params: SearchChannelsParams
): Promise<SearchChannelsResponse> {
  const res = await api.get('/api/channel/search', { params })
  return res.data
}

/**
 * Get single channel by ID
 */
export async function getChannel(id: number): Promise<GetChannelResponse> {
  const res = await api.get(`/api/channel/${id}`)
  return res.data
}

/**
 * Get channel operations summary for administrators
 */
export async function getChannelOps(): Promise<ChannelOpsResponse> {
  const res = await api.get('/api/channel/ops', channelActionConfig())
  return res.data
}

/**
 * Create new channel(s)
 * Supports single, batch, and multi-key modes
 */
export async function createChannel(
  data: AddChannelRequest
): Promise<{ success: boolean; message?: string }> {
  const res = await api.post('/api/channel', data, channelActionConfig())
  return res.data
}

/**
 * Update existing channel
 */
export async function updateChannel(
  id: number,
  data: Partial<Channel>
): Promise<{ success: boolean; message?: string; data?: Channel }> {
  const res = await api.put(
    '/api/channel/',
    { id, ...data },
    channelActionConfig()
  )
  return res.data
}

/**
 * Update channel enabled/disabled status.
 */
export async function updateChannelStatus(
  id: number,
  status: number
): Promise<{ success: boolean; message?: string; data?: boolean }> {
  const res = await api.post(
    `/api/channel/${id}/status`,
    { status },
    channelActionConfig()
  )
  return res.data
}

/**
 * Batch update channel enabled/disabled status.
 */
export async function batchUpdateChannelStatus(
  ids: number[],
  status: number
): Promise<{ success: boolean; message?: string; data?: number }> {
  const res = await api.post(
    '/api/channel/status/batch',
    { ids, status },
    channelActionConfig()
  )
  return res.data
}

/**
 * Delete single channel
 */
export async function deleteChannel(
  id: number
): Promise<{ success: boolean; message?: string }> {
  const res = await api.delete(`/api/channel/${id}`, channelActionConfig())
  return res.data
}

/**
 * Batch delete channels
 */
export async function batchDeleteChannels(
  data: BatchDeleteParams
): Promise<{ success: boolean; message?: string; data?: number }> {
  const res = await api.post('/api/channel/batch', data, channelActionConfig())
  return res.data
}

/**
 * Batch set tag for channels
 */
export async function batchSetChannelTag(
  data: BatchSetTagParams
): Promise<{ success: boolean; message?: string; data?: number }> {
  const res = await api.post(
    '/api/channel/batch/tag',
    data,
    channelActionConfig()
  )
  return res.data
}

// ============================================================================
// Channel Operations
// ============================================================================

/**
 * Test channel connectivity
 */
export async function testChannel(
  id: number,
  params?: { model?: string; endpoint_type?: string; stream?: boolean }
): Promise<ChannelTestResponse> {
  const res = await api.get(
    `/api/channel/test/${id}`,
    channelActionConfig({ params })
  )
  return res.data
}

/**
 * Update channel balance
 */
export async function updateChannelBalance(
  id: number
): Promise<ChannelBalanceResponse> {
  const res = await api.get(
    `/api/channel/update_balance/${id}`,
    channelActionConfig()
  )
  return res.data
}

/**
 * Fetch available models from upstream provider
 */
export async function fetchUpstreamModels(
  id: number
): Promise<FetchModelsResponse> {
  const res = await api.get(
    `/api/channel/fetch_models/${id}`,
    channelActionConfig()
  )
  return res.data
}

/**
 * Copy/clone a channel
 */
export async function copyChannel(
  id: number,
  params: CopyChannelParams = {}
): Promise<CopyChannelResponse> {
  const res = await api.post(
    `/api/channel/copy/${id}`,
    null,
    channelActionConfig({ params })
  )
  return res.data
}

/**
 * Fix channel abilities
 */
export async function fixChannelAbilities(): Promise<{
  success: boolean
  message?: string
  data?: { success: number; fails: number }
}> {
  const res = await api.post(
    '/api/channel/fix',
    undefined,
    channelActionConfig()
  )
  return res.data
}

/**
 * Delete all disabled channels
 */
export async function deleteDisabledChannels(): Promise<{
  success: boolean
  message?: string
  data?: number
}> {
  const res = await api.delete('/api/channel/disabled', channelActionConfig())
  return res.data
}

/**
 * Get channel key (requires 2FA verification)
 */
export async function getChannelKey(
  id: number,
  proofToken?: string
): Promise<{ success: boolean; message?: string; data?: { key: string } }> {
  const res = await api.post(
    `/api/channel/${id}/key`,
    undefined,
    channelActionConfig({
      headers: proofToken ? { 'X-Security-Proof': proofToken } : undefined,
    })
  )
  return res.data
}

// ============================================================================
// Codex Channel Operations
// ============================================================================

export async function refreshCodexCredential(
  channelId: number
): Promise<CodexCredentialRefreshResponse> {
  const res = await api.post(
    `/api/channel/${channelId}/codex/refresh`,
    {},
    channelActionConfig()
  )
  return res.data
}

export async function getCodexUsage(
  channelId: number
): Promise<CodexUsageResponse> {
  const res = await api.get(
    `/api/channel/${channelId}/codex/usage`,
    channelActionConfig({ disableDuplicate: true })
  )
  return res.data
}

export async function getCodexResetCredits(
  channelId: number
): Promise<CodexResetCreditsResponse> {
  const res = await api.get(
    `/api/channel/${channelId}/codex/usage/reset-credits`,
    channelActionConfig({ disableDuplicate: true })
  )
  return res.data
}

export async function resetCodexUsage(
  channelId: number
): Promise<CodexUsageResetResponse> {
  const res = await api.post(
    `/api/channel/${channelId}/codex/usage/reset`,
    {},
    channelActionConfig({ disableDuplicate: true })
  )
  return res.data
}

// ============================================================================
// Multi-Key Management
// ============================================================================

/**
 * Manage multi-key channel operations
 */
export async function manageMultiKeys(
  params: MultiKeyManageParams
): Promise<MultiKeyStatusResponse | { success: boolean; message?: string }> {
  const res = await api.post(
    '/api/channel/multi_key/manage',
    params,
    channelActionConfig()
  )
  return res.data
}

/**
 * Get key status for multi-key channel
 */
export async function getMultiKeyStatus(
  channelId: number,
  page = 1,
  pageSize = 50,
  status?: number
): Promise<MultiKeyStatusResponse> {
  return manageMultiKeys({
    channel_id: channelId,
    action: 'get_key_status',
    page,
    page_size: pageSize,
    status,
  }) as Promise<MultiKeyStatusResponse>
}

/**
 * Enable a specific key in multi-key channel
 */
export async function enableMultiKey(
  channelId: number,
  keyIndex: number
): Promise<{ success: boolean; message?: string }> {
  return manageMultiKeys({
    channel_id: channelId,
    action: 'enable_key',
    key_index: keyIndex,
  }) as Promise<{ success: boolean; message?: string }>
}

/**
 * Disable a specific key in multi-key channel
 */
export async function disableMultiKey(
  channelId: number,
  keyIndex: number
): Promise<{ success: boolean; message?: string }> {
  return manageMultiKeys({
    channel_id: channelId,
    action: 'disable_key',
    key_index: keyIndex,
  }) as Promise<{ success: boolean; message?: string }>
}

/**
 * Delete a specific key in multi-key channel
 */
export async function deleteMultiKey(
  channelId: number,
  keyIndex: number
): Promise<{ success: boolean; message?: string }> {
  return manageMultiKeys({
    channel_id: channelId,
    action: 'delete_key',
    key_index: keyIndex,
  }) as Promise<{ success: boolean; message?: string }>
}

/**
 * Enable all keys in multi-key channel
 */
export async function enableAllMultiKeys(
  channelId: number
): Promise<{ success: boolean; message?: string }> {
  return manageMultiKeys({
    channel_id: channelId,
    action: 'enable_all_keys',
  }) as Promise<{ success: boolean; message?: string }>
}

/**
 * Disable all keys in multi-key channel
 */
export async function disableAllMultiKeys(
  channelId: number
): Promise<{ success: boolean; message?: string }> {
  return manageMultiKeys({
    channel_id: channelId,
    action: 'disable_all_keys',
  }) as Promise<{ success: boolean; message?: string }>
}

/**
 * Delete all disabled keys in multi-key channel
 */
export async function deleteDisabledMultiKeys(
  channelId: number
): Promise<{ success: boolean; message?: string; data?: number }> {
  return manageMultiKeys({
    channel_id: channelId,
    action: 'delete_disabled_keys',
  }) as Promise<{ success: boolean; message?: string; data?: number }>
}

// ============================================================================
// Tag Operations
// ============================================================================

/**
 * Enable all channels with a specific tag
 */
export async function enableTagChannels(
  tag: string
): Promise<{ success: boolean; message?: string }> {
  const res = await api.post(
    '/api/channel/tag/enabled',
    { tag },
    channelActionConfig()
  )
  return res.data
}

/**
 * Disable all channels with a specific tag
 */
export async function disableTagChannels(
  tag: string
): Promise<{ success: boolean; message?: string }> {
  const res = await api.post(
    '/api/channel/tag/disabled',
    { tag },
    channelActionConfig()
  )
  return res.data
}

/**
 * Edit all channels with a specific tag
 */
export async function editTagChannels(
  params: TagOperationParams
): Promise<{ success: boolean; message?: string }> {
  const res = await api.put('/api/channel/tag', params, channelActionConfig())
  return res.data
}

/**
 * Get models for a specific tag
 */
export async function getTagModels(
  tag: string
): Promise<{ success: boolean; message?: string; data?: string }> {
  const res = await api.get('/api/channel/tag/models', { params: { tag } })
  return res.data
}

// ============================================================================
// Utility Functions
// ============================================================================

/**
 * Fetch models from the current unsaved channel form configuration.
 */
export async function fetchModels(data: {
  base_url: string
  type: number
  key?: string
  channel_id?: number
  advanced_custom?: string
  header_override?: string
  proxy?: string
}): Promise<FetchModelsResponse> {
  const res = await api.post(
    '/api/channel/fetch_models',
    data,
    channelActionConfig()
  )
  return res.data
}

/**
 * Delete an Ollama model from a channel
 */
export async function deleteOllamaModel(params: {
  channel_id: number
  model_name: string
}): Promise<{ success: boolean; message?: string }> {
  const res = await api.delete(
    '/api/channel/ollama/delete',
    channelActionConfig({ data: params })
  )
  return res.data
}

/**
 * Test all enabled channels
 */
export async function testAllChannels(): Promise<{
  success: boolean
  message?: string
}> {
  const res = await api.get('/api/channel/test', channelActionConfig())
  return res.data
}

/**
 * Update balance for all enabled channels
 */
export async function updateAllChannelsBalance(): Promise<{
  success: boolean
  message?: string
}> {
  const res = await api.get(
    '/api/channel/update_balance',
    channelActionConfig()
  )
  return res.data
}

/**
 * Get all available models
 */
export async function getAllModels(): Promise<{
  success: boolean
  message?: string
  data?: Array<{ id: string; [key: string]: unknown }>
}> {
  const res = await api.get('/api/channel/models')
  return res.data
}

/**
 * Get all enabled models
 */
export async function getEnabledModels(): Promise<{
  success: boolean
  message?: string
  data?: string[]
}> {
  const res = await api.get('/api/channel/models_enabled')
  return res.data
}

// ============================================================================
// Ollama Utilities
// ============================================================================

/**
 * Check Ollama version for a given channel
 */
export async function getOllamaVersion(
  channelId: number
): Promise<{ success: boolean; message?: string; data?: { version: string } }> {
  const res = await api.get(`/api/channel/ollama/version/${channelId}`)
  return res.data
}

// ============================================================================
// Group Management
// ============================================================================

/**
 * Get all available groups (re-exported from users API for convenience)
 */
export const getGroups = getUserGroups

// ============================================================================
// Prefill Groups (Model Groups)
// ============================================================================

/**
 * Get prefill groups for quick model selection
 */
export async function getPrefillGroups(
  type: 'model' | 'group' = 'model'
): Promise<{
  success: boolean
  message?: string
  data?: Array<{ id: number; name: string; items: string | string[] }>
}> {
  const res = await api.get('/api/prefill_group', { params: { type } })
  return res.data
}

// ============================================================================
// OpenCode Go Account Pool
// ============================================================================

export async function getOpenCodeGoPool(
  channelId: number
): Promise<OpenCodeGoPool> {
  const res = await api.get(
    `/api/channel/${channelId}/opencode-go/pool`,
    channelActionConfig({ disableDuplicate: true })
  )
  return requireOpenCodeGoData(
    openCodeGoPoolSchema,
    res.data,
    'Failed to load OpenCode Go account pool'
  )
}

export async function importOpenCodeGoIdentities(
  channelId: number,
  input: { label: string; authCookies: string },
  proofToken?: string
) {
  const res = await api.post(
    `/api/channel/${channelId}/opencode-go/identities/import`,
    { label: input.label, auth_cookies: input.authCookies },
    openCodeGoActionConfig(proofToken)
  )
  return requireOpenCodeGoData(
    openCodeGoImportMutationSchema,
    res.data,
    'Failed to import OpenCode Go accounts'
  )
}

export async function updateOpenCodeGoIdentityLabel(
  channelId: number,
  identityUid: string,
  label: string
): Promise<OpenCodeGoPool> {
  const res = await api.patch(
    `/api/channel/${channelId}/opencode-go/identities/${encodeURIComponent(identityUid)}`,
    { label },
    channelActionConfig()
  )
  return requireOpenCodeGoData(
    openCodeGoPoolSchema,
    res.data,
    'Failed to update OpenCode Go account label'
  )
}

export async function setOpenCodeGoIdentityEnabled(
  channelId: number,
  identityUid: string,
  enabled: boolean
): Promise<OpenCodeGoPool> {
  const res = await api.patch(
    `/api/channel/${channelId}/opencode-go/identities/${encodeURIComponent(identityUid)}/enabled`,
    { enabled },
    channelActionConfig()
  )
  return requireOpenCodeGoData(
    openCodeGoPoolSchema,
    res.data,
    'Failed to update OpenCode Go account state'
  )
}

export async function replaceOpenCodeGoIdentityCookie(
  channelId: number,
  identityUid: string,
  authCookie: string,
  proofToken?: string
): Promise<OpenCodeGoPool> {
  const res = await api.put(
    `/api/channel/${channelId}/opencode-go/identities/${encodeURIComponent(identityUid)}/cookie`,
    { auth_cookie: authCookie },
    openCodeGoActionConfig(proofToken)
  )
  return requireOpenCodeGoData(
    openCodeGoPoolSchema,
    res.data,
    'Failed to replace OpenCode Go account Cookie'
  )
}

export async function refreshOpenCodeGoIdentity(
  channelId: number,
  identityUid: string
): Promise<OpenCodeGoPool> {
  const res = await api.post(
    `/api/channel/${channelId}/opencode-go/identities/${encodeURIComponent(identityUid)}/refresh`,
    undefined,
    channelActionConfig()
  )
  return requireOpenCodeGoData(
    openCodeGoPoolSchema,
    res.data,
    'Failed to refresh OpenCode Go account'
  )
}

export async function deleteOpenCodeGoIdentity(
  channelId: number,
  identityUid: string,
  proofToken?: string
): Promise<OpenCodeGoPool> {
  const res = await api.delete(
    `/api/channel/${channelId}/opencode-go/identities/${encodeURIComponent(identityUid)}`,
    openCodeGoActionConfig(proofToken)
  )
  return requireOpenCodeGoData(
    openCodeGoPoolSchema,
    res.data,
    'Failed to delete OpenCode Go account'
  )
}

export async function setOpenCodeGoWorkspaceEnabled(
  channelId: number,
  workspaceUid: string,
  enabled: boolean
): Promise<OpenCodeGoPool> {
  const res = await api.patch(
    `/api/channel/${channelId}/opencode-go/workspaces/${encodeURIComponent(workspaceUid)}/enabled`,
    { enabled },
    channelActionConfig()
  )
  return requireOpenCodeGoData(
    openCodeGoPoolSchema,
    res.data,
    'Failed to update OpenCode Go workspace state'
  )
}

export async function refreshOpenCodeGoWorkspace(
  channelId: number,
  workspaceUid: string
): Promise<OpenCodeGoPool> {
  const res = await api.post(
    `/api/channel/${channelId}/opencode-go/workspaces/${encodeURIComponent(workspaceUid)}/refresh`,
    undefined,
    channelActionConfig()
  )
  return requireOpenCodeGoData(
    openCodeGoPoolSchema,
    res.data,
    'Failed to refresh OpenCode Go workspace'
  )
}

export async function recheckOpenCodeGoWorkspaceRisk(
  channelId: number,
  workspaceUid: string
) {
  const res = await api.post(
    `/api/channel/${channelId}/opencode-go/workspaces/${encodeURIComponent(workspaceUid)}/risk-recheck`,
    undefined,
    channelActionConfig()
  )
  return requireOpenCodeGoData(
    openCodeGoRiskMutationSchema,
    res.data,
    'Failed to recheck OpenCode Go workspace risk'
  )
}

export async function deleteOpenCodeGoWorkspace(
  channelId: number,
  workspaceUid: string,
  proofToken?: string
): Promise<OpenCodeGoPool> {
  const res = await api.delete(
    `/api/channel/${channelId}/opencode-go/workspaces/${encodeURIComponent(workspaceUid)}`,
    openCodeGoActionConfig(proofToken)
  )
  return requireOpenCodeGoData(
    openCodeGoPoolSchema,
    res.data,
    'Failed to delete OpenCode Go workspace'
  )
}

export async function deleteOpenCodeGoNonMemberWorkspaces(
  channelId: number,
  proofToken?: string
) {
  const res = await api.delete(
    `/api/channel/${channelId}/opencode-go/workspaces/non-members`,
    openCodeGoActionConfig(proofToken)
  )
  return requireOpenCodeGoData(
    openCodeGoDeleteNonMembersSchema,
    res.data,
    'Failed to delete non-member OpenCode Go workspaces'
  )
}

export async function refreshAllOpenCodeGoIdentities(channelId: number) {
  const res = await api.post(
    `/api/channel/${channelId}/opencode-go/refresh-all`,
    undefined,
    channelActionConfig()
  )
  return requireOpenCodeGoData(
    openCodeGoTaskStartSchema,
    res.data,
    'Failed to start OpenCode Go refresh task'
  )
}

export async function recheckAllOpenCodeGoWorkspaceRisks(channelId: number) {
  const res = await api.post(
    `/api/channel/${channelId}/opencode-go/risk-recheck-all`,
    undefined,
    channelActionConfig()
  )
  return requireOpenCodeGoData(
    openCodeGoTaskStartSchema,
    res.data,
    'Failed to start OpenCode Go risk recheck task'
  )
}

export async function getOpenCodeGoTask(
  channelId: number,
  kind: 'refresh' | 'risk-recheck',
  taskId: string
): Promise<OpenCodeGoSystemTask> {
  const segment = kind === 'refresh' ? 'refresh-tasks' : 'risk-recheck-tasks'
  const res = await api.get(
    `/api/channel/${channelId}/opencode-go/${segment}/${encodeURIComponent(taskId)}`,
    channelActionConfig({ disableDuplicate: true })
  )
  return requireOpenCodeGoData(
    openCodeGoSystemTaskSchema,
    res.data,
    'Failed to load OpenCode Go task'
  )
}

export async function updateOpenCodeGoLifecyclePolicy(
  channelId: number,
  policy: Omit<OpenCodeGoLifecyclePolicy, 'automation_enabled'>,
  proofToken?: string
): Promise<OpenCodeGoLifecyclePolicy> {
  const res = await api.put(
    `/api/channel/${channelId}/opencode-go/lifecycle-policy`,
    policy,
    openCodeGoActionConfig(proofToken)
  )
  return requireOpenCodeGoData(
    openCodeGoLifecyclePolicySchema,
    res.data,
    'Failed to update OpenCode Go lifecycle policy'
  )
}

export async function enableOpenCodeGoChinaModels(
  channelId: number,
  workspaceUid: string,
  proofToken?: string
) {
  const res = await api.post(
    `/api/channel/${channelId}/opencode-go/workspaces/${encodeURIComponent(workspaceUid)}/china-models/enable`,
    undefined,
    openCodeGoActionConfig(proofToken)
  )
  return requireOpenCodeGoData(
    openCodeGoChinaModelsMutationSchema,
    res.data,
    'Failed to enable China-deployed models'
  )
}

export async function applyOpenCodeGoReferralReward(
  channelId: number,
  workspaceUid: string,
  proofToken?: string
) {
  const res = await api.post(
    `/api/channel/${channelId}/opencode-go/workspaces/${encodeURIComponent(workspaceUid)}/referral-rewards/apply`,
    undefined,
    openCodeGoActionConfig(proofToken)
  )
  return requireOpenCodeGoData(
    openCodeGoReferralMutationSchema,
    res.data,
    'Failed to apply OpenCode Go referral reward'
  )
}

export async function cancelOpenCodeGoSubscriptionRenewal(
  channelId: number,
  workspaceUid: string,
  proofToken?: string
) {
  const res = await api.post(
    `/api/channel/${channelId}/opencode-go/workspaces/${encodeURIComponent(workspaceUid)}/subscription/cancel-renewal`,
    { confirmation: 'CANCEL RENEWAL' },
    openCodeGoActionConfig(proofToken)
  )
  return requireOpenCodeGoData(
    openCodeGoCancellationMutationSchema,
    res.data,
    'Failed to cancel OpenCode Go subscription renewal'
  )
}
