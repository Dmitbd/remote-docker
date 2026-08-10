#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/e2e/lib.sh
source "${script_dir}/lib.sh"

e2e_require_opt_in
[[ -t 0 ]] || e2e_fail 'desktop lifecycle acceptance is interactive and requires a terminal'

e2e_note 'Restart both computers. Before launching Remote Docker, verify that the app, managed WSL distribution, and Remote Docker console windows are absent. Type CLEAN_REBOOT.'
read -r confirmation
[[ "${confirmation}" == 'CLEAN_REBOOT' ]] || e2e_fail 'clean manual-start state was not confirmed'

e2e_note 'Launch Remote Docker manually on both computers. Verify both open in Paused state and do not start Docker work automatically. Type PAUSED.'
read -r confirmation
[[ "${confirmation}" == 'PAUSED' ]] || e2e_fail 'initial Paused state was not confirmed'

e2e_note 'On Windows, start hosting and then pause it. Verify the UI returns to Paused and owned WSL/sync work stops. Type HOST_PAUSED.'
read -r confirmation
[[ "${confirmation}" == 'HOST_PAUSED' ]] || e2e_fail 'host pause cleanup was not confirmed'

e2e_note 'Close the application window and verify only the tray/menu-bar icon remains. Reopen it, choose Finish work, then verify the icon and owned background processes disappear. Type QUIT_CLEAN.'
read -r confirmation
[[ "${confirmation}" == 'QUIT_CLEAN' ]] || e2e_fail 'clean application quit was not confirmed'

e2e_note 'Manual start, pause, close-to-tray, and clean quit: PASS'
