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
import assert from 'node:assert/strict'
import { after, describe, test } from 'node:test'

import { Window } from 'happy-dom'

const domWindow = new Window()
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'CustomEvent',
  'MutationObserver',
  'requestAnimationFrame',
  'cancelAnimationFrame',
  'getComputedStyle',
  'matchMedia',
  'customElements',
] as const

for (const key of domGlobals) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { flexRender, getCoreRowModel, useReactTable } =
  await import('@tanstack/react-table')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        'Token Breakdown': 'Token Breakdown',
        'Uncached Input Tokens': 'Uncached Input Tokens',
        'Total Input Tokens': 'Total Input Tokens',
        'Output Tokens': 'Output Tokens',
        'Cache Read': 'Cache Read',
        'Cache Write (5m)': 'Cache Write (5m)',
        'Cache Write (1h)': 'Cache Write (1h)',
        Cache: 'Cache',
      },
    },
  },
})

const { usageLogSchema } = await import('../../data/schema')
const { TokenBreakdown } = await import('../dialogs/details-dialog')
const { useCommonLogsColumns } = await import('../columns/common-logs-columns')
const { UsageLogsMobileList } = await import('../usage-logs-mobile-card')
type UsageLog = import('../../data/schema').UsageLog

function DesktopTokenCell({ log }: { log: UsageLog }) {
  const columns = useCommonLogsColumns(false)
  const table = useReactTable({
    columns,
    data: [log],
    getCoreRowModel: getCoreRowModel(),
  })
  const cell = table
    .getRowModel()
    .rows[0]?.getAllCells()
    .find((candidate) => candidate.column.id === 'prompt_tokens')
  assert.ok(cell)
  return flexRender(cell.column.columnDef.cell, cell.getContext())
}

function MobileTokenCard({ log }: { log: UsageLog }) {
  const table = useReactTable({
    columns: [{ accessorKey: 'created_at' }],
    data: [log],
    getCoreRowModel: getCoreRowModel(),
  })
  return <UsageLogsMobileList table={table} logCategory='common' />
}
const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

describe('usage log input token display', () => {
  after(() => {
    domWindow.close()
  })

  test('labels uncached and cache-read input without exposing total input', async () => {
    const container = document.createElement('div')
    document.body.append(container)
    const root = createRoot(container)
    const log = usageLogSchema.parse({
      id: 1,
      user_id: 1,
      created_at: 1,
      type: 2,
      content: '',
      prompt_tokens: 30_289,
      completion_tokens: 55,
    })

    await act(async () => {
      root.render(
        <I18nextProvider i18n={i18n}>
          <TokenBreakdown
            log={log}
            other={{ input_tokens_total: 30_289, cache_tokens: 29_952 }}
          />
        </I18nextProvider>
      )
    })

    const text = (container.textContent ?? '').replaceAll(/\s+/g, ' ')
    assert.match(text, /Uncached Input Tokens\s*337/)
    assert.doesNotMatch(text, /Total Input Tokens/)
    assert.doesNotMatch(text, /30,289/)
    assert.match(text, /Cache Read\s*29,952/)
    assert.match(text, /Output Tokens\s*55/)

    await act(async () => root.unmount())
    container.remove()
  })

  test('does not subtract cache or claim a total without explicit total input', async () => {
    const container = document.createElement('div')
    document.body.append(container)
    const root = createRoot(container)
    const log = usageLogSchema.parse({
      id: 2,
      user_id: 1,
      created_at: 1,
      type: 2,
      content: '',
      prompt_tokens: 900,
      completion_tokens: 55,
    })

    await act(async () => {
      root.render(
        <I18nextProvider i18n={i18n}>
          <TokenBreakdown log={log} other={{ cache_tokens: 256 }} />
        </I18nextProvider>
      )
    })

    const text = (container.textContent ?? '').replaceAll(/\s+/g, ' ')
    assert.match(text, /Uncached Input Tokens\s*900/)
    assert.match(text, /Cache Read\s*256/)
    assert.doesNotMatch(text, /Total Input Tokens/)
    assert.doesNotMatch(text, /644/)

    await act(async () => root.unmount())
    container.remove()
  })

  test('hides Messages total while rendering cache and output across views', async () => {
    const container = document.createElement('div')
    document.body.append(container)
    const root = createRoot(container)
    const other = {
      input_tokens_total: 210,
      cache_tokens: 80,
      cache_creation_tokens_5m: 20,
      cache_creation_tokens_1h: 10,
    }
    const log = usageLogSchema.parse({
      id: 3,
      user_id: 1,
      created_at: 1,
      type: 2,
      content: '',
      prompt_tokens: 100,
      completion_tokens: 40,
      other: JSON.stringify(other),
    })

    await act(async () => {
      root.render(
        <I18nextProvider i18n={i18n}>
          <div data-view='desktop'>
            <DesktopTokenCell log={log} />
          </div>
          <div data-view='mobile'>
            <MobileTokenCard log={log} />
          </div>
          <div data-view='details'>
            <TokenBreakdown log={log} other={other} />
          </div>
        </I18nextProvider>
      )
    })

    const textFor = (view: string) =>
      (container.querySelector(`[data-view="${view}"]`)?.textContent ?? '')
        .replaceAll(/\s+/g, ' ')
        .trim()

    const desktopText = textFor('desktop')
    assert.match(desktopText, /130 \/ 40/)
    assert.doesNotMatch(desktopText, /Total Input Tokens/)
    assert.doesNotMatch(desktopText, /\b210\b/)
    assert.match(desktopText, /Cache↓ 80/)
    assert.match(desktopText, /↑ 30/)

    const mobileText = textFor('mobile')
    assert.match(mobileText, /130 \/ 40/)
    assert.doesNotMatch(mobileText, /Total Input Tokens/)
    assert.doesNotMatch(mobileText, /\b210\b/)
    assert.match(mobileText, /Cache↓ 80/)
    assert.match(mobileText, /↑ 30/)

    const detailsText = textFor('details')
    assert.match(detailsText, /Uncached Input Tokens\s*130/)
    assert.doesNotMatch(detailsText, /Total Input Tokens/)
    assert.doesNotMatch(detailsText, /\b210\b/)
    assert.match(detailsText, /Output Tokens\s*40/)
    assert.match(detailsText, /Cache Read\s*80/)
    assert.match(detailsText, /Cache Write \(5m\)\s*20/)
    assert.match(detailsText, /Cache Write \(1h\)\s*10/)

    await act(async () => root.unmount())
    container.remove()
  })
})
