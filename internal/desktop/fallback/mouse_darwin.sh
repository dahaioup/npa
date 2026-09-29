#!/bin/sh
set -eu
case "$1" in
  move) command -v cliclick >/dev/null 2>&1 && exec cliclick "m:$2,$3" ;;
  button) command -v cliclick >/dev/null 2>&1 && exec cliclick "${2:0:1}p" ;;
  scroll) command -v cliclick >/dev/null 2>&1 && exec cliclick "w:$2" ;;
esac
echo "mouse fallback command unavailable" >&2
exit 127
