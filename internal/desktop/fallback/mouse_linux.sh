#!/bin/sh
set -eu
command -v xdotool >/dev/null 2>&1 || { echo "xdotool unavailable" >&2; exit 127; }
case "$1" in
  move)
    if [ "$4" = "true" ]; then exec xdotool mousemove_relative -- "$2" "$3"; fi
    exec xdotool mousemove "$2" "$3" ;;
  button)
    case "$2" in
      left) button=1 ;;
      right) button=3 ;;
      middle) button=2 ;;
      *) exit 2 ;;
    esac
    case "$3" in
      down) exec xdotool mousedown "$button" ;;
      up) exec xdotool mouseup "$button" ;;
      *) exec xdotool click "$button" ;;
    esac ;;
  scroll)
    if [ "$2" != "0" ]; then exec xdotool click 6; fi
    if [ "$3" -gt 0 ]; then exec xdotool click 5; else exec xdotool click 4; fi ;;
esac
exit 2
