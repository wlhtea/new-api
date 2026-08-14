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
import { OpenCodeProtocolSettings } from './opencode-protocol-settings'

type OpenCodeAPIKeyChannelSettingsProps = {
  disabled?: boolean
}

export function OpenCodeAPIKeyChannelSettings(
  props: OpenCodeAPIKeyChannelSettingsProps
) {
  return (
    <fieldset
      disabled={props.disabled}
      className='border-border/60 flex min-w-0 flex-col gap-5 border-t pt-5 disabled:opacity-60'
    >
      <OpenCodeProtocolSettings title='OpenCode protocol routing' />
    </fieldset>
  )
}
