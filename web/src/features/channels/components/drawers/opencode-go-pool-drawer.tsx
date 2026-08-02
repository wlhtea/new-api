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
import {
  AlertCircle,
  Filter,
  History,
  Loader2,
  RefreshCw,
  ShieldAlert,
  ShieldCheck,
  SlidersHorizontal,
  Trash2,
  UserPlus,
  UsersRound,
} from 'lucide-react'
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import {
  sideDrawerContentClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
import { Input } from '@/components/ui/input'
import {
  Progress,
  ProgressLabel,
  ProgressValue,
} from '@/components/ui/progress'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { SecureVerificationDialog } from '@/features/auth/secure-verification'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { useAuthStore } from '@/stores/auth-store'

import { useOpenCodeGoPool } from '../../hooks/use-opencode-go-pool'
import {
  getOpenCodeGoTaskProgress,
  isOpenCodeGoWorkspaceStale,
  listOpenCodeGoWorkspaceRows,
} from '../../lib/opencode-go-pool'
import type {
  OpenCodeGoIdentity,
  OpenCodeGoWorkspace,
} from '../../lib/opencode-go-schemas'
import type { Channel } from '../../types'
import {
  OpenCodeGoCookieDialog,
  OpenCodeGoImportDialog,
  OpenCodeGoLabelDialog,
} from '../opencode-go/opencode-go-account-dialogs'
import { OpenCodeGoIdentitySection } from '../opencode-go/opencode-go-identity-section'
import { OpenCodeGoOperations } from '../opencode-go/opencode-go-operations'
import {
  OpenCodeGoPolicyForm,
  type OpenCodeGoPolicyFormValues,
} from '../opencode-go/opencode-go-policy-form'
import type { OpenCodeGoWorkspaceSensitiveAction } from '../opencode-go/opencode-go-workspace-row'

type OpenCodeGoPoolDrawerProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  channel: Channel | null
  initialTab?: 'accounts' | 'policy' | 'operations'
}

type ConfirmationAction =
  | { kind: 'delete-identity'; identity: OpenCodeGoIdentity }
  | { kind: 'delete-non-members'; count: number }
  | {
      kind: OpenCodeGoWorkspaceSensitiveAction
      workspace: OpenCodeGoWorkspace
    }
  | { kind: 'policy-cancellation'; values: OpenCodeGoPolicyFormValues }

function poolErrorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback
}

export function OpenCodeGoPoolDrawer(props: OpenCodeGoPoolDrawerProps) {
  const { t, i18n } = useTranslation()
  const currentUser = useAuthStore((state) => state.auth.user)
  const channelId = props.channel?.id || 0
  const pool = useOpenCodeGoPool(channelId, props.open)
  const [tab, setTab] = useState(props.initialTab || 'accounts')
  const [workspaceFilter, setWorkspaceFilter] = useState<'all' | 'non-members'>(
    'all'
  )
  const [importOpen, setImportOpen] = useState(false)
  const [labelIdentity, setLabelIdentity] = useState<OpenCodeGoIdentity | null>(
    null
  )
  const [cookieIdentity, setCookieIdentity] =
    useState<OpenCodeGoIdentity | null>(null)
  const [confirmation, setConfirmation] = useState<ConfirmationAction | null>(
    null
  )
  const [confirmationText, setConfirmationText] = useState('')
  const [nowSeconds, setNowSeconds] = useState(() => Date.now() / 1000)

  const canOperate = hasPermission(
    currentUser,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.OPERATE
  )
  const canSensitiveWrite = hasPermission(
    currentUser,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.SENSITIVE_WRITE
  )

  useEffect(() => {
    if (props.open) {
      setTab(props.initialTab || 'accounts')
    }
  }, [channelId, props.initialTab, props.open])

  useEffect(() => {
    if (!props.open) return
    setNowSeconds(Date.now() / 1000)
    const timer = window.setInterval(
      () => setNowSeconds(Date.now() / 1000),
      30_000
    )
    return () => window.clearInterval(timer)
  }, [props.open])

  const view = pool.poolQuery.data
  const workspaceRows = useMemo(
    () => (view ? listOpenCodeGoWorkspaceRows(view) : []),
    [view]
  )
  const visibleWorkspaceUids = useMemo(() => {
    if (workspaceFilter === 'all') return undefined
    return new Set(
      workspaceRows
        .filter((row) => row.workspace.membership_status !== 'active')
        .map((row) => row.workspace.uid)
    )
  }, [workspaceFilter, workspaceRows])
  const nonMemberCount = workspaceRows.filter(
    (row) => row.workspace.membership_status !== 'active'
  ).length
  const staleCount = workspaceRows.filter((row) =>
    isOpenCodeGoWorkspaceStale(row.workspace)
  ).length
  const riskCount = workspaceRows.filter(
    (row) => row.workspace.effective_state === 'risk_blocked'
  ).length
  const taskProgress = getOpenCodeGoTaskProgress(pool.taskQuery.data)
  const taskStatus = pool.taskQuery.data?.status
  const taskInProgress =
    Boolean(pool.activeTask) &&
    taskStatus !== 'succeeded' &&
    taskStatus !== 'failed'
  const busyKey = pool.sensitiveBusyKey || pool.ordinaryBusyKey

  const handleOpenChange = (open: boolean) => {
    props.onOpenChange(open)
    if (!open) {
      pool.verification.cancel()
      setImportOpen(false)
      setLabelIdentity(null)
      setCookieIdentity(null)
      setConfirmation(null)
      setConfirmationText('')
      setWorkspaceFilter('all')
      setTab('accounts')
    }
  }

  const runWorkspaceSensitiveAction = (
    action: OpenCodeGoWorkspaceSensitiveAction,
    workspace: OpenCodeGoWorkspace
  ) => {
    setConfirmationText('')
    setConfirmation({ kind: action, workspace })
  }

  const submitPolicy = (values: OpenCodeGoPolicyFormValues) => {
    if (
      values.auto_cancel_subscription_renewal &&
      !view?.lifecycle_policy.auto_cancel_subscription_renewal
    ) {
      setConfirmationText('')
      setConfirmation({ kind: 'policy-cancellation', values })
      return
    }
    void pool.updatePolicy(values)
  }

  const performConfirmation = () => {
    const action = confirmation
    if (!action) return
    setConfirmation(null)
    setConfirmationText('')

    switch (action.kind) {
      case 'delete-identity':
        void pool.deleteIdentity(action.identity.uid)
        return
      case 'delete-non-members':
        void pool.deleteNonMembers()
        return
      case 'enable-china-models':
        void pool.enableChinaModels(action.workspace.uid)
        return
      case 'apply-referral-reward':
        void pool.applyReferralReward(action.workspace.uid)
        return
      case 'cancel-renewal':
        void pool.cancelRenewal(action.workspace.uid)
        return
      case 'delete-workspace':
        void pool.deleteWorkspace(action.workspace.uid)
        return
      case 'policy-cancellation':
        void pool.updatePolicy(action.values)
    }
  }

  let confirmationTitle = ''
  let confirmationDescription = ''
  let confirmationDestructive = false
  let confirmationRequiresText = false
  if (confirmation) {
    switch (confirmation.kind) {
      case 'delete-identity':
        confirmationTitle = t('Delete OpenCode Go account')
        confirmationDescription = t(
          'Delete account "{{name}}" and every workspace discovered under it?',
          {
            name:
              confirmation.identity.label ||
              confirmation.identity.email ||
              confirmation.identity.uid,
          }
        )
        confirmationDestructive = true
        break
      case 'delete-non-members':
        confirmationTitle = t('Delete non-member workspaces')
        confirmationDescription = t(
          'Permanently delete {{count}} non-member workspaces?',
          { count: confirmation.count }
        )
        confirmationDestructive = true
        break
      case 'enable-china-models':
        confirmationTitle = t('Enable China-deployed models')
        confirmationDescription = t(
          'Change the upstream setting for workspace "{{name}}"?',
          { name: confirmation.workspace.name || confirmation.workspace.uid }
        )
        break
      case 'apply-referral-reward':
        confirmationTitle = t('Apply one referral reward')
        confirmationDescription = t(
          'Consume one available reward and refresh quota for workspace "{{name}}"?',
          { name: confirmation.workspace.name || confirmation.workspace.uid }
        )
        break
      case 'cancel-renewal':
        confirmationTitle = t('Cancel subscription renewal')
        confirmationDescription = t(
          'Renewal will be cancelled while access remains active until the current period ends. Type CANCEL RENEWAL to continue.'
        )
        confirmationDestructive = true
        confirmationRequiresText = true
        break
      case 'delete-workspace':
        confirmationTitle = t('Delete workspace')
        confirmationDescription = t(
          'Permanently delete workspace "{{name}}" from this pool?',
          { name: confirmation.workspace.name || confirmation.workspace.uid }
        )
        confirmationDestructive = true
        break
      case 'policy-cancellation':
        confirmationTitle = t('Enable automatic renewal cancellation')
        confirmationDescription = t(
          'Eligible membership renewals may be cancelled automatically when global lifecycle automation is enabled.'
        )
        confirmationDestructive = true
    }
  }

  let accountListContent: ReactNode
  if (pool.poolQuery.isLoading) {
    accountListContent = (
      <div className='space-y-5' aria-label={t('Loading account pool')}>
        {['primary', 'secondary'].map((skeletonKey) => (
          <div key={skeletonKey} className='space-y-3 border-b pb-5'>
            <Skeleton className='h-6 w-56' />
            <Skeleton className='h-4 w-80 max-w-full' />
            <div className='grid gap-3 md:grid-cols-3'>
              <Skeleton className='h-44' />
              <Skeleton className='h-44' />
              <Skeleton className='h-44' />
            </div>
          </div>
        ))}
      </div>
    )
  } else if (pool.poolQuery.isError) {
    accountListContent = (
      <Empty className='min-h-72 border'>
        <EmptyHeader>
          <EmptyMedia variant='icon'>
            <AlertCircle />
          </EmptyMedia>
          <EmptyTitle>{t('Failed to load account pool')}</EmptyTitle>
          <EmptyDescription>
            {poolErrorMessage(
              pool.poolQuery.error,
              t('OpenCode Go account pool request failed')
            )}
          </EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button
            type='button'
            variant='outline'
            onClick={() => pool.poolQuery.refetch()}
          >
            <RefreshCw className='size-4' />
            {t('Retry')}
          </Button>
        </EmptyContent>
      </Empty>
    )
  } else if (!view || view.identities.length === 0) {
    accountListContent = (
      <Empty className='min-h-72 border'>
        <EmptyHeader>
          <EmptyMedia variant='icon'>
            <UsersRound />
          </EmptyMedia>
          <EmptyTitle>{t('No OpenCode Go accounts')}</EmptyTitle>
          <EmptyDescription>
            {t('Import an auth Cookie to discover its workspaces')}
          </EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button
            type='button'
            disabled={!canSensitiveWrite || !view?.crypto_secret_configured}
            onClick={() => setImportOpen(true)}
          >
            <UserPlus className='size-4' />
            {t('Import accounts')}
          </Button>
        </EmptyContent>
      </Empty>
    )
  } else if (workspaceFilter === 'non-members' && nonMemberCount === 0) {
    accountListContent = (
      <Empty className='min-h-72 border'>
        <EmptyHeader>
          <EmptyMedia variant='icon'>
            <Filter />
          </EmptyMedia>
          <EmptyTitle>{t('No non-member workspaces')}</EmptyTitle>
          <EmptyDescription>
            {t('All discovered workspaces currently have active membership')}
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    )
  } else {
    accountListContent = view.identities.map((identity) => (
      <OpenCodeGoIdentitySection
        key={identity.uid}
        identity={identity}
        visibleWorkspaceUids={visibleWorkspaceUids}
        nowSeconds={nowSeconds}
        locale={i18n.language}
        canOperate={canOperate}
        canSensitiveWrite={canSensitiveWrite}
        busyKey={busyKey}
        onEditLabel={setLabelIdentity}
        onReplaceCookie={setCookieIdentity}
        onRefreshIdentity={(identityUid) =>
          pool.runOrdinaryAction({
            kind: 'identity-refresh',
            identityUid,
          })
        }
        onToggleIdentity={(identityUid, enabled) =>
          pool.runOrdinaryAction({
            kind: 'identity-toggle',
            identityUid,
            enabled,
          })
        }
        onDeleteIdentity={(identityValue) =>
          setConfirmation({
            kind: 'delete-identity',
            identity: identityValue,
          })
        }
        onRefreshWorkspace={(workspaceUid) =>
          pool.runOrdinaryAction({
            kind: 'workspace-refresh',
            workspaceUid,
          })
        }
        onRiskRecheckWorkspace={(workspaceUid) =>
          pool.runOrdinaryAction({
            kind: 'workspace-risk',
            workspaceUid,
          })
        }
        onToggleWorkspace={(workspaceUid, enabled) =>
          pool.runOrdinaryAction({
            kind: 'workspace-toggle',
            workspaceUid,
            enabled,
          })
        }
        onWorkspaceSensitiveAction={runWorkspaceSensitiveAction}
      />
    ))
  }

  let policyContent: ReactNode = null
  if (pool.poolQuery.isLoading) {
    policyContent = (
      <div
        className='mx-auto w-full max-w-3xl space-y-4'
        aria-label={t('Loading lifecycle policy')}
      >
        <Skeleton className='h-7 w-48' />
        <Skeleton className='h-20 w-full' />
        <Skeleton className='h-72 w-full' />
      </div>
    )
  } else if (pool.poolQuery.isError) {
    policyContent = (
      <Empty className='mx-auto min-h-72 max-w-3xl border'>
        <EmptyHeader>
          <EmptyMedia variant='icon'>
            <AlertCircle />
          </EmptyMedia>
          <EmptyTitle>{t('Failed to load lifecycle policy')}</EmptyTitle>
          <EmptyDescription>
            {poolErrorMessage(
              pool.poolQuery.error,
              t('OpenCode Go account pool request failed')
            )}
          </EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button
            type='button'
            variant='outline'
            onClick={() => pool.poolQuery.refetch()}
          >
            <RefreshCw className='size-4' />
            {t('Retry')}
          </Button>
        </EmptyContent>
      </Empty>
    )
  } else if (view) {
    policyContent = (
      <OpenCodeGoPolicyForm
        policy={view.lifecycle_policy}
        disabled={!canSensitiveWrite}
        isSubmitting={pool.sensitiveBusyKey === 'sensitive:policy'}
        onSubmit={submitPolicy}
      />
    )
  }

  return (
    <>
      <Sheet open={props.open} onOpenChange={handleOpenChange}>
        <SheetContent
          className={sideDrawerContentClassName(
            'max-w-none sm:!max-w-[min(96vw,1400px)]'
          )}
        >
          <SheetHeader className={sideDrawerHeaderClassName()}>
            <div className='flex min-w-0 flex-col gap-3 xl:flex-row xl:items-start xl:justify-between'>
              <div className='min-w-0'>
                <SheetTitle className='flex min-w-0 items-center gap-2'>
                  <UsersRound className='text-primary size-5 shrink-0' />
                  <span className='truncate'>
                    {t('OpenCode Go account pool')}
                  </span>
                </SheetTitle>
                <SheetDescription className='mt-1 truncate'>
                  {props.channel?.name || '-'} · #{channelId}
                </SheetDescription>
              </div>
              <div className='flex min-w-0 flex-wrap gap-2 pr-8 text-xs'>
                <Badge variant='outline'>
                  {t('{{count}} accounts', {
                    count: view?.identities.length || 0,
                  })}
                </Badge>
                <Badge variant='outline'>
                  {t('{{count}} workspaces', { count: workspaceRows.length })}
                </Badge>
                <Badge
                  variant={
                    view?.eligible_workspace_count ? 'default' : 'warning'
                  }
                >
                  {t('{{count}} eligible', {
                    count: view?.eligible_workspace_count || 0,
                  })}
                </Badge>
                {riskCount > 0 && (
                  <Badge variant='destructive'>
                    {t('{{count}} risk blocked', { count: riskCount })}
                  </Badge>
                )}
                {staleCount > 0 && (
                  <Badge variant='warning'>
                    {t('{{count}} stale', { count: staleCount })}
                  </Badge>
                )}
              </div>
            </div>
          </SheetHeader>

          <Tabs
            value={tab}
            onValueChange={setTab}
            className='min-h-0 flex-1 gap-0 overflow-hidden'
          >
            <div className='border-border/70 flex min-w-0 items-center border-b px-4 sm:px-6'>
              <TabsList variant='line' className='max-w-full overflow-x-auto'>
                <TabsTrigger value='accounts'>
                  <UsersRound />
                  {t('Accounts')}
                </TabsTrigger>
                <TabsTrigger value='policy'>
                  <SlidersHorizontal />
                  {t('Lifecycle policy')}
                </TabsTrigger>
                <TabsTrigger value='operations'>
                  <History />
                  {t('Operations')}
                  {pool.bulkResults.length > 0 && (
                    <Badge variant='secondary'>{pool.bulkResults.length}</Badge>
                  )}
                </TabsTrigger>
              </TabsList>
            </div>

            <TabsContent
              value='accounts'
              className='min-h-0 overflow-y-auto overscroll-contain'
            >
              <div className='border-border/70 bg-background sticky top-0 z-10 flex min-w-0 flex-col gap-3 border-b px-4 py-3 sm:flex-row sm:items-center sm:justify-between sm:px-6'>
                <div className='flex min-w-0 flex-wrap items-center gap-2'>
                  <Button
                    type='button'
                    size='sm'
                    disabled={
                      !canSensitiveWrite || !view?.crypto_secret_configured
                    }
                    onClick={() => setImportOpen(true)}
                  >
                    <UserPlus className='size-4' />
                    {t('Import accounts')}
                  </Button>
                  <Button
                    type='button'
                    size='sm'
                    variant='outline'
                    disabled={
                      !canOperate ||
                      !view?.identities.length ||
                      busyKey === 'bulk:refresh'
                    }
                    onClick={() =>
                      pool.runOrdinaryAction({ kind: 'refresh-all' })
                    }
                  >
                    {busyKey === 'bulk:refresh' ? (
                      <Loader2 className='size-4 animate-spin' />
                    ) : (
                      <RefreshCw className='size-4' />
                    )}
                    {t('Refresh all')}
                  </Button>
                  <Button
                    type='button'
                    size='sm'
                    variant='outline'
                    disabled={
                      !canOperate || riskCount === 0 || busyKey === 'bulk:risk'
                    }
                    onClick={() => pool.runOrdinaryAction({ kind: 'risk-all' })}
                  >
                    {busyKey === 'bulk:risk' ? (
                      <Loader2 className='size-4 animate-spin' />
                    ) : (
                      <ShieldCheck className='size-4' />
                    )}
                    {t('Recheck all risks')}
                  </Button>
                  <Button
                    type='button'
                    size='sm'
                    variant='outline'
                    className='text-destructive hover:text-destructive'
                    disabled={!canSensitiveWrite || nonMemberCount === 0}
                    onClick={() =>
                      setConfirmation({
                        kind: 'delete-non-members',
                        count: nonMemberCount,
                      })
                    }
                  >
                    <Trash2 className='size-4' />
                    {t('Delete non-members')}
                  </Button>
                </div>

                <Select
                  items={[
                    { value: 'all', label: t('All workspaces') },
                    { value: 'non-members', label: t('Non-members') },
                  ]}
                  value={workspaceFilter}
                  onValueChange={(value) =>
                    setWorkspaceFilter(value as 'all' | 'non-members')
                  }
                >
                  <SelectTrigger size='sm' className='w-full sm:w-44'>
                    <Filter className='size-4' />
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent alignItemWithTrigger={false}>
                    <SelectGroup>
                      <SelectItem value='all'>{t('All workspaces')}</SelectItem>
                      <SelectItem value='non-members'>
                        {t('Non-members')}
                      </SelectItem>
                    </SelectGroup>
                  </SelectContent>
                </Select>
              </div>

              <div className='mx-auto flex w-full max-w-[86rem] flex-col gap-4 px-4 py-4 sm:px-6'>
                {taskInProgress && pool.activeTask && taskProgress && (
                  <Alert>
                    <Loader2 className='size-4 animate-spin' />
                    <AlertTitle>
                      {pool.activeTask.kind === 'refresh'
                        ? t('Refreshing account pool')
                        : t('Rechecking workspace risks')}
                    </AlertTitle>
                    <AlertDescription>
                      <Progress
                        value={taskProgress.progress}
                        className='mt-2 gap-2'
                      >
                        <ProgressLabel>
                          {t('{{processed}} of {{total}}', {
                            processed: taskProgress.processed,
                            total: taskProgress.total,
                          })}
                        </ProgressLabel>
                        <ProgressValue>
                          {() => `${taskProgress.progress.toFixed(0)}%`}
                        </ProgressValue>
                      </Progress>
                    </AlertDescription>
                  </Alert>
                )}

                {view && !view.crypto_secret_configured && (
                  <Alert variant='destructive'>
                    <ShieldAlert className='size-4' />
                    <AlertTitle>
                      {t('Credential encryption is unavailable')}
                    </AlertTitle>
                    <AlertDescription>
                      {t(
                        'Configure CRYPTO_SECRET before importing or refreshing accounts'
                      )}
                    </AlertDescription>
                  </Alert>
                )}

                {view &&
                  view.identities.length > 0 &&
                  view.eligible_workspace_count === 0 && (
                    <Alert>
                      <AlertCircle className='size-4' />
                      <AlertTitle>{t('No eligible workspace')}</AlertTitle>
                      <AlertDescription>
                        {t(
                          'The channel remains unavailable until a workspace recovers'
                        )}
                      </AlertDescription>
                    </Alert>
                  )}

                {accountListContent}
              </div>
            </TabsContent>

            <TabsContent
              value='policy'
              className='min-h-0 overflow-y-auto overscroll-contain px-4 py-5 sm:px-6'
            >
              {policyContent}
            </TabsContent>

            <TabsContent
              value='operations'
              className='min-h-0 overflow-y-auto overscroll-contain px-4 py-5 sm:px-6'
            >
              <OpenCodeGoOperations
                operations={view?.operations || []}
                bulkResults={pool.bulkResults}
                locale={i18n.language}
              />
            </TabsContent>
          </Tabs>
        </SheetContent>
      </Sheet>

      <OpenCodeGoImportDialog
        open={importOpen}
        onOpenChange={setImportOpen}
        isSubmitting={pool.sensitiveBusyKey === 'sensitive:import'}
        onSubmit={(values) => void pool.importIdentities(values)}
      />
      <OpenCodeGoLabelDialog
        identity={labelIdentity}
        open={Boolean(labelIdentity)}
        onOpenChange={(open) => !open && setLabelIdentity(null)}
        isSubmitting={pool.ordinaryMutation.isPending}
        onSubmit={(identity, label) => {
          setLabelIdentity(null)
          pool.runOrdinaryAction({
            kind: 'identity-label',
            identityUid: identity.uid,
            label,
          })
        }}
      />
      <OpenCodeGoCookieDialog
        identity={cookieIdentity}
        open={Boolean(cookieIdentity)}
        onOpenChange={(open) => !open && setCookieIdentity(null)}
        isSubmitting={Boolean(pool.sensitiveBusyKey?.endsWith(':cookie'))}
        onSubmit={(identity, authCookie) => {
          setCookieIdentity(null)
          void pool.replaceIdentityCookie(identity.uid, authCookie)
        }}
      />

      <ConfirmDialog
        open={Boolean(confirmation)}
        onOpenChange={(open) => {
          if (!open) {
            setConfirmation(null)
            setConfirmationText('')
          }
        }}
        title={confirmationTitle}
        desc={confirmationDescription}
        destructive={confirmationDestructive}
        confirmText={t('Confirm')}
        disabled={
          confirmationRequiresText && confirmationText !== 'CANCEL RENEWAL'
        }
        handleConfirm={performConfirmation}
      >
        {confirmationRequiresText && (
          <Input
            value={confirmationText}
            onChange={(event) => setConfirmationText(event.target.value)}
            placeholder='CANCEL RENEWAL'
            aria-label={t('Renewal cancellation confirmation')}
            autoComplete='off'
          />
        )}
      </ConfirmDialog>

      <SecureVerificationDialog
        open={pool.verification.open}
        onOpenChange={(open) => {
          if (!open) pool.verification.cancel()
        }}
        methods={pool.verification.methods}
        state={pool.verification.state}
        onVerify={async (method, code) => {
          try {
            await pool.verification.executeVerification(method, code)
          } catch {
            // The verification hook already displays the request error.
          }
        }}
        onCancel={pool.verification.cancel}
        onCodeChange={pool.verification.setCode}
        onMethodChange={pool.verification.switchMethod}
      />
    </>
  )
}
