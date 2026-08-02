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
import { Cookie, Loader2, Pencil } from 'lucide-react'
import { useEffect } from 'react'
import { flushSync } from 'react-dom'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
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
import { Textarea } from '@/components/ui/textarea'

import type { OpenCodeGoIdentity } from '../../lib/opencode-go-schemas'

const importSchema = z.object({
  label: z.string().max(128),
  authCookies: z
    .string()
    .trim()
    .min(1)
    .max(2 * 1024 * 1024),
})

const cookieSchema = z.object({
  authCookie: z
    .string()
    .trim()
    .min(1)
    .max(64 * 1024),
})

const labelSchema = z.object({
  label: z.string().max(128),
})

type ImportValues = z.infer<typeof importSchema>
type CookieValues = z.infer<typeof cookieSchema>
type LabelValues = z.infer<typeof labelSchema>

type ImportDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  isSubmitting: boolean
  onSubmit: (values: ImportValues) => void
}

export function OpenCodeGoImportDialog(props: ImportDialogProps) {
  const { t } = useTranslation()
  const form = useForm<ImportValues>({
    resolver: zodResolver(importSchema),
    defaultValues: { label: '', authCookies: '' },
  })

  useEffect(() => {
    if (props.open) form.reset({ label: '', authCookies: '' })
  }, [form, props.open])

  const submit = (values: ImportValues) => {
    const payload = {
      label: values.label.trim(),
      authCookies: values.authCookies,
    }
    flushSync(() => {
      form.setValue('authCookies', '')
      props.onOpenChange(false)
    })
    props.onSubmit(payload)
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={
        <span className='flex items-center gap-2'>
          <Cookie className='size-5' aria-hidden='true' />
          {t('Import OpenCode Go accounts')}
        </span>
      }
      description={t('Paste one auth Cookie per line')}
      showCloseButton={!props.isSubmitting}
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            disabled={props.isSubmitting}
            onClick={() => props.onOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='submit'
            form='opencode-go-import-form'
            disabled={props.isSubmitting}
          >
            {props.isSubmitting && <Loader2 className='size-4 animate-spin' />}
            {t('Verify and import')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <form
          id='opencode-go-import-form'
          className='space-y-4'
          onSubmit={form.handleSubmit(submit)}
          autoComplete='off'
        >
          <FormField
            control={form.control}
            name='label'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Label')}</FormLabel>
                <FormControl>
                  <Input placeholder={t('Optional account label')} {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='authCookies'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Auth Cookies')}</FormLabel>
                <FormControl>
                  <Textarea
                    className='min-h-44 resize-y font-mono text-xs'
                    autoComplete='off'
                    spellCheck={false}
                    {...field}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'The Cookie field is cleared immediately after submission'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
        </form>
      </Form>
    </Dialog>
  )
}

type CookieDialogProps = {
  identity: OpenCodeGoIdentity | null
  open: boolean
  onOpenChange: (open: boolean) => void
  isSubmitting: boolean
  onSubmit: (identity: OpenCodeGoIdentity, authCookie: string) => void
}

export function OpenCodeGoCookieDialog(props: CookieDialogProps) {
  const { t } = useTranslation()
  const form = useForm<CookieValues>({
    resolver: zodResolver(cookieSchema),
    defaultValues: { authCookie: '' },
  })

  useEffect(() => {
    if (props.open) form.reset({ authCookie: '' })
  }, [form, props.open])

  const submit = (values: CookieValues) => {
    if (!props.identity) return
    const authCookie = values.authCookie
    flushSync(() => {
      form.setValue('authCookie', '')
      props.onOpenChange(false)
    })
    props.onSubmit(props.identity, authCookie)
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Replace account Cookie')}
      description={props.identity?.label || props.identity?.email || ''}
      showCloseButton={!props.isSubmitting}
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            disabled={props.isSubmitting}
            onClick={() => props.onOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='submit'
            form='opencode-go-cookie-form'
            disabled={props.isSubmitting}
          >
            {props.isSubmitting && <Loader2 className='size-4 animate-spin' />}
            {t('Verify and replace')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <form
          id='opencode-go-cookie-form'
          onSubmit={form.handleSubmit(submit)}
          autoComplete='off'
        >
          <FormField
            control={form.control}
            name='authCookie'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Auth Cookie')}</FormLabel>
                <FormControl>
                  <Textarea
                    className='min-h-32 resize-y font-mono text-xs'
                    autoComplete='off'
                    spellCheck={false}
                    {...field}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </form>
      </Form>
    </Dialog>
  )
}

type LabelDialogProps = {
  identity: OpenCodeGoIdentity | null
  open: boolean
  onOpenChange: (open: boolean) => void
  isSubmitting: boolean
  onSubmit: (identity: OpenCodeGoIdentity, label: string) => void
}

export function OpenCodeGoLabelDialog(props: LabelDialogProps) {
  const { t } = useTranslation()
  const form = useForm<LabelValues>({
    resolver: zodResolver(labelSchema),
    defaultValues: { label: '' },
  })

  useEffect(() => {
    if (props.open) form.reset({ label: props.identity?.label || '' })
  }, [form, props.identity, props.open])

  const submit = (values: LabelValues) => {
    if (!props.identity) return
    props.onSubmit(props.identity, values.label.trim())
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={
        <span className='flex items-center gap-2'>
          <Pencil className='size-5' aria-hidden='true' />
          {t('Edit account label')}
        </span>
      }
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            disabled={props.isSubmitting}
            onClick={() => props.onOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='submit'
            form='opencode-go-label-form'
            disabled={props.isSubmitting}
          >
            {props.isSubmitting && <Loader2 className='size-4 animate-spin' />}
            {t('Save changes')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <form id='opencode-go-label-form' onSubmit={form.handleSubmit(submit)}>
          <FormField
            control={form.control}
            name='label'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Label')}</FormLabel>
                <FormControl>
                  <Input {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </form>
      </Form>
    </Dialog>
  )
}
