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
import { Clock3 } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import {
  Progress,
  ProgressLabel,
  ProgressValue,
} from '@/components/ui/progress'
import { toIntlLocale } from '@/i18n/languages'
import { cn } from '@/lib/utils'

import {
  formatOpenCodeGoResetCountdown,
  openCodeGoQuotaLayoutClasses,
} from '../../lib/opencode-go-pool'
import type { OpenCodeGoQuotaWindow } from '../../lib/opencode-go-schemas'

const QUOTA_WINDOW_LABELS: Record<OpenCodeGoQuotaWindow['kind'], string> = {
  rolling: 'Rolling window',
  weekly: 'Weekly window',
  monthly: 'Monthly window',
}

type OpenCodeGoQuotaWindowProps = {
  kind: OpenCodeGoQuotaWindow['kind']
  window?: OpenCodeGoQuotaWindow
  stale: boolean
  nowSeconds: number
  locale?: string
}

function formatTimestamp(timestamp: number, locale?: string): string {
  if (timestamp <= 0) return '-'
  return new Intl.DateTimeFormat(toIntlLocale(locale), {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(timestamp * 1000))
}

export function OpenCodeGoQuotaWindowView(props: OpenCodeGoQuotaWindowProps) {
  const { t } = useTranslation()

  if (!props.window) {
    return (
      <div
        className={cn(
          openCodeGoQuotaLayoutClasses.window,
          'border-dashed justify-center'
        )}
        data-state='missing'
      >
        <p className='font-medium'>{t(QUOTA_WINDOW_LABELS[props.kind])}</p>
        <p className='text-muted-foreground text-xs'>
          {t('No authoritative quota snapshot')}
        </p>
      </div>
    )
  }

  const used = Math.min(100, Math.max(0, props.window.used_percent))
  const remaining = Math.min(100, Math.max(0, props.window.remaining_percent))

  return (
    <div
      className={cn(
        openCodeGoQuotaLayoutClasses.window,
        props.stale && 'border-warning/50 bg-warning/5'
      )}
      data-state={props.stale ? 'stale' : 'complete'}
    >
      <div className='flex min-w-0 items-center justify-between gap-2'>
        <p className='truncate font-medium'>
          {t(QUOTA_WINDOW_LABELS[props.kind])}
        </p>
        <Badge variant={props.stale ? 'warning' : 'outline'}>
          {props.stale ? t('Stale') : t('Console %')}
        </Badge>
      </div>

      <Progress value={used} className='gap-2' aria-label={t('Used quota')}>
        <ProgressLabel>{t('Used')}</ProgressLabel>
        <ProgressValue>{() => `${used.toFixed(1)}%`}</ProgressValue>
      </Progress>

      <div className='grid grid-cols-2 gap-3 text-xs tabular-nums'>
        <div className='min-w-0'>
          <p className='text-muted-foreground'>{t('Remaining')}</p>
          <p className='mt-0.5 text-base font-semibold'>
            {remaining.toFixed(1)}%
          </p>
        </div>
        <div className='min-w-0 text-right'>
          <p className='text-muted-foreground'>{t('Calculated balance')}</p>
          <p className='mt-0.5 text-base font-semibold'>
            ${props.window.calculated_remaining_usd.toFixed(2)}
          </p>
        </div>
      </div>

      <div className='text-muted-foreground mt-auto space-y-1 border-t pt-2 text-xs'>
        <div className='flex min-w-0 items-center justify-between gap-2'>
          <span>{t('Calculated limit')}</span>
          <span className='shrink-0 tabular-nums'>
            ${props.window.calculated_limit_usd.toFixed(2)}
          </span>
        </div>
        <div className='flex min-w-0 items-center justify-between gap-2'>
          <span>{t('Calculated used')}</span>
          <span className='shrink-0 tabular-nums'>
            ${props.window.calculated_used_usd.toFixed(2)}
          </span>
        </div>
        <div className='flex min-w-0 items-start justify-between gap-2'>
          <span className='flex shrink-0 items-center gap-1'>
            <Clock3 className='size-3' aria-hidden='true' />
            {t('Resets')}
          </span>
          <span className='min-w-0 text-right tabular-nums'>
            {formatOpenCodeGoResetCountdown(
              props.window.reset_at,
              props.nowSeconds,
              toIntlLocale(props.locale)
            )}
          </span>
        </div>
        <p
          className='truncate text-right'
          title={formatTimestamp(props.window.reset_at, props.locale)}
        >
          {formatTimestamp(props.window.reset_at, props.locale)}
        </p>
        <div className='flex min-w-0 items-start justify-between gap-2 border-t pt-1'>
          <span className='shrink-0'>{t('Snapshot time')}</span>
          <time
            className='min-w-0 truncate text-right tabular-nums'
            dateTime={new Date(props.window.fetched_at * 1000).toISOString()}
            title={formatTimestamp(props.window.fetched_at, props.locale)}
          >
            {formatTimestamp(props.window.fetched_at, props.locale)}
          </time>
        </div>
      </div>
    </div>
  )
}
