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
import { zodResolver } from '@hookform/resolvers/zod'
import { AlertTriangle, Loader2, Save } from 'lucide-react'
import { useEffect } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import type { z } from 'zod'

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'

import {
  openCodeGoLifecyclePolicySchema,
  type OpenCodeGoLifecyclePolicy,
} from '../../lib/opencode-go-schemas'

const policyFormSchema = openCodeGoLifecyclePolicySchema.omit({
  automation_enabled: true,
})

export type OpenCodeGoPolicyFormValues = z.infer<typeof policyFormSchema>

type OpenCodeGoPolicyFormProps = {
  policy: OpenCodeGoLifecyclePolicy
  disabled: boolean
  isSubmitting: boolean
  onSubmit: (values: OpenCodeGoPolicyFormValues) => void
}

export function OpenCodeGoPolicyForm(props: OpenCodeGoPolicyFormProps) {
  const { t } = useTranslation()
  const form = useForm<OpenCodeGoPolicyFormValues>({
    resolver: zodResolver(policyFormSchema),
    defaultValues: {
      auto_enable_china_models: props.policy.auto_enable_china_models,
      auto_apply_referral_rewards: props.policy.auto_apply_referral_rewards,
      referral_rewards_max_per_run: props.policy.referral_rewards_max_per_run,
      auto_cancel_subscription_renewal:
        props.policy.auto_cancel_subscription_renewal,
    },
  })

  useEffect(() => {
    form.reset({
      auto_enable_china_models: props.policy.auto_enable_china_models,
      auto_apply_referral_rewards: props.policy.auto_apply_referral_rewards,
      referral_rewards_max_per_run: props.policy.referral_rewards_max_per_run,
      auto_cancel_subscription_renewal:
        props.policy.auto_cancel_subscription_renewal,
    })
  }, [form, props.policy])

  return (
    <Form {...form}>
      <form
        onSubmit={form.handleSubmit(props.onSubmit)}
        className='mx-auto flex w-full max-w-3xl flex-col gap-5'
      >
        <div className='flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between'>
          <div>
            <h3 className='text-sm font-semibold'>{t('Lifecycle policy')}</h3>
            <p className='text-muted-foreground mt-1 text-xs'>
              {t('Channel-scoped automation policy')}
            </p>
          </div>
          <Badge
            variant={props.policy.automation_enabled ? 'default' : 'secondary'}
          >
            {props.policy.automation_enabled
              ? t('Global automation enabled')
              : t('Global automation disabled')}
          </Badge>
        </div>

        {!props.policy.automation_enabled && (
          <Alert>
            <AlertTriangle className='size-4' />
            <AlertTitle>{t('Automation is globally disabled')}</AlertTitle>
            <AlertDescription>
              {t('Saved channel policies will not run automatically')}
            </AlertDescription>
          </Alert>
        )}

        <fieldset
          disabled={props.disabled || props.isSubmitting}
          className='divide-border/60 border-border/60 divide-y border-y disabled:opacity-60'
        >
          <FormField
            control={form.control}
            name='auto_enable_china_models'
            render={({ field }) => (
              <FormItem className='flex min-h-18 items-center justify-between gap-4 py-3'>
                <div className='min-w-0'>
                  <FormLabel>{t('Enable China-deployed models')}</FormLabel>
                  <FormDescription>
                    {t('Synchronize the workspace model-region setting')}
                  </FormDescription>
                </div>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                    aria-label={t('Enable China-deployed models')}
                  />
                </FormControl>
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='auto_apply_referral_rewards'
            render={({ field }) => (
              <FormItem className='flex min-h-18 items-center justify-between gap-4 py-3'>
                <div className='min-w-0'>
                  <FormLabel>{t('Apply referral rewards')}</FormLabel>
                  <FormDescription>
                    {t(
                      'Only after all authoritative quota windows are exhausted'
                    )}
                  </FormDescription>
                </div>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                    aria-label={t('Apply referral rewards')}
                  />
                </FormControl>
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='referral_rewards_max_per_run'
            render={({ field }) => (
              <FormItem className='grid min-h-18 gap-3 py-3 sm:grid-cols-[minmax(0,1fr)_7rem] sm:items-center'>
                <div className='min-w-0'>
                  <FormLabel>{t('Referral rewards per run')}</FormLabel>
                  <FormDescription>{t('Maximum 20')}</FormDescription>
                </div>
                <FormControl>
                  <Input
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
            name='auto_cancel_subscription_renewal'
            render={({ field }) => (
              <FormItem className='flex min-h-18 items-center justify-between gap-4 py-3'>
                <div className='min-w-0'>
                  <FormLabel>{t('Cancel subscription renewal')}</FormLabel>
                  <FormDescription className='text-destructive'>
                    {t('Disabled by default; access remains until period end')}
                  </FormDescription>
                </div>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                    aria-label={t('Cancel subscription renewal')}
                  />
                </FormControl>
              </FormItem>
            )}
          />
        </fieldset>

        <div className='flex justify-end'>
          <Button
            type='submit'
            disabled={
              props.disabled || props.isSubmitting || !form.formState.isDirty
            }
          >
            {props.isSubmitting ? (
              <Loader2 className='size-4 animate-spin' />
            ) : (
              <Save className='size-4' />
            )}
            {t('Save policy')}
          </Button>
        </div>
      </form>
    </Form>
  )
}
