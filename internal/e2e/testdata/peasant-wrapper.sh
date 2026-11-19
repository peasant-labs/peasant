#!/bin/sh
# Runs the PEASANT_BIN command under the name peasant. Linked into a directory
# as peasant beside peasant.argv, which holds the command's words one per line;
# the caller's arguments follow them.
count=$#
while IFS= read -r word; do
	set -- "$@" "$word"
done < "${0%/*}/peasant.argv"
while [ "$count" -gt 0 ]; do
	set -- "$@" "$1"
	shift
	count=$((count - 1))
done
exec "$@"
