#!/bin/sh
# Stands in for peasant: appends each argument on its own line to
# invocations.log and exits with the status read from exit-code, both beside
# the link it was started through. Shell builtins only: hooks run it on a PATH
# that names no system directory.
dir=${0%/*}
printf '%s\n' "$@" >> "$dir/invocations.log"
read -r code < "$dir/exit-code"
exit "$code"
