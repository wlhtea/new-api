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
  Ban,
  CheckCircle2,
  ChevronDown,
  Gift,
  Globe2,
  Loader2,
  MoreHorizontal,
  Power,
  PowerOff,
  RefreshCw,
  ShieldCheck,
  Trash2,
  XCircle,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'

import {
  getOpenCodeGoQuotaWindow,
  isOpenCodeGoWorkspaceRecovered,
  isOpenCodeGoWorkspaceStale,
  openCodeGoQuotaLayoutClasses,
} from '../../lib/opencode-go-pool'
import type { OpenCodeGoWorkspace } from '../../lib/opencode-go-schemas'
import { OpenCodeGoQuotaWindowView } from './opencode-go-quota-window'

const WORKSPACE_STATE_LABELS: Record<string, string> = {
  pending: 'Pending',
  eligible: 'Eligible',
  manual_disabled: 'Manual disabled',
  stale: 'Stale snapshot',
  auth_error: 'Authentication error',
  key_error: 'Credential error',
  membership_expired: 'Membership inactive',
  quota_exhausted: 'Quota exhausted',
  risk_blocked: 'Risk blocked',
  cooldown: 'Cooling down',
}

const MODEL_STATE_LABELS: Record<string, string> = {
  available: 'Available',
  region_blocked: 'Region blocked',
  rpm_cooldown: 'RPM cooldown',
  transient_cooldown: 'Transient cooldown',
  disabled: 'Disabled',
}

export type OpenCodeGoWorkspaceSensitiveAction =
  | 'enable-china-models'
  | 'apply-referral-reward'
  | 'cancel-renewal'
  | 'delete-workspace'

type OpenCodeGoWorkspaceRowProps = {
  workspace: OpenCodeGoWorkspace
  nowSeconds: number
  locale?: string
  canOperate: boolean
  canSensitiveWrite: boolean
  busyKey: string | null
  onRefresh: (workspaceUid: string) => void
  onRiskRecheck: (workspaceUid: string) => void
  onToggle: (workspaceUid: string, enabled: boolean) => void
  onSensitiveAction: (
    action: OpenCodeGoWorkspaceSensitiveAction,
    workspace: OpenCodeGoWorkspace
  ) => void
}

function formatTimestamp(timestamp: number, locale?: string): string {
  if (timestamp <= 0) return '-'
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(timestamp * 1000))
}

function workspaceBadgeVariant(
  state: string
): 'default' | 'secondary' | 'destructive' | 'warning' | 'outline' {
  if (state === 'eligible') return 'default'
  if (state === 'risk_blocked' || state === 'auth_error') return 'destructive'
  if (state === 'quota_exhausted' || state === 'cooldown') return 'warning'
  if (state === 'manual_disabled') return 'secondary'
  return 'outline'
}

function modelBadgeVariant(
  state: string
): 'secondary' | 'destructive' | 'warning' | 'outline' {
  if (state === 'available') return 'outline'
  if (state === 'disabled' || state === 'region_blocked') return 'destructive'
  if (state.includes('cooldown')) return 'warning'
  return 'secondary'
}

function ErrorBand(props: { message: string }) {
  if (!props.message) return null
  return (
    <div className='border-destructive/40 bg-destructive/5 text-destructive border-l-2 px-3 py-2 text-xs break-words'>
      {props.message}
    </div>
  )
}

export function OpenCodeGoWorkspaceRow(props: OpenCodeGoWorkspaceRowProps) {
  const { t } = useTranslation()
  const workspace = props.workspace
  const stale = isOpenCodeGoWorkspaceStale(workspace)
  const recovered = isOpenCodeGoWorkspaceRecovered(workspace)
  const refreshBusy = props.busyKey === `workspace:${workspace.uid}:refresh`
  const toggleBusy = props.busyKey === `workspace:${workspace.uid}:toggle`
  const riskBusy = props.busyKey === `workspace:${workspace.uid}:risk`
  let toggleIcon = <Power className='text-success size-4' />
  if (toggleBusy) {
    toggleIcon = <Loader2 className='size-4 animate-spin' />
  } else if (workspace.manual_enabled) {
    toggleIcon = <PowerOff className='text-destructive size-4' />
  }

  let chinaModelsIcon = (
    <ChevronDown
      className='text-muted-foreground size-3.5'
      aria-hidden='true'
    />
  )
  let chinaModelsLabel = t('Unknown')
  if (workspace.china_models_enabled === true) {
    chinaModelsIcon = (
      <CheckCircle2 className='text-success size-3.5' aria-hidden='true' />
    )
    chinaModelsLabel = t('Enabled')
  } else if (workspace.china_models_enabled === false) {
    chinaModelsIcon = (
      <XCircle className='text-destructive size-3.5' aria-hidden='true' />
    )
    chinaModelsLabel = t('Disabled')
  }

  return (
    <section
      className='border-border/60 min-w-0 border-t py-4 first:border-t-0'
      data-workspace-state={workspace.effective_state}
    >
      <div className='flex min-w-0 flex-col gap-3 lg:flex-row lg:items-start lg:justify-between'>
        <div className='min-w-0 space-y-2'>
          <div className='flex min-w-0 flex-wrap items-center gap-2'>
            <h4 className='max-w-full truncate text-sm font-semibold'>
              {workspace.name || workspace.email || workspace.uid}
            </h4>
            <Badge variant={workspaceBadgeVariant(workspace.effective_state)}>
              {t(
                WORKSPACE_STATE_LABELS[workspace.effective_state] ||
                  workspace.effective_state
              )}
            </Badge>
            {recovered && (
              <Badge
                variant='outline'
                className='text-success border-success/40'
              >
                <CheckCircle2 className='size-3' aria-hidden='true' />
                {t('Recovered')}
              </Badge>
            )}
          </div>
          <div className='text-muted-foreground flex min-w-0 flex-wrap gap-x-4 gap-y-1 text-xs'>
            <span className='max-w-full truncate'>
              {workspace.email || '-'}
            </span>
            <span className='font-mono'>{workspace.uid}</span>
            <span>
              {workspace.membership_status === 'active'
                ? t('Member')
                : t('Non-member')}
            </span>
            <span>
              {workspace.has_api_key
                ? t('API key ready')
                : t('API key missing')}
            </span>
          </div>
        </div>

        <div className='flex shrink-0 items-center gap-1 self-end lg:self-start'>
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  type='button'
                  size='icon-sm'
                  variant='ghost'
                  disabled={!props.canOperate || refreshBusy}
                  onClick={() => props.onRefresh(workspace.uid)}
                  aria-label={t('Refresh workspace')}
                />
              }
            >
              {refreshBusy ? (
                <Loader2 className='size-4 animate-spin' />
              ) : (
                <RefreshCw className='size-4' />
              )}
            </TooltipTrigger>
            <TooltipContent>{t('Refresh workspace')}</TooltipContent>
          </Tooltip>

          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  type='button'
                  size='icon-sm'
                  variant='ghost'
                  disabled={!props.canOperate || riskBusy}
                  onClick={() => props.onRiskRecheck(workspace.uid)}
                  aria-label={t('Recheck risk')}
                />
              }
            >
              {riskBusy ? (
                <Loader2 className='size-4 animate-spin' />
              ) : (
                <ShieldCheck className='size-4' />
              )}
            </TooltipTrigger>
            <TooltipContent>{t('Recheck risk')}</TooltipContent>
          </Tooltip>

          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  type='button'
                  size='icon-sm'
                  variant='ghost'
                  disabled={!props.canOperate || toggleBusy}
                  onClick={() =>
                    props.onToggle(workspace.uid, !workspace.manual_enabled)
                  }
                  aria-label={
                    workspace.manual_enabled ? t('Disable') : t('Enable')
                  }
                />
              }
            >
              {toggleIcon}
            </TooltipTrigger>
            <TooltipContent>
              {workspace.manual_enabled ? t('Disable') : t('Enable')}
            </TooltipContent>
          </Tooltip>

          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  type='button'
                  size='icon-sm'
                  variant='ghost'
                  aria-label={t('Workspace actions')}
                />
              }
            >
              <MoreHorizontal className='size-4' />
            </DropdownMenuTrigger>
            <DropdownMenuContent align='end' className='w-64'>
              <DropdownMenuItem
                disabled={
                  !props.canSensitiveWrite ||
                  workspace.china_models_enabled === true
                }
                onClick={() =>
                  props.onSensitiveAction('enable-china-models', workspace)
                }
              >
                {t('Enable China-deployed models')}
                <DropdownMenuShortcut>
                  <Globe2 className='size-4' />
                </DropdownMenuShortcut>
              </DropdownMenuItem>
              <DropdownMenuItem
                disabled={
                  !props.canSensitiveWrite ||
                  workspace.available_referral_rewards <= 0
                }
                onClick={() =>
                  props.onSensitiveAction('apply-referral-reward', workspace)
                }
              >
                {t('Apply one referral reward')}
                <DropdownMenuShortcut>
                  <Gift className='size-4' />
                </DropdownMenuShortcut>
              </DropdownMenuItem>
              <DropdownMenuItem
                disabled={
                  !props.canSensitiveWrite ||
                  workspace.membership_status !== 'active' ||
                  workspace.renewal_cancelled_at > 0
                }
                onClick={() =>
                  props.onSensitiveAction('cancel-renewal', workspace)
                }
              >
                {t('Cancel subscription renewal')}
                <DropdownMenuShortcut>
                  <Ban className='size-4' />
                </DropdownMenuShortcut>
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                disabled={!props.canSensitiveWrite}
                className='text-destructive focus:text-destructive'
                onClick={() =>
                  props.onSensitiveAction('delete-workspace', workspace)
                }
              >
                {t('Delete workspace')}
                <DropdownMenuShortcut>
                  <Trash2 className='size-4' />
                </DropdownMenuShortcut>
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      {(workspace.state_reason ||
        stale ||
        workspace.cooldown_until > props.nowSeconds) && (
        <div
          className={cn(
            'mt-3 flex min-w-0 flex-wrap items-center gap-x-4 gap-y-1 border-l-2 px-3 py-2 text-xs',
            stale
              ? 'border-warning/60 bg-warning/5 text-warning'
              : 'border-border bg-muted/30 text-muted-foreground'
          )}
        >
          {stale && <span>{t('Authoritative quota snapshot is stale')}</span>}
          {workspace.state_reason && (
            <span className='min-w-0 break-words'>
              {workspace.state_reason}
            </span>
          )}
          {workspace.cooldown_until > props.nowSeconds && (
            <span>
              {t('Cooldown until {{time}}', {
                time: formatTimestamp(workspace.cooldown_until, props.locale),
              })}
            </span>
          )}
        </div>
      )}

      <div className={cn(openCodeGoQuotaLayoutClasses.grid, 'mt-4')}>
        {(['rolling', 'weekly', 'monthly'] as const).map((kind) => (
          <OpenCodeGoQuotaWindowView
            key={kind}
            kind={kind}
            window={getOpenCodeGoQuotaWindow(workspace, kind)}
            stale={stale}
            nowSeconds={props.nowSeconds}
            locale={props.locale}
          />
        ))}
      </div>

      <div className='mt-4 grid min-w-0 gap-x-6 gap-y-2 text-xs sm:grid-cols-2 xl:grid-cols-4'>
        <div>
          <span className='text-muted-foreground'>{t('Last synced')}</span>
          <p
            className='mt-0.5 truncate'
            title={formatTimestamp(workspace.last_synced_at, props.locale)}
          >
            {formatTimestamp(workspace.last_synced_at, props.locale)}
          </p>
        </div>
        <div>
          <span className='text-muted-foreground'>
            {t('Subscription ends')}
          </span>
          <p className='mt-0.5 truncate'>
            {formatTimestamp(workspace.subscription_ends_at, props.locale)}
          </p>
        </div>
        <div>
          <span className='text-muted-foreground'>{t('China models')}</span>
          <p className='mt-0.5 flex items-center gap-1'>
            {chinaModelsIcon}
            {chinaModelsLabel}
          </p>
        </div>
        <div>
          <span className='text-muted-foreground'>{t('Referral rewards')}</span>
          <p className='mt-0.5 tabular-nums'>
            {t('{{available}} available / {{used}} used', {
              available: workspace.available_referral_rewards,
              used: workspace.used_referral_rewards,
            })}
          </p>
        </div>
      </div>

      <ErrorBand
        message={
          workspace.last_error ||
          workspace.quota_error ||
          workspace.china_models_error ||
          workspace.renewal_cancel_error
        }
      />

      <details className='border-border/60 mt-4 border-t pt-3'>
        <summary className='focus-visible:ring-ring flex cursor-pointer list-none items-center justify-between gap-3 rounded-sm text-xs font-medium focus-visible:ring-2 focus-visible:outline-none'>
          <span>
            {t('Models ({{count}})', { count: workspace.models.length })}
          </span>
          <ChevronDown
            className='text-muted-foreground size-4'
            aria-hidden='true'
          />
        </summary>
        <div className='mt-3 flex min-w-0 flex-wrap gap-2'>
          {workspace.models.length === 0 ? (
            <span className='text-muted-foreground text-xs'>
              {t('No discovered models')}
            </span>
          ) : (
            workspace.models.map((model) => (
              <Badge
                key={model.model}
                variant={modelBadgeVariant(model.state)}
                title={model.last_error || model.last_error_code || model.model}
                className='max-w-full'
              >
                <span className='truncate'>{model.model}</span>
                <span className='text-[10px] opacity-70'>
                  {t(MODEL_STATE_LABELS[model.state] || model.state)}
                </span>
              </Badge>
            ))
          )}
        </div>
      </details>
    </section>
  )
}
