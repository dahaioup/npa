#!/bin/sh
set -eu
screen="$1"
path="$2"
if [ -z "$screen" ] || [ "$screen" = "primary" ]; then
  exec screencapture -x "$path"
fi
exec screencapture -D "$screen" -x "$path"
