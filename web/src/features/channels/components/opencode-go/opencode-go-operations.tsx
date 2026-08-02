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
import { AlertCircle, CheckCircle2, History } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'

import {
  isOpenCodeGoBulkResultFailure,
  type OpenCodeGoBulkResult,
} from '../../lib/opencode-go-pool'
import type { OpenCodeGoOperation } from '../../lib/opencode-go-schemas'

const BULK_STATUS_LABELS: Record<string, string> = {
  imported: 'Imported',
  duplicate: 'Duplicate',
  error: 'Failed',
  refreshed: 'Refreshed',
  recovered: 'Recovered',
  blocked: 'Blocked',
  not_recovered: 'Not recovered',
  failed: 'Failed',
}

const OPERATION_ACTION_LABELS: Record<string, string> = {
  risk_recheck: 'Risk recheck',
  enable_china_models: 'Enable China-deployed models',
  apply_referral_reward: 'Apply referral reward',
  cancel_subscription_renewal: 'Cancel subscription renewal',
}

const OPERATION_STATUS_LABELS: Record<string, string> = {
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
}

const OPERATION_SOURCE_LABELS: Record<string, string> = {
  manual: 'Manual',
  system: 'Automation',
}

type OpenCodeGoOperationsProps = {
  operations: OpenCodeGoOperation[]
  bulkResults: OpenCodeGoBulkResult[]
  locale?: string
}

function formatTimestamp(timestamp: number, locale?: string): string {
  if (timestamp <= 0) return '-'
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'medium',
  }).format(new Date(timestamp * 1000))
}

export function OpenCodeGoOperations(props: OpenCodeGoOperationsProps) {
  const { t } = useTranslation()
  const failedBulkResults = props.bulkResults.filter(
    isOpenCodeGoBulkResultFailure
  )

  return (
    <div className='mx-auto flex w-full max-w-5xl flex-col gap-6'>
      {props.bulkResults.length > 0 && (
        <section className='space-y-3'>
          <div className='flex items-center justify-between gap-3'>
            <h3 className='text-sm font-semibold'>
              {t('Latest batch results')}
            </h3>
            <Badge
              variant={failedBulkResults.length > 0 ? 'warning' : 'outline'}
            >
              {t('{{success}} succeeded / {{failed}} failed', {
                success: props.bulkResults.length - failedBulkResults.length,
                failed: failedBulkResults.length,
              })}
            </Badge>
          </div>
          {failedBulkResults.length > 0 && (
            <Alert>
              <AlertCircle className='size-4' />
              <AlertTitle>
                {t('Batch completed with partial failures')}
              </AlertTitle>
              <AlertDescription>
                {t('Each account result is listed below')}
              </AlertDescription>
            </Alert>
          )}
          <div className='border-border/70 divide-border/60 divide-y border-y'>
            {props.bulkResults.map((result) => {
              const failed = isOpenCodeGoBulkResultFailure(result)
              return (
                <div
                  key={result.key}
                  className='grid min-w-0 gap-1 py-2 text-xs sm:grid-cols-[minmax(0,1fr)_8rem_minmax(0,2fr)] sm:items-start sm:gap-3'
                >
                  <span className='min-w-0 truncate font-mono'>
                    {result.key}
                  </span>
                  <Badge variant={failed ? 'destructive' : 'outline'}>
                    {failed ? (
                      <AlertCircle className='size-3' aria-hidden='true' />
                    ) : (
                      <CheckCircle2 className='size-3' aria-hidden='true' />
                    )}
                    {t(BULK_STATUS_LABELS[result.status] || result.status)}
                  </Badge>
                  <span className='text-muted-foreground min-w-0 break-words'>
                    {result.error || '-'}
                  </span>
                </div>
              )
            })}
          </div>
        </section>
      )}

      <section className='space-y-3'>
        <div className='flex items-center justify-between gap-3'>
          <h3 className='text-sm font-semibold'>{t('Operation history')}</h3>
          <Badge variant='outline'>{props.operations.length}</Badge>
        </div>

        {props.operations.length === 0 ? (
          <Empty className='min-h-52 border'>
            <EmptyHeader>
              <EmptyMedia variant='icon'>
                <History />
              </EmptyMedia>
              <EmptyTitle>{t('No operation history')}</EmptyTitle>
              <EmptyDescription>
                {t('Lifecycle and risk operations will appear here')}
              </EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <div className='border-border/70 divide-border/60 divide-y border-y'>
            {props.operations.map((operation) => (
              <article
                key={operation.uid}
                className='grid min-w-0 gap-2 py-3 text-xs md:grid-cols-[10rem_minmax(0,1fr)_8rem_8rem] md:gap-4'
              >
                <time className='text-muted-foreground tabular-nums'>
                  {formatTimestamp(operation.started_at, props.locale)}
                </time>
                <div className='min-w-0'>
                  <p className='truncate font-medium'>
                    {t(
                      OPERATION_ACTION_LABELS[operation.action] ||
                        operation.action
                    )}
                  </p>
                  <p className='text-muted-foreground mt-0.5 truncate font-mono'>
                    {operation.workspace_uid || '-'}
                  </p>
                  {(operation.result || operation.error) && (
                    <p
                      className={
                        operation.error
                          ? 'text-destructive mt-1 break-words'
                          : 'text-muted-foreground mt-1 break-words'
                      }
                    >
                      {operation.error || operation.result}
                    </p>
                  )}
                </div>
                <Badge
                  variant={
                    operation.status === 'failed' ? 'destructive' : 'outline'
                  }
                >
                  {t(
                    OPERATION_STATUS_LABELS[operation.status] ||
                      operation.status
                  )}
                </Badge>
                <span className='text-muted-foreground truncate'>
                  {t(
                    OPERATION_SOURCE_LABELS[operation.source] ||
                      operation.source
                  )}
                </span>
              </article>
            ))}
          </div>
        )}
      </section>
    </div>
  )
}
