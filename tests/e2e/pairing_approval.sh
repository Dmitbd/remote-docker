#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/e2e/lib.sh
source "${script_dir}/lib.sh"

e2e_require_opt_in
[[ -t 0 ]] || e2e_fail 'pairing acceptance is interactive and requires a terminal'

e2e_note 'Start hosting on Windows, start search on the Mac, select the expected PC, and verify that both applications show the same device names and six-digit code. Type SAME_CODE.'
read -r confirmation
[[ "${confirmation}" == 'SAME_CODE' ]] || e2e_fail 'matching pairing identity and code were not confirmed'

e2e_note 'Approve only on Windows. Verify both applications show a connected Mac-to-Windows link and their respective Client/Host roles. Type CONNECTED_ROLES.'
read -r confirmation
[[ "${confirmation}" == 'CONNECTED_ROLES' ]] || e2e_fail 'connected role display was not confirmed'

e2e_note 'Try to connect another Mac while the trusted pair exists. Verify Windows does not accept it and the UI explains the one-device limit. Type ONE_TRUSTED_PEER.'
read -r confirmation
[[ "${confirmation}" == 'ONE_TRUSTED_PEER' ]] || e2e_fail 'one trusted peer enforcement was not confirmed'

e2e_note 'Disconnect from the first Mac. Verify Windows reports who disconnected and returns to Waiting for connection; pause Windows and verify it becomes Paused. Type DISCONNECT_VISIBLE.'
read -r confirmation
[[ "${confirmation}" == 'DISCONNECT_VISIBLE' ]] || e2e_fail 'visible disconnect and host pause were not confirmed'

e2e_note 'Visible pairing approval, roles, one-peer limit, and disconnect state: PASS'
