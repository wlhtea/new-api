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

import type { ColumnDef } from '@tanstack/react-table'
import { Window } from 'happy-dom'

import type { UsageLog } from '../../data/schema'

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
        Affinity: 'Affinity',
        'Request Fingerprint': 'Request fingerprint',
        'Round Robin': 'Round robin',
        Workspace: 'Workspace',
        Source: 'Source',
        Other: 'Other',
        Token: 'Token',
      },
    },
  },
})

const { usageLogSchema } = await import('../../data/schema')
const { useCommonLogsColumns } = await import('../columns/common-logs-columns')
const { UsageLogsMobileList } = await import('../usage-logs-mobile-card')

function AdminAffinityViews({ log }: { log: UsageLog }) {
  const columns = useCommonLogsColumns(true)
  const affinityColumn = columns.find((column) => column.id === 'affinity')
  assert.ok(affinityColumn)

  const desktopTable = useReactTable({
    columns: [affinityColumn],
    data: [log],
    getCoreRowModel: getCoreRowModel(),
  })
  const desktopCell = desktopTable.getRowModel().rows[0]?.getAllCells()[0]
  assert.ok(desktopCell)

  const mobileColumns: ColumnDef<UsageLog>[] = [
    { accessorKey: 'created_at' },
    affinityColumn,
  ]
  const mobileTable = useReactTable({
    columns: mobileColumns,
    data: [log],
    getCoreRowModel: getCoreRowModel(),
  })

  return (
    <>
      <div data-view='desktop'>
        {flexRender(
          desktopCell.column.columnDef.cell,
          desktopCell.getContext()
        )}
      </div>
      <div data-view='mobile'>
        <UsageLogsMobileList table={mobileTable} logCategory='common' />
      </div>
    </>
  )
}

function UserAffinityView({ log }: { log: UsageLog }) {
  const columns = useCommonLogsColumns(false)
  assert.equal(
    columns.find((column) => column.id === 'affinity'),
    undefined
  )

  const mobileColumns: ColumnDef<UsageLog>[] = [{ accessorKey: 'created_at' }]
  const mobileTable = useReactTable({
    columns: mobileColumns,
    data: [log],
    getCoreRowModel: getCoreRowModel(),
  })

  return (
    <div data-view='user-mobile'>
      <UsageLogsMobileList table={mobileTable} logCategory='common' />
    </div>
  )
}

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

describe('usage log affinity display', () => {
  after(() => {
    domWindow.close()
  })

  test('shows one compact fingerprint and workspace marker in admin desktop and mobile views', async () => {
    const container = document.createElement('div')
    document.body.append(container)
    const root = createRoot(container)
    const workspaceUid = '4f7c9a10-6b7e-4e26-91b4-a71687fb4c01'
    const log = usageLogSchema.parse({
      id: 1,
      user_id: 1,
      created_at: 1,
      type: 2,
      content: '',
      other: JSON.stringify({
        admin_info: {
          opencode_go_affinity_source: 'claude-code-session',
          opencode_go_workspace_uid: workspaceUid,
        },
      }),
    })

    await act(async () => {
      root.render(
        <I18nextProvider i18n={i18n}>
          <AdminAffinityViews log={log} />
        </I18nextProvider>
      )
    })

    for (const view of ['desktop', 'mobile']) {
      const element = container.querySelector(`[data-view="${view}"]`)
      assert.ok(element)
      const text = (element.textContent ?? '').replaceAll(/\s+/g, ' ')
      assert.match(text, /Request fingerprint/)
      assert.match(text, /ws: 4f7c9a10/)
      const marker = element.querySelector(
        '[data-affinity-method="fingerprint"]'
      )
      assert.ok(marker)
      assert.match(
        marker.getAttribute('aria-label') ?? '',
        /X-Claude-Code-Session-Id/
      )
      assert.match(
        marker.getAttribute('aria-label') ?? '',
        new RegExp(workspaceUid)
      )
    }

    assert.equal(
      container.querySelectorAll(
        '[data-view="mobile"] [data-affinity-method="fingerprint"]'
      ).length,
      1
    )

    await act(async () => root.unmount())
    container.remove()
  })

  test('hides affinity columns and mobile fields from ordinary users even when payload data exists', async () => {
    const container = document.createElement('div')
    document.body.append(container)
    const root = createRoot(container)
    const workspaceUid = '4f7c9a10-6b7e-4e26-91b4-a71687fb4c01'
    const log = usageLogSchema.parse({
      id: 1,
      user_id: 1,
      created_at: 1,
      type: 2,
      content: '',
      other: JSON.stringify({
        opencode_go_affinity_source: 'claude-code-session',
        opencode_go_workspace_uid: workspaceUid,
        admin_info: {
          opencode_go_affinity_source: 'claude-code-session',
          opencode_go_workspace_uid: workspaceUid,
        },
      }),
    })

    await act(async () => {
      root.render(
        <I18nextProvider i18n={i18n}>
          <UserAffinityView log={log} />
        </I18nextProvider>
      )
    })

    const mobileView = container.querySelector('[data-view="user-mobile"]')
    assert.ok(mobileView)
    assert.equal(mobileView.querySelector('[data-affinity-method]'), null)
    assert.doesNotMatch(mobileView.textContent ?? '', /Request fingerprint/)
    assert.doesNotMatch(mobileView.textContent ?? '', /4f7c9a10/)
    assert.doesNotMatch(mobileView.textContent ?? '', new RegExp(workspaceUid))

    await act(async () => root.unmount())
    container.remove()
  })
})
