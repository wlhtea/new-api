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
  Cookie,
  Loader2,
  MoreHorizontal,
  Pencil,
  Power,
  PowerOff,
  RefreshCw,
  Trash2,
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

import type {
  OpenCodeGoIdentity,
  OpenCodeGoWorkspace,
} from '../../lib/opencode-go-schemas'
import {
  OpenCodeGoWorkspaceRow,
  type OpenCodeGoWorkspaceSensitiveAction,
} from './opencode-go-workspace-row'

const IDENTITY_STATUS_LABELS: Record<string, string> = {
  pending: 'Pending',
  active: 'Active',
  stale: 'Stale',
  auth_error: 'Authentication error',
  manual_disabled: 'Manual disabled',
}

type OpenCodeGoIdentitySectionProps = {
  identity: OpenCodeGoIdentity
  visibleWorkspaceUids?: Set<string>
  nowSeconds: number
  locale?: string
  canOperate: boolean
  canSensitiveWrite: boolean
  busyKey: string | null
  onEditLabel: (identity: OpenCodeGoIdentity) => void
  onReplaceCookie: (identity: OpenCodeGoIdentity) => void
  onRefreshIdentity: (identityUid: string) => void
  onToggleIdentity: (identityUid: string, enabled: boolean) => void
  onDeleteIdentity: (identity: OpenCodeGoIdentity) => void
  onRefreshWorkspace: (workspaceUid: string) => void
  onRiskRecheckWorkspace: (workspaceUid: string) => void
  onToggleWorkspace: (workspaceUid: string, enabled: boolean) => void
  onWorkspaceSensitiveAction: (
    action: OpenCodeGoWorkspaceSensitiveAction,
    workspace: OpenCodeGoWorkspace
  ) => void
}

function identityBadgeVariant(
  status: string
): 'default' | 'secondary' | 'destructive' | 'warning' | 'outline' {
  if (status === 'active') return 'default'
  if (status === 'auth_error') return 'destructive'
  if (status === 'stale') return 'warning'
  if (status === 'manual_disabled') return 'secondary'
  return 'outline'
}

export function OpenCodeGoIdentitySection(
  props: OpenCodeGoIdentitySectionProps
) {
  const { t } = useTranslation()
  const identity = props.identity
  const workspaces = identity.workspaces.filter(
    (workspace) =>
      !props.visibleWorkspaceUids ||
      props.visibleWorkspaceUids.has(workspace.uid)
  )
  const refreshBusy = props.busyKey === `identity:${identity.uid}:refresh`
  const toggleBusy = props.busyKey === `identity:${identity.uid}:toggle`
  let toggleIcon = <Power className='text-success size-4' />
  if (toggleBusy) {
    toggleIcon = <Loader2 className='size-4 animate-spin' />
  } else if (identity.manual_enabled) {
    toggleIcon = <PowerOff className='text-destructive size-4' />
  }

  if (workspaces.length === 0 && props.visibleWorkspaceUids) return null

  return (
    <section className='border-border/70 min-w-0 border-b py-5 last:border-b-0'>
      <div className='flex min-w-0 flex-col gap-3 sm:flex-row sm:items-start sm:justify-between'>
        <div className='min-w-0 space-y-2'>
          <div className='flex min-w-0 flex-wrap items-center gap-2'>
            <h3 className='max-w-full truncate text-sm font-semibold'>
              {identity.label || identity.email || identity.uid}
            </h3>
            <Badge variant={identityBadgeVariant(identity.status)}>
              {t(IDENTITY_STATUS_LABELS[identity.status] || identity.status)}
            </Badge>
            <Badge
              variant={identity.has_auth_cookie ? 'outline' : 'destructive'}
            >
              <Cookie className='size-3' aria-hidden='true' />
              {identity.has_auth_cookie
                ? t('Cookie stored')
                : t('Cookie missing')}
            </Badge>
          </div>
          <div className='text-muted-foreground flex min-w-0 flex-wrap gap-x-4 gap-y-1 text-xs'>
            <span className='max-w-full truncate'>{identity.email || '-'}</span>
            <span className='font-mono'>{identity.uid}</span>
            <span>
              {t('{{count}} workspaces', { count: identity.workspaces.length })}
            </span>
          </div>
          {identity.last_error && (
            <p className='text-destructive max-w-3xl text-xs break-words'>
              {identity.last_error}
            </p>
          )}
        </div>

        <div className='flex shrink-0 items-center gap-1 self-end sm:self-start'>
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  type='button'
                  size='icon-sm'
                  variant='ghost'
                  disabled={!props.canOperate || refreshBusy}
                  onClick={() => props.onRefreshIdentity(identity.uid)}
                  aria-label={t('Refresh account')}
                />
              }
            >
              {refreshBusy ? (
                <Loader2 className='size-4 animate-spin' />
              ) : (
                <RefreshCw className='size-4' />
              )}
            </TooltipTrigger>
            <TooltipContent>{t('Refresh account')}</TooltipContent>
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
                    props.onToggleIdentity(
                      identity.uid,
                      !identity.manual_enabled
                    )
                  }
                  aria-label={
                    identity.manual_enabled ? t('Disable') : t('Enable')
                  }
                />
              }
            >
              {toggleIcon}
            </TooltipTrigger>
            <TooltipContent>
              {identity.manual_enabled ? t('Disable') : t('Enable')}
            </TooltipContent>
          </Tooltip>

          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  type='button'
                  size='icon-sm'
                  variant='ghost'
                  aria-label={t('Account actions')}
                />
              }
            >
              <MoreHorizontal className='size-4' />
            </DropdownMenuTrigger>
            <DropdownMenuContent align='end' className='w-52'>
              <DropdownMenuItem
                disabled={!props.canOperate}
                onClick={() => props.onEditLabel(identity)}
              >
                {t('Edit label')}
                <DropdownMenuShortcut>
                  <Pencil className='size-4' />
                </DropdownMenuShortcut>
              </DropdownMenuItem>
              <DropdownMenuItem
                disabled={!props.canSensitiveWrite}
                onClick={() => props.onReplaceCookie(identity)}
              >
                {t('Replace Cookie')}
                <DropdownMenuShortcut>
                  <Cookie className='size-4' />
                </DropdownMenuShortcut>
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                disabled={!props.canSensitiveWrite}
                className='text-destructive focus:text-destructive'
                onClick={() => props.onDeleteIdentity(identity)}
              >
                {t('Delete account')}
                <DropdownMenuShortcut>
                  <Trash2 className='size-4' />
                </DropdownMenuShortcut>
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      <div className='mt-4 min-w-0 pl-0 sm:pl-3'>
        {workspaces.length === 0 ? (
          <p className='text-muted-foreground border-border/60 border-t py-5 text-sm'>
            {t('No workspaces discovered for this account')}
          </p>
        ) : (
          workspaces.map((workspace) => (
            <OpenCodeGoWorkspaceRow
              key={workspace.uid}
              workspace={workspace}
              nowSeconds={props.nowSeconds}
              locale={props.locale}
              canOperate={props.canOperate}
              canSensitiveWrite={props.canSensitiveWrite}
              busyKey={props.busyKey}
              onRefresh={props.onRefreshWorkspace}
              onRiskRecheck={props.onRiskRecheckWorkspace}
              onToggle={props.onToggleWorkspace}
              onSensitiveAction={props.onWorkspaceSensitiveAction}
            />
          ))
        )}
      </div>
    </section>
  )
}
