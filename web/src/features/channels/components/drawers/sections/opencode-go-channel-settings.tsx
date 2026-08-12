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
  ArrowRight,
  Gauge,
  Network,
  ShieldAlert,
  ShieldCheck,
  Waypoints,
} from 'lucide-react'
import { useRef } from 'react'
import { useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'

import type { ChannelFormInput } from '../../../lib/channel-form'
import { inferOpenCodeGoIdentityProxyPolicyOnEnable } from '../../../lib/opencode-go-identity-proxy'

type OpenCodeGoChannelSettingsProps = {
  disabled?: boolean
  lifecyclePolicyReadOnly?: boolean
  onOpenLifecyclePolicy?: () => void
}

export function OpenCodeGoChannelSettings(
  props: OpenCodeGoChannelSettingsProps
) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormInput>()
  const lifecyclePolicyDisabled =
    props.disabled || props.lifecyclePolicyReadOnly
  const genericFailoverEnabled = form.watch(
    'opencode_go_generic_failover_enabled'
  )
  const identityProxyEnabled = form.watch('opencode_go_identity_proxy_enabled')
  const rawSettings = form.watch('settings')
  const identityProxyInference = useRef({
    rawSettings,
    applied: false,
  })
  if (identityProxyInference.current.rawSettings !== rawSettings) {
    identityProxyInference.current = { rawSettings, applied: false }
  }

  return (
    <fieldset
      disabled={props.disabled}
      className='border-border/60 flex min-w-0 flex-col gap-5 border-t pt-5 disabled:opacity-60'
    >
      <div className='flex items-center gap-2'>
        <Network
          className='text-muted-foreground h-3.5 w-3.5'
          aria-hidden='true'
        />
        <h4 className='text-muted-foreground text-xs font-medium tracking-wide uppercase'>
          {t('Identity proxy routing')}
        </h4>
      </div>

      <div className='divide-border/60 border-border/60 divide-y border-y'>
        <FormField
          control={form.control}
          name='opencode_go_identity_proxy_enabled'
          render={({ field }) => (
            <FormItem className='flex min-h-16 items-center justify-between gap-4 py-3'>
              <div className='min-w-0'>
                <FormLabel>{t('Use identity-scoped proxy sessions')}</FormLabel>
                <FormDescription>
                  {t(
                    'Uses the channel Proxy Address and gives each imported account its own rotating sticky session'
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  checked={field.value === true}
                  onCheckedChange={(checked) => {
                    field.onChange(checked)
                    if (!checked || identityProxyInference.current.applied) {
                      return
                    }

                    const inferred = inferOpenCodeGoIdentityProxyPolicyOnEnable(
                      form.getValues('proxy'),
                      form.getValues('settings')
                    )
                    if (inferred.country) {
                      form.setValue(
                        'opencode_go_identity_proxy_country',
                        inferred.country,
                        { shouldDirty: true, shouldValidate: true }
                      )
                    }
                    if (inferred.rotateMinutes) {
                      form.setValue(
                        'opencode_go_identity_proxy_rotate_minutes',
                        inferred.rotateMinutes,
                        { shouldDirty: true, shouldValidate: true }
                      )
                    }
                    if (inferred.country || inferred.rotateMinutes) {
                      identityProxyInference.current.applied = true
                    }
                  }}
                  aria-label={t('Use identity-scoped proxy sessions')}
                />
              </FormControl>
            </FormItem>
          )}
        />

        <div className='grid gap-4 py-3 sm:grid-cols-2'>
          <FormField
            control={form.control}
            name='opencode_go_identity_proxy_country'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Proxy country')}</FormLabel>
                <FormControl>
                  <Input
                    disabled={!identityProxyEnabled}
                    maxLength={2}
                    autoCapitalize='characters'
                    value={field.value || ''}
                    onChange={(event) =>
                      field.onChange(
                        event.target.value.replaceAll(/[a-z]/g, (char) =>
                          char.toUpperCase()
                        )
                      )
                    }
                    aria-label={t('Proxy country')}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='opencode_go_identity_proxy_rotate_minutes'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Rotation interval (minutes)')}</FormLabel>
                <FormControl>
                  <Input
                    disabled={!identityProxyEnabled}
                    type='number'
                    min={1}
                    max={180}
                    value={field.value ?? ''}
                    onChange={(event) =>
                      field.onChange(event.target.valueAsNumber)
                    }
                    aria-label={t('Rotation interval (minutes)')}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </div>
      </div>

      <div className='flex items-center gap-2'>
        <Waypoints
          className='text-muted-foreground h-3.5 w-3.5'
          aria-hidden='true'
        />
        <h4 className='text-muted-foreground text-xs font-medium tracking-wide uppercase'>
          {t('OpenCode Go protocol routing')}
        </h4>
      </div>

      <Alert>
        <AlertDescription>
          {t(
            'Account credentials are managed in the OpenCode Go account pool after this channel is created.'
          )}
        </AlertDescription>
      </Alert>

      <div className='grid gap-4 sm:grid-cols-[14rem_minmax(0,1fr)]'>
        <FormField
          control={form.control}
          name='opencode_go_default_protocol'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Fallback protocol')}</FormLabel>
              <Select
                items={[
                  { value: 'built-in', label: t('Built-in routing') },
                  { value: 'chat', label: t('Chat Completions') },
                  { value: 'messages', label: t('Claude Messages') },
                  { value: 'responses', label: t('OpenAI Responses') },
                ]}
                value={field.value || 'built-in'}
                onValueChange={(value) =>
                  field.onChange(value === 'built-in' ? '' : value)
                }
              >
                <FormControl>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                </FormControl>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    <SelectItem value='built-in'>
                      {t('Built-in routing')}
                    </SelectItem>
                    <SelectItem value='chat'>
                      {t('Chat Completions')}
                    </SelectItem>
                    <SelectItem value='messages'>
                      {t('Claude Messages')}
                    </SelectItem>
                    <SelectItem value='responses'>
                      {t('OpenAI Responses')}
                    </SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
              <FormMessage />
            </FormItem>
          )}
        />

        <FormField
          control={form.control}
          name='opencode_go_model_protocols'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Model protocol overrides')}</FormLabel>
              <FormControl>
                <Textarea
                  className='min-h-24 resize-y font-mono text-xs'
                  placeholder='{"model-*":"messages"}'
                  spellCheck={false}
                  {...field}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
      </div>

      <div className='flex items-center gap-2'>
        <ShieldAlert
          className='text-muted-foreground h-3.5 w-3.5'
          aria-hidden='true'
        />
        <h4 className='text-muted-foreground text-xs font-medium tracking-wide uppercase'>
          {t('Failure-only workspace affinity')}
        </h4>
      </div>

      <div className='divide-border/60 border-border/60 divide-y border-y'>
        <FormField
          control={form.control}
          name='opencode_go_generic_failover_enabled'
          render={({ field }) => (
            <FormItem className='flex min-h-16 items-center justify-between gap-4 py-3'>
              <div className='min-w-0'>
                <FormLabel>{t('Generic upstream failover')}</FormLabel>
                <FormDescription>
                  {t(
                    'Switches only after repeated 500, 502, 503, or 504 responses'
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  checked={field.value === true}
                  onCheckedChange={field.onChange}
                  aria-label={t('Generic upstream failover')}
                />
              </FormControl>
            </FormItem>
          )}
        />

        <div className='grid gap-4 py-3 sm:grid-cols-3'>
          <FormField
            control={form.control}
            name='opencode_go_generic_failover_threshold'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Failure threshold')}</FormLabel>
                <FormControl>
                  <Input
                    disabled={!genericFailoverEnabled}
                    type='number'
                    min={2}
                    max={10}
                    value={field.value}
                    onChange={(event) =>
                      field.onChange(event.target.valueAsNumber)
                    }
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='opencode_go_generic_failover_window_seconds'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Failure window (seconds)')}</FormLabel>
                <FormControl>
                  <Input
                    disabled={!genericFailoverEnabled}
                    type='number'
                    min={1}
                    max={300}
                    value={field.value}
                    onChange={(event) =>
                      field.onChange(event.target.valueAsNumber)
                    }
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='opencode_go_generic_failover_lease_seconds'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Backup lease (seconds)')}</FormLabel>
                <FormControl>
                  <Input
                    disabled={!genericFailoverEnabled}
                    type='number'
                    min={1}
                    max={86400}
                    value={field.value}
                    onChange={(event) =>
                      field.onChange(event.target.valueAsNumber)
                    }
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </div>
      </div>

      <div className='flex items-center gap-2'>
        <Gauge
          className='text-muted-foreground h-3.5 w-3.5'
          aria-hidden='true'
        />
        <h4 className='text-muted-foreground text-xs font-medium tracking-wide uppercase'>
          {t('Load-aware scheduling')}
        </h4>
      </div>

      <div className='divide-border/60 border-border/60 divide-y border-y'>
        <FormField
          control={form.control}
          name='opencode_go_load_aware_enabled'
          render={({ field }) => (
            <FormItem className='flex min-h-16 items-center justify-between gap-4 py-3'>
              <div className='min-w-0'>
                <FormLabel>{t('Balance traffic by account load')}</FormLabel>
                <FormDescription>
                  {t(
                    'Routes steady-state requests toward less-busy accounts and keeps an explicit failover lease in charge on errors'
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  checked={field.value === true}
                  onCheckedChange={field.onChange}
                  aria-label={t('Balance traffic by account load')}
                />
              </FormControl>
            </FormItem>
          )}
        />

        <FormField
          control={form.control}
          name='opencode_go_affinity_fallback'
          render={({ field }) => (
            <FormItem className='flex min-h-16 items-center justify-between gap-4 py-3'>
              <div className='min-w-0'>
                <FormLabel>{t('Affinity fallback')}</FormLabel>
                <FormDescription>
                  {t(
                    'Keep requests that carry no session marker on a stable account'
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Select
                  value={field.value}
                  onValueChange={(value) => field.onChange(value)}
                >
                  <SelectTrigger className='w-40'>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectGroup>
                      <SelectItem value='none'>{t('Disabled')}</SelectItem>
                      <SelectItem value='token'>
                        {t('By request API key')}
                      </SelectItem>
                    </SelectGroup>
                  </SelectContent>
                </Select>
              </FormControl>
            </FormItem>
          )}
        />
      </div>

      <div className='flex flex-wrap items-center justify-between gap-2'>
        <div className='flex items-center gap-2'>
          <ShieldCheck
            className='text-muted-foreground h-3.5 w-3.5'
            aria-hidden='true'
          />
          <h4 className='text-muted-foreground text-xs font-medium tracking-wide uppercase'>
            {t('Lifecycle policy')}
          </h4>
        </div>
        {props.lifecyclePolicyReadOnly && props.onOpenLifecyclePolicy && (
          <Button
            type='button'
            variant='outline'
            size='sm'
            onClick={props.onOpenLifecyclePolicy}
          >
            {t('Open account pool')}
            <ArrowRight aria-hidden='true' />
          </Button>
        )}
      </div>

      {props.lifecyclePolicyReadOnly && (
        <Alert>
          <AlertDescription>
            {t(
              'Lifecycle policy is managed from the OpenCode Go account pool.'
            )}
          </AlertDescription>
        </Alert>
      )}

      <div className='divide-border/60 border-border/60 divide-y border-y'>
        <FormField
          control={form.control}
          name='opencode_go_auto_enable_china_models'
          render={({ field }) => (
            <FormItem className='flex min-h-16 items-center justify-between gap-4 py-3'>
              <div className='min-w-0'>
                <FormLabel>{t('Enable China-deployed models')}</FormLabel>
                <FormDescription>
                  {t(
                    'Applies only when global lifecycle automation is enabled'
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  disabled={lifecyclePolicyDisabled}
                  checked={field.value !== false}
                  onCheckedChange={field.onChange}
                  aria-label={t('Enable China-deployed models')}
                />
              </FormControl>
            </FormItem>
          )}
        />

        <FormField
          control={form.control}
          name='opencode_go_auto_apply_referral_rewards'
          render={({ field }) => (
            <FormItem className='flex min-h-16 items-center justify-between gap-4 py-3'>
              <div className='min-w-0'>
                <FormLabel>{t('Apply referral rewards')}</FormLabel>
                <FormDescription>
                  {t(
                    'Applies only to authoritative exhausted-member snapshots'
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  disabled={lifecyclePolicyDisabled}
                  checked={field.value !== false}
                  onCheckedChange={field.onChange}
                  aria-label={t('Apply referral rewards')}
                />
              </FormControl>
            </FormItem>
          )}
        />

        <FormField
          control={form.control}
          name='opencode_go_referral_rewards_max_per_run'
          render={({ field }) => (
            <FormItem className='grid min-h-16 gap-3 py-3 sm:grid-cols-[minmax(0,1fr)_7rem] sm:items-center'>
              <div className='min-w-0'>
                <FormLabel>{t('Referral rewards per run')}</FormLabel>
                <FormDescription>{t('Maximum 20')}</FormDescription>
              </div>
              <FormControl>
                <Input
                  disabled={lifecyclePolicyDisabled}
                  type='number'
                  min={0}
                  max={20}
                  value={field.value}
                  onChange={(event) =>
                    field.onChange(event.target.valueAsNumber)
                  }
                />
              </FormControl>
              <FormMessage className='sm:col-span-2' />
            </FormItem>
          )}
        />

        <FormField
          control={form.control}
          name='opencode_go_auto_cancel_subscription_renewal'
          render={({ field }) => (
            <FormItem className='flex min-h-16 items-center justify-between gap-4 py-3'>
              <div className='min-w-0'>
                <FormLabel>{t('Cancel subscription renewal')}</FormLabel>
                <FormDescription>
                  {t('Disabled by default; current-period access is retained')}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  disabled={lifecyclePolicyDisabled}
                  checked={field.value === true}
                  onCheckedChange={field.onChange}
                  aria-label={t('Cancel subscription renewal')}
                />
              </FormControl>
            </FormItem>
          )}
        />
      </div>
    </fieldset>
  )
}
