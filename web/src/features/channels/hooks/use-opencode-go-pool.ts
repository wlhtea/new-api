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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { useSecureVerification } from '@/features/auth/secure-verification'

import {
  applyOpenCodeGoReferralReward,
  cancelOpenCodeGoSubscriptionRenewal,
  deleteOpenCodeGoIdentity,
  deleteOpenCodeGoNonMemberWorkspaces,
  deleteOpenCodeGoWorkspace,
  enableOpenCodeGoChinaModels,
  getOpenCodeGoPool,
  getOpenCodeGoTask,
  importOpenCodeGoIdentities,
  recheckAllOpenCodeGoWorkspaceRisks,
  recheckOpenCodeGoWorkspaceRisk,
  refreshAllOpenCodeGoIdentities,
  refreshOpenCodeGoIdentity,
  refreshOpenCodeGoWorkspace,
  replaceOpenCodeGoIdentityCookie,
  setOpenCodeGoIdentityEnabled,
  setOpenCodeGoWorkspaceEnabled,
  updateOpenCodeGoIdentityLabel,
  updateOpenCodeGoLifecyclePolicy,
} from '../api'
import { channelsQueryKeys } from '../lib/channel-actions'
import {
  getOpenCodeGoOrdinaryBusyKey,
  getOpenCodeGoTaskResults,
  openCodeGoPoolQueryKeys,
  type OpenCodeGoBulkResult,
  type OpenCodeGoOrdinaryAction,
} from '../lib/opencode-go-pool'
import type {
  OpenCodeGoLifecyclePolicy,
  OpenCodeGoPool,
  OpenCodeGoSystemTask,
} from '../lib/opencode-go-schemas'

type OpenCodeGoTaskKind = 'refresh' | 'risk-recheck'

type OrdinaryActionResult = {
  pool?: OpenCodeGoPool
  task?: OpenCodeGoSystemTask
  taskKind?: OpenCodeGoTaskKind
  successMessage: string
}

type ActiveTask = {
  id: string
  kind: OpenCodeGoTaskKind
}

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback
}

export function useOpenCodeGoPool(channelId: number, enabled: boolean) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [activeTask, setActiveTask] = useState<ActiveTask | null>(null)
  const [bulkResults, setBulkResults] = useState<OpenCodeGoBulkResult[]>([])
  const [sensitiveBusyKey, setSensitiveBusyKey] = useState<string | null>(null)
  const handledTaskIdRef = useRef('')
  const verification = useSecureVerification()

  const poolQuery = useQuery({
    queryKey: openCodeGoPoolQueryKeys.pool(channelId),
    queryFn: () => getOpenCodeGoPool(channelId),
    enabled: enabled && channelId > 0,
    staleTime: 10_000,
    retry: false,
  })

  const storePool = (pool: OpenCodeGoPool) => {
    queryClient.setQueryData(openCodeGoPoolQueryKeys.pool(channelId), pool)
    queryClient.invalidateQueries({ queryKey: channelsQueryKeys.lists() })
    queryClient.invalidateQueries({
      queryKey: channelsQueryKeys.detail(channelId),
    })
  }

  useEffect(() => {
    setActiveTask(null)
    setBulkResults([])
    setSensitiveBusyKey(null)
    handledTaskIdRef.current = ''
  }, [channelId])

  const ordinaryMutation = useMutation<
    OrdinaryActionResult,
    unknown,
    OpenCodeGoOrdinaryAction
  >({
    mutationFn: async (action) => {
      switch (action.kind) {
        case 'identity-label':
          return {
            pool: await updateOpenCodeGoIdentityLabel(
              channelId,
              action.identityUid,
              action.label
            ),
            successMessage: t('Account label updated'),
          }
        case 'identity-toggle':
          return {
            pool: await setOpenCodeGoIdentityEnabled(
              channelId,
              action.identityUid,
              action.enabled
            ),
            successMessage: action.enabled
              ? t('Account enabled')
              : t('Account disabled'),
          }
        case 'identity-refresh':
          return {
            pool: await refreshOpenCodeGoIdentity(
              channelId,
              action.identityUid
            ),
            successMessage: t('Account refreshed'),
          }
        case 'workspace-toggle':
          return {
            pool: await setOpenCodeGoWorkspaceEnabled(
              channelId,
              action.workspaceUid,
              action.enabled
            ),
            successMessage: action.enabled
              ? t('Workspace enabled')
              : t('Workspace disabled'),
          }
        case 'workspace-refresh':
          return {
            pool: await refreshOpenCodeGoWorkspace(
              channelId,
              action.workspaceUid
            ),
            successMessage: t('Workspace refreshed'),
          }
        case 'workspace-risk': {
          const result = await recheckOpenCodeGoWorkspaceRisk(
            channelId,
            action.workspaceUid
          )
          return {
            pool: result.pool,
            successMessage: result.result.blocked
              ? t('Risk recheck confirmed the block')
              : t('Risk recheck completed'),
          }
        }
        case 'refresh-all': {
          const result = await refreshAllOpenCodeGoIdentities(channelId)
          return {
            task: result.task,
            taskKind: 'refresh',
            successMessage: result.created
              ? t('Refresh task started')
              : t('Existing refresh task resumed'),
          }
        }
        case 'risk-all': {
          const result = await recheckAllOpenCodeGoWorkspaceRisks(channelId)
          return {
            task: result.task,
            taskKind: 'risk-recheck',
            successMessage: result.created
              ? t('Risk recheck task started')
              : t('Existing risk recheck task resumed'),
          }
        }
      }
    },
    onSuccess: (result) => {
      if (result.pool) storePool(result.pool)
      if (result.task && result.taskKind) {
        handledTaskIdRef.current = ''
        setActiveTask({ id: result.task.task_id, kind: result.taskKind })
      }
      toast.success(result.successMessage)
    },
    onError: (error) => {
      toast.error(errorMessage(error, t('OpenCode Go operation failed')))
    },
  })

  const taskQuery = useQuery({
    queryKey: openCodeGoPoolQueryKeys.task(
      channelId,
      activeTask?.kind || '',
      activeTask?.id || ''
    ),
    queryFn: () => {
      if (!activeTask) {
        throw new Error('OpenCode Go task context is unavailable')
      }
      return getOpenCodeGoTask(channelId, activeTask.kind, activeTask.id)
    },
    enabled: enabled && Boolean(activeTask),
    retry: false,
    refetchInterval: (query) => {
      const status = query.state.data?.status
      return status === 'succeeded' || status === 'failed' ? false : 1200
    },
  })

  useEffect(() => {
    const task = taskQuery.data
    if (!task || (task.status !== 'succeeded' && task.status !== 'failed')) {
      return
    }
    if (handledTaskIdRef.current === task.task_id) return
    handledTaskIdRef.current = task.task_id
    setBulkResults(getOpenCodeGoTaskResults(task))
    queryClient.invalidateQueries({
      queryKey: openCodeGoPoolQueryKeys.pool(channelId),
    })
    queryClient.invalidateQueries({ queryKey: channelsQueryKeys.lists() })
    queryClient.invalidateQueries({
      queryKey: channelsQueryKeys.detail(channelId),
    })
    if (task.status === 'failed') {
      toast.error(task.error || t('OpenCode Go task failed'))
    } else {
      toast.success(t('OpenCode Go task completed'))
    }
  }, [channelId, queryClient, t, taskQuery.data])

  const startSensitiveAction = async <T>(
    busyKey: string,
    title: string,
    description: string,
    action: (proofToken: string) => Promise<T>,
    onSuccess: (result: T) => void
  ) => {
    try {
      await verification.startVerification(
        async (proofToken) => {
          if (!proofToken) throw new Error(t('Security proof is required'))
          setSensitiveBusyKey(busyKey)
          try {
            const result = await action(proofToken)
            onSuccess(result)
            return result
          } finally {
            setSensitiveBusyKey(null)
          }
        },
        {
          scope: 'channel.opencode_go.pool.write',
          preferredMethod: 'passkey',
          title,
          description,
        }
      )
    } catch (error) {
      toast.error(errorMessage(error, t('OpenCode Go operation failed')))
    }
  }

  const importIdentities = async (input: {
    label: string
    authCookies: string
  }) => {
    await startSensitiveAction(
      'sensitive:import',
      t('Verify account import'),
      t('Confirm your identity before importing OpenCode Go accounts.'),
      (proofToken) => importOpenCodeGoIdentities(channelId, input, proofToken),
      (result) => {
        storePool(result.pool)
        setBulkResults(
          result.results.map((item) => ({
            key: t('Line {{index}}', { index: item.index }),
            status: item.status,
            error: item.error,
          }))
        )
        toast.success(t('Account import completed'))
      }
    )
  }

  const replaceIdentityCookie = async (
    identityUid: string,
    authCookie: string
  ) => {
    await startSensitiveAction(
      `identity:${identityUid}:cookie`,
      t('Verify Cookie replacement'),
      t('Confirm your identity before replacing this account Cookie.'),
      (proofToken) =>
        replaceOpenCodeGoIdentityCookie(
          channelId,
          identityUid,
          authCookie,
          proofToken
        ),
      (pool) => {
        storePool(pool)
        toast.success(t('Account Cookie replaced'))
      }
    )
  }

  const deleteIdentity = async (identityUid: string) => {
    await startSensitiveAction(
      `identity:${identityUid}:delete`,
      t('Verify account deletion'),
      t(
        'Confirm your identity before deleting this account and its workspaces.'
      ),
      (proofToken) =>
        deleteOpenCodeGoIdentity(channelId, identityUid, proofToken),
      (pool) => {
        storePool(pool)
        toast.success(t('Account deleted'))
      }
    )
  }

  const deleteWorkspace = async (workspaceUid: string) => {
    await startSensitiveAction(
      `workspace:${workspaceUid}:delete`,
      t('Verify workspace deletion'),
      t('Confirm your identity before deleting this workspace.'),
      (proofToken) =>
        deleteOpenCodeGoWorkspace(channelId, workspaceUid, proofToken),
      (pool) => {
        storePool(pool)
        toast.success(t('Workspace deleted'))
      }
    )
  }

  const deleteNonMembers = async () => {
    await startSensitiveAction(
      'sensitive:delete-non-members',
      t('Verify non-member cleanup'),
      t('Confirm your identity before deleting all non-member workspaces.'),
      (proofToken) =>
        deleteOpenCodeGoNonMemberWorkspaces(channelId, proofToken),
      (result) => {
        storePool(result.pool)
        toast.success(
          t('Deleted {{count}} non-member workspaces', {
            count: result.deleted_count,
          })
        )
      }
    )
  }

  const updatePolicy = async (
    policy: Omit<OpenCodeGoLifecyclePolicy, 'automation_enabled'>
  ) => {
    await startSensitiveAction(
      'sensitive:policy',
      t('Verify lifecycle policy update'),
      t('Confirm your identity before changing lifecycle automation.'),
      (proofToken) =>
        updateOpenCodeGoLifecyclePolicy(channelId, policy, proofToken),
      (updatedPolicy) => {
        queryClient.setQueryData<OpenCodeGoPool>(
          openCodeGoPoolQueryKeys.pool(channelId),
          (pool) => (pool ? { ...pool, lifecycle_policy: updatedPolicy } : pool)
        )
        queryClient.invalidateQueries({ queryKey: channelsQueryKeys.lists() })
        queryClient.invalidateQueries({
          queryKey: channelsQueryKeys.detail(channelId),
        })
        toast.success(t('Lifecycle policy updated'))
      }
    )
  }

  const enableChinaModels = async (workspaceUid: string) => {
    await startSensitiveAction(
      `workspace:${workspaceUid}:china-models`,
      t('Verify China-model change'),
      t('Confirm your identity before changing the upstream workspace.'),
      (proofToken) =>
        enableOpenCodeGoChinaModels(channelId, workspaceUid, proofToken),
      (result) => {
        storePool(result.pool)
        toast.success(t('China-deployed models enabled'))
      }
    )
  }

  const applyReferralReward = async (workspaceUid: string) => {
    await startSensitiveAction(
      `workspace:${workspaceUid}:referral`,
      t('Verify referral reward use'),
      t('Confirm your identity before applying one available referral reward.'),
      (proofToken) =>
        applyOpenCodeGoReferralReward(channelId, workspaceUid, proofToken),
      (result) => {
        storePool(result.pool)
        toast.success(
          t('Applied {{count}} referral rewards', {
            count: result.summary.applied,
          })
        )
      }
    )
  }

  const cancelRenewal = async (workspaceUid: string) => {
    await startSensitiveAction(
      `workspace:${workspaceUid}:cancel-renewal`,
      t('Verify renewal cancellation'),
      t('Confirm your identity before cancelling subscription renewal.'),
      (proofToken) =>
        cancelOpenCodeGoSubscriptionRenewal(
          channelId,
          workspaceUid,
          proofToken
        ),
      (result) => {
        storePool(result.pool)
        toast.success(t('Subscription renewal cancelled'))
      }
    )
  }

  return {
    poolQuery,
    ordinaryMutation,
    ordinaryBusyKey: getOpenCodeGoOrdinaryBusyKey(
      ordinaryMutation.isPending,
      ordinaryMutation.variables
    ),
    sensitiveBusyKey,
    activeTask,
    taskQuery,
    bulkResults,
    verification,
    runOrdinaryAction: ordinaryMutation.mutate,
    importIdentities,
    replaceIdentityCookie,
    deleteIdentity,
    deleteWorkspace,
    deleteNonMembers,
    updatePolicy,
    enableChinaModels,
    applyReferralReward,
    cancelRenewal,
  }
}
