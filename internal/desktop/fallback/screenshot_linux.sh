#!/bin/sh
set -eu
path="$1"
if command -v gnome-screenshot >/dev/null 2>&1; then exec gnome-screenshot -f "$path"; fi
if command -v import >/dev/null 2>&1; then exec import -window root "$path"; fi
if command -v scrot >/dev/null 2>&1; then exec scrot "$path"; fi
echo "no screenshot command available" >&2
exit 127
