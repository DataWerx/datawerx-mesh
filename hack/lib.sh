#!/usr/bin/env bash
# Shared console styling for the hack/ scripts: light color + emoji helpers.
#
# Source it near the top of a script (the scripts live one level below hack/):
#   source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib.sh"
#
# Color auto-disables when stdout is not a terminal or NO_COLOR is set, so piped
# logs and CI output stay plain. The emoji are plain unicode and always emitted.
#
# Helpers:
#   say <msg>      blue "==>" progress step
#   ok <msg>       green ✅ success line
#   warn <msg>     yellow ⚠️ recoverable degradation
#   die <msg>      red ❌ failure line (to stderr; does not exit)
#   pass <name>    green ✅ ok test result
#   failmsg <name> red ❌ FAIL test result

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  _c_reset=$'\e[0m'; _c_blue=$'\e[34m'; _c_green=$'\e[32m'
  _c_yellow=$'\e[33m'; _c_red=$'\e[31m'; _c_bold=$'\e[1m'
else
  _c_reset=; _c_blue=; _c_green=; _c_yellow=; _c_red=; _c_bold=
fi

say()     { printf '%s==>%s %s\n' "${_c_blue}${_c_bold}" "${_c_reset}" "$*"; }
ok()      { printf '%s✅ %s%s\n' "${_c_green}" "$*" "${_c_reset}"; }
warn()    { printf '%s⚠️  %s%s\n' "${_c_yellow}" "$*" "${_c_reset}"; }
die()     { printf '%s❌ %s%s\n' "${_c_red}" "$*" "${_c_reset}" >&2; }
pass()    { printf '%s✅ ok%s   - %s\n' "${_c_green}" "${_c_reset}" "$*"; }
failmsg() { printf '%s❌ FAIL%s - %s\n' "${_c_red}" "${_c_reset}" "$*"; }
