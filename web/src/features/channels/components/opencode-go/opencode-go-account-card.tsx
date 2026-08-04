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
import { ChevronRight, UserRound } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import {
  Progress,
  ProgressLabel,
  ProgressValue,
} from '@/components/ui/progress'
import { cn } from '@/lib/utils'

import {
  getOpenCodeGoQuotaWindow,
  isOpenCodeGoWorkspaceStale,
} from '../../lib/opencode-go-pool'
import type {
  OpenCodeGoIdentity,
  OpenCodeGoQuotaWindow,
  OpenCodeGoWorkspace,
} from '../../lib/opencode-go-schemas'
import {
  OPENCODE_GO_IDENTITY_STATUS_LABELS,
  OPENCODE_GO_QUOTA_WINDOW_LABELS,
  OPENCODE_GO_WORKSPACE_STATE_LABELS,
  openCodeGoIdentityBadgeVariant,
  openCodeGoWorkspaceBadgeVariant,
} from './opencode-go-status'

type OpenCodeGoAccountCardProps = {
  identity: OpenCodeGoIdentity
  workspaces: OpenCodeGoWorkspace[]
  onOpenDetails: () => void
}

function quotaIndicatorClass(
  window: OpenCodeGoQuotaWindow | undefined,
  stale: boolean
): string {
  if (!window || stale) {
    return '[&_[data-slot=progress-indicator]]:bg-muted-foreground/40'
  }
  if (window.used_percent >= 95) {
    return '[&_[data-slot=progress-indicator]]:bg-destructive'
  }
  if (window.used_percent >= 80) {
    return '[&_[data-slot=progress-indicator]]:bg-warning'
  }
  return ''
}

export function OpenCodeGoAccountCard(props: OpenCodeGoAccountCardProps) {
  const { t } = useTranslation()
  const accountName =
    props.identity.label || props.identity.email || props.identity.uid

  return (
    <article
      className='border-border/70 bg-card hover:border-primary/40 hover:bg-muted/15 relative h-full min-w-0 overflow-hidden rounded-md border transition-colors'
      data-account-card
    >
      <div className='flex h-full w-full min-w-0 flex-col p-4'>
        <div className='flex min-w-0 items-start gap-3'>
          <span className='bg-muted text-muted-foreground flex size-9 shrink-0 items-center justify-center rounded-md'>
            <UserRound className='size-4' aria-hidden='true' />
          </span>
          <div className='min-w-0 flex-1'>
            <div className='flex min-w-0 flex-wrap items-center gap-2'>
              <h3 className='max-w-full truncate text-sm font-semibold'>
                {accountName}
              </h3>
              <Badge
                variant={openCodeGoIdentityBadgeVariant(props.identity.status)}
              >
                {t(
                  OPENCODE_GO_IDENTITY_STATUS_LABELS[props.identity.status] ||
                    props.identity.status
                )}
              </Badge>
            </div>
            <p className='text-muted-foreground mt-1 truncate text-xs'>
              {props.identity.email || '-'}
            </p>
          </div>
        </div>

        <div className='mt-4 w-full min-w-0 flex-1 space-y-4'>
          {props.workspaces.length === 0 ? (
            <p className='text-muted-foreground border-border/60 border-t py-5 text-sm'>
              {t('No workspaces discovered for this account')}
            </p>
          ) : (
            props.workspaces.map((workspace) => {
              const stale = isOpenCodeGoWorkspaceStale(workspace)
              return (
                <section
                  key={workspace.uid}
                  className='border-border/60 min-w-0 border-t pt-3 first:border-t-0 first:pt-0'
                  data-account-workspace-summary
                >
                  <div className='flex min-w-0 items-center justify-between gap-2'>
                    <div className='min-w-0'>
                      <h4 className='truncate text-xs font-semibold'>
                        {workspace.name || workspace.email || workspace.uid}
                      </h4>
                      <p className='text-muted-foreground mt-0.5 text-[11px]'>
                        {workspace.membership_status === 'active'
                          ? t('Member')
                          : t('Non-member')}
                      </p>
                    </div>
                    <Badge
                      variant={openCodeGoWorkspaceBadgeVariant(
                        workspace.effective_state
                      )}
                    >
                      {t(
                        OPENCODE_GO_WORKSPACE_STATE_LABELS[
                          workspace.effective_state
                        ] || workspace.effective_state
                      )}
                    </Badge>
                  </div>

                  <div className='mt-3 space-y-2'>
                    {(['rolling', 'weekly', 'monthly'] as const).map((kind) => {
                      const window = getOpenCodeGoQuotaWindow(workspace, kind)
                      const used = window
                        ? Math.min(100, Math.max(0, window.used_percent))
                        : 0
                      let quotaState = 'complete'
                      if (!window) {
                        quotaState = 'missing'
                      } else if (stale) {
                        quotaState = 'stale'
                      }
                      return (
                        <Progress
                          key={kind}
                          value={used}
                          className={cn(
                            'gap-1 [&_[data-slot=progress-track]]:h-1.5',
                            quotaIndicatorClass(window, stale)
                          )}
                          data-quota-state={quotaState}
                          aria-label={t('Used quota')}
                        >
                          <ProgressLabel className='text-muted-foreground text-[11px] font-medium'>
                            {t(OPENCODE_GO_QUOTA_WINDOW_LABELS[kind])}
                          </ProgressLabel>
                          <ProgressValue className='text-foreground text-[11px]'>
                            {() => (window ? `${used.toFixed(1)}%` : '-')}
                          </ProgressValue>
                        </Progress>
                      )
                    })}
                  </div>
                </section>
              )
            })
          )}
        </div>

        <div className='border-border/60 text-muted-foreground mt-4 flex w-full items-center justify-between gap-3 border-t pt-3 text-xs'>
          <span>
            {t('{{count}} workspaces', {
              count: props.identity.workspaces.length,
            })}
          </span>
          <span className='text-foreground flex items-center gap-1 font-medium'>
            {t('Details')}
            <ChevronRight className='size-3.5' aria-hidden='true' />
          </span>
        </div>
      </div>
      <button
        type='button'
        className='focus-visible:ring-ring absolute inset-0 z-10 rounded-md outline-none focus-visible:ring-2 focus-visible:ring-inset'
        onClick={props.onOpenDetails}
        aria-label={`${t('Details')}: ${accountName}`}
      >
        <span className='sr-only'>{t('Details')}</span>
      </button>
    </article>
  )
}
