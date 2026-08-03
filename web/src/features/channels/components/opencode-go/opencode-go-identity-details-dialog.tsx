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
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

import type {
  OpenCodeGoIdentity,
  OpenCodeGoWorkspace,
} from '../../lib/opencode-go-schemas'
import {
  OPENCODE_GO_IDENTITY_STATUS_LABELS,
  openCodeGoIdentityBadgeVariant,
} from './opencode-go-status'
import {
  OpenCodeGoWorkspaceRow,
  type OpenCodeGoWorkspaceSensitiveAction,
} from './opencode-go-workspace-row'

type OpenCodeGoIdentityDetailsDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  identity: OpenCodeGoIdentity
  workspaces: OpenCodeGoWorkspace[]
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

export function OpenCodeGoIdentityDetailsDialog(
  props: OpenCodeGoIdentityDetailsDialogProps
) {
  const { t } = useTranslation()
  const identity = props.identity
  const accountName = identity.label || identity.email || identity.uid
  const refreshBusy = props.busyKey === `identity:${identity.uid}:refresh`
  const toggleBusy = props.busyKey === `identity:${identity.uid}:toggle`
  let toggleIcon = <Power className='size-4' />
  if (toggleBusy) {
    toggleIcon = <Loader2 className='size-4 animate-spin' />
  } else if (identity.manual_enabled) {
    toggleIcon = <PowerOff className='size-4' />
  }

  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent
        className='max-h-[min(92vh,900px)] grid-rows-[auto_minmax(0,1fr)] gap-0 overflow-hidden p-0 sm:max-w-[min(94vw,1100px)]'
        data-account-details
      >
        <DialogHeader className='border-border/70 border-b px-5 py-4 pr-12'>
          <div className='flex min-w-0 flex-col gap-3 lg:flex-row lg:items-start lg:justify-between'>
            <div className='min-w-0'>
              <div className='flex min-w-0 flex-wrap items-center gap-2'>
                <DialogTitle className='max-w-full truncate'>
                  {accountName}
                </DialogTitle>
                <Badge
                  variant={openCodeGoIdentityBadgeVariant(identity.status)}
                >
                  {t(
                    OPENCODE_GO_IDENTITY_STATUS_LABELS[identity.status] ||
                      identity.status
                  )}
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
              <DialogDescription className='mt-1 min-w-0'>
                <span className='block truncate'>{identity.email || '-'}</span>
                <span className='mt-1 block truncate font-mono text-xs'>
                  {identity.uid}
                </span>
              </DialogDescription>
            </div>

            <div className='flex shrink-0 flex-wrap items-center gap-2'>
              <Button
                type='button'
                size='sm'
                variant='outline'
                disabled={!props.canOperate || refreshBusy}
                onClick={() => props.onRefreshIdentity(identity.uid)}
              >
                {refreshBusy ? (
                  <Loader2 className='size-4 animate-spin' />
                ) : (
                  <RefreshCw className='size-4' />
                )}
                {t('Refresh account')}
              </Button>
              <Button
                type='button'
                size='sm'
                variant='outline'
                disabled={!props.canOperate || toggleBusy}
                onClick={() =>
                  props.onToggleIdentity(identity.uid, !identity.manual_enabled)
                }
              >
                {toggleIcon}
                {identity.manual_enabled ? t('Disable') : t('Enable')}
              </Button>
              <Button
                type='button'
                size='sm'
                variant='outline'
                disabled={!props.canOperate}
                onClick={() => props.onEditLabel(identity)}
              >
                <Pencil className='size-4' />
                {t('Edit label')}
              </Button>
              <Button
                type='button'
                size='sm'
                variant='outline'
                disabled={!props.canSensitiveWrite}
                onClick={() => props.onReplaceCookie(identity)}
              >
                <Cookie className='size-4' />
                {t('Replace Cookie')}
              </Button>
              <Button
                type='button'
                size='sm'
                variant='ghost'
                className='text-destructive hover:text-destructive'
                disabled={!props.canSensitiveWrite}
                onClick={() => props.onDeleteIdentity(identity)}
              >
                <Trash2 className='size-4' />
                {t('Delete account')}
              </Button>
            </div>
          </div>

          {identity.last_error && (
            <p className='border-destructive/40 bg-destructive/5 text-destructive mt-1 border-l-2 px-3 py-2 text-xs break-words'>
              {identity.last_error}
            </p>
          )}
        </DialogHeader>

        <div className='min-h-0 overflow-y-auto overscroll-contain px-5 py-2'>
          {props.workspaces.length === 0 ? (
            <p className='text-muted-foreground py-8 text-center text-sm'>
              {t('No workspaces discovered for this account')}
            </p>
          ) : (
            props.workspaces.map((workspace) => (
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
      </DialogContent>
    </Dialog>
  )
}
