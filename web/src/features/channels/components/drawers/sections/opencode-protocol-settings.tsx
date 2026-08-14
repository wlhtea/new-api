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
import { Textarea } from '@/components/ui/textarea'

import type { ChannelFormInput } from '../../../lib/channel-form'

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
    </>
  )
}
