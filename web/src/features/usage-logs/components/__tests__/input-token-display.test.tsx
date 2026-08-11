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
        'Input Tokens': 'Input Tokens',
        'Output Tokens': 'Output Tokens',
        'Cache Read': 'Cache Read',
        'Cache Write': 'Cache Write',
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

async function renderTokenViews(
  log: UsageLog,
  other: import('../../types').LogOtherData
) {
  const container = document.createElement('div')
  document.body.append(container)
  const root = createRoot(container)

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

  return {
    textFor(view: 'desktop' | 'mobile' | 'details') {
      return (
        container.querySelector(`[data-view="${view}"]`)?.textContent ?? ''
      )
        .replaceAll(/\s+/g, ' ')
        .trim()
    },
    async cleanup() {
      await act(async () => root.unmount())
      container.remove()
    },
  }
}

function createLog(
  id: number,
  promptTokens: number,
  completionTokens: number,
  other: import('../../types').LogOtherData
): UsageLog {
  return usageLogSchema.parse({
    id,
    user_id: 1,
    created_at: 1,
    type: 2,
    content: '',
    prompt_tokens: promptTokens,
    completion_tokens: completionTokens,
    other: JSON.stringify(other),
  })
}

describe('usage log input token display', () => {
  after(() => {
    domWindow.close()
  })

  test('renders OpenAI top-level usage across all views', async () => {
    const other = {
      input_tokens_total: 210,
      cache_tokens: 80,
      cache_creation_tokens: 30,
    }
    const view = await renderTokenViews(createLog(1, 210, 40, other), other)

    for (const name of ['desktop', 'mobile'] as const) {
      const text = view.textFor(name)
      assert.match(text, /210 \/ 40/)
      assert.match(text, /Cache↓ 80/)
      assert.match(text, /↑ 30/)
      assert.doesNotMatch(text, /\b130\b/)
    }

    const detailsText = view.textFor('details')
    assert.match(detailsText, /Input Tokens\s*210/)
    assert.match(detailsText, /Output Tokens\s*40/)
    assert.match(detailsText, /Cache Read\s*80/)
    assert.match(detailsText, /Cache Write\s*30/)
    assert.doesNotMatch(detailsText, /\b130\b/)

    await view.cleanup()
  })

  test('renders Claude Messages native input across all views', async () => {
    const other = {
      input_tokens_total: 210,
      cache_tokens: 80,
      cache_creation_tokens_5m: 20,
      cache_creation_tokens_1h: 10,
    }
    const view = await renderTokenViews(createLog(2, 100, 40, other), other)

    for (const name of ['desktop', 'mobile'] as const) {
      const text = view.textFor(name)
      assert.match(text, /100 \/ 40/)
      assert.match(text, /Cache↓ 80/)
      assert.match(text, /↑ 30/)
      assert.doesNotMatch(text, /\b130\b/)
      assert.doesNotMatch(text, /\b210\b/)
    }

    const detailsText = view.textFor('details')
    assert.match(detailsText, /Input Tokens\s*100/)
    assert.doesNotMatch(detailsText, /\b210\b/)
    assert.doesNotMatch(detailsText, /\b130\b/)
    assert.match(detailsText, /Output Tokens\s*40/)
    assert.match(detailsText, /Cache Read\s*80/)
    assert.match(detailsText, /Cache Write \(5m\)\s*20/)
    assert.match(detailsText, /Cache Write \(1h\)\s*10/)

    await view.cleanup()
  })

  test('keeps legacy prompt input when no normalized total exists', async () => {
    const other = { cache_tokens: 256 }
    const view = await renderTokenViews(createLog(3, 900, 55, other), other)

    for (const name of ['desktop', 'mobile'] as const) {
      const text = view.textFor(name)
      assert.match(text, /900 \/ 55/)
      assert.match(text, /Cache↓ 256/)
      assert.doesNotMatch(text, /\b644\b/)
    }

    const detailsText = view.textFor('details')
    assert.match(detailsText, /Input Tokens\s*900/)
    assert.match(detailsText, /Output Tokens\s*55/)
    assert.match(detailsText, /Cache Read\s*256/)
    assert.doesNotMatch(detailsText, /\b644\b/)

    await view.cleanup()
  })

  test('keeps no-cache values and zero-value placeholders unchanged', async () => {
    const noCacheView = await renderTokenViews(createLog(4, 12, 3, {}), {})

    assert.match(noCacheView.textFor('desktop'), /12 \/ 3/)
    assert.match(noCacheView.textFor('mobile'), /12 \/ 3/)
    assert.match(noCacheView.textFor('details'), /Input Tokens\s*12/)
    assert.match(noCacheView.textFor('details'), /Output Tokens\s*3/)
    assert.doesNotMatch(noCacheView.textFor('details'), /Cache/)

    await noCacheView.cleanup()

    const zeroView = await renderTokenViews(createLog(5, 0, 0, {}), {})

    assert.equal(zeroView.textFor('desktop'), '-')
    assert.match(zeroView.textFor('mobile'), /-/)
    assert.equal(zeroView.textFor('details'), '')

    await zeroView.cleanup()
  })
})
