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
import { useState } from 'react'

import type {
  OpenCodeGoIdentity,
  OpenCodeGoWorkspace,
} from '../../lib/opencode-go-schemas'
import { OpenCodeGoAccountCard } from './opencode-go-account-card'
import { OpenCodeGoIdentityDetailsDialog } from './opencode-go-identity-details-dialog'
import type { OpenCodeGoWorkspaceSensitiveAction } from './opencode-go-workspace-row'

type OpenCodeGoIdentitySectionProps = {
  identity: OpenCodeGoIdentity
  visibleWorkspaceUids?: Set<string>
  nowSeconds: number
  locale?: string
  canOperate: boolean
  canSensitiveWrite: boolean
  busyKey: string | null
  onEditLabel: (identity: OpenCodeGoIdentity) => void
  onReplaceCookie: (identity: OpenCodeGoIdentity) => void
  onRefreshIdentity: (identityUid: string) => void
  onToggleIdentity: (identityUid: string, enabled: boolean) => void
  onDeleteIdentity: (identity: OpenCodeGoIdentity) => void
  onRefreshWorkspace: (workspaceUid: string) => void
  onRiskRecheckWorkspace: (workspaceUid: string) => void
  onToggleWorkspace: (workspaceUid: string, enabled: boolean) => void
  onWorkspaceSensitiveAction: (
    action: OpenCodeGoWorkspaceSensitiveAction,
    workspace: OpenCodeGoWorkspace
  ) => void
}

export function OpenCodeGoIdentitySection(
  props: OpenCodeGoIdentitySectionProps
) {
  const [detailsOpen, setDetailsOpen] = useState(false)
  const workspaces = props.identity.workspaces.filter(
    (workspace) =>
      !props.visibleWorkspaceUids ||
      props.visibleWorkspaceUids.has(workspace.uid)
  )

  if (workspaces.length === 0 && props.visibleWorkspaceUids) return null

  return (
    <>
      <OpenCodeGoAccountCard
        identity={props.identity}
        workspaces={workspaces}
        onOpenDetails={() => setDetailsOpen(true)}
      />
      <OpenCodeGoIdentityDetailsDialog
        open={detailsOpen}
        onOpenChange={setDetailsOpen}
        identity={props.identity}
        workspaces={workspaces}
        nowSeconds={props.nowSeconds}
        locale={props.locale}
        canOperate={props.canOperate}
        canSensitiveWrite={props.canSensitiveWrite}
        busyKey={props.busyKey}
        onEditLabel={(identity) => {
          setDetailsOpen(false)
          props.onEditLabel(identity)
        }}
        onReplaceCookie={(identity) => {
          setDetailsOpen(false)
          props.onReplaceCookie(identity)
        }}
        onRefreshIdentity={props.onRefreshIdentity}
        onToggleIdentity={props.onToggleIdentity}
        onDeleteIdentity={(identity) => {
          setDetailsOpen(false)
          props.onDeleteIdentity(identity)
        }}
        onRefreshWorkspace={props.onRefreshWorkspace}
        onRiskRecheckWorkspace={props.onRiskRecheckWorkspace}
        onToggleWorkspace={props.onToggleWorkspace}
        onWorkspaceSensitiveAction={(action, workspace) => {
          setDetailsOpen(false)
          props.onWorkspaceSensitiveAction(action, workspace)
        }}
      />
    </>
  )
}
