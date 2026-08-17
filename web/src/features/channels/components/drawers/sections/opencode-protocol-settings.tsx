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
import { Waypoints } from 'lucide-react'
import type { ReactNode } from 'react'
import { useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormDescription,
  FormMessage,
} from '@/components/ui/form'
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

import {
  OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_DROP_KNOWN,
  OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_INVALID,
  OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_STRICT,
  type ChannelFormInput,
} from '../../../lib/channel-form'

type OpenCodeProtocolSettingsProps = {
  title: string
  notice?: ReactNode
}

export function OpenCodeProtocolSettings(props: OpenCodeProtocolSettingsProps) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormInput>()

  return (
    <>
      <div className='flex items-center gap-2'>
        <Waypoints
          className='text-muted-foreground h-3.5 w-3.5'
          aria-hidden='true'
        />
        <h4 className='text-muted-foreground text-xs font-medium tracking-wide uppercase'>
          {t(props.title)}
        </h4>
      </div>

      {props.notice}

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

      <FormField
        control={form.control}
        name='opencode_go_unsupported_optional_field_policy'
        render={({ field }) => {
          const hasInvalidPersistedPolicy =
            field.value ===
            OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_INVALID

          return (
            <FormItem>
              <FormLabel>{t('Unsupported optional field policy')}</FormLabel>
              <Select
                items={[
                  {
                    value: OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_STRICT,
                    label: t('Strict: reject unrelayable registered hints'),
                  },
                  {
                    value:
                      OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_DROP_KNOWN,
                    label: t('Drop known optional hints'),
                  },
                  {
                    value:
                      OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_INVALID,
                    label: t('Invalid stored policy'),
                  },
                ]}
                value={field.value}
                onValueChange={field.onChange}
              >
                <FormControl>
                  <SelectTrigger aria-invalid={hasInvalidPersistedPolicy}>
                    <SelectValue />
                  </SelectTrigger>
                </FormControl>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    <SelectItem
                      value={
                        OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_STRICT
                      }
                    >
                      {t('Strict: reject unrelayable registered hints')}
                    </SelectItem>
                    <SelectItem
                      value={
                        OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_DROP_KNOWN
                      }
                    >
                      {t('Drop known optional hints')}
                    </SelectItem>
                    <SelectItem
                      value={
                        OPENCODE_GO_UNSUPPORTED_OPTIONAL_FIELD_POLICY_INVALID
                      }
                      disabled
                    >
                      {t('Invalid stored policy')}
                    </SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
              <FormDescription
                className={
                  hasInvalidPersistedPolicy ? 'text-destructive' : undefined
                }
              >
                {hasInvalidPersistedPolicy
                  ? t(
                      'The stored policy is invalid. Select a valid policy before saving.'
                    )
                  : t(
                      'Malformed, unknown, security-sensitive, and core-semantic fields are always rejected.'
                    )}
              </FormDescription>
              <FormMessage />
            </FormItem>
          )
        }}
      />

      <FormField
        control={form.control}
        name='opencode_go_billing_usage_conversion_enabled'
        render={({ field }) => (
          <FormItem className='border-border/60 flex min-h-16 items-center justify-between gap-4 border-y py-3'>
            <div className='min-w-0'>
              <FormLabel>
                {t('Enable OpenAI-compatible Usage conversion')}
              </FormLabel>
              <FormDescription>
                {t(
                  'Controls public Usage projection only; it does not change model pricing or internal settlement.'
                )}
              </FormDescription>
            </div>
            <FormControl>
              <Switch
                checked={field.value !== false}
                onCheckedChange={field.onChange}
                aria-label={t('Enable OpenAI-compatible Usage conversion')}
              />
            </FormControl>
            <FormMessage />
          </FormItem>
        )}
      />
    </>
  )
}
