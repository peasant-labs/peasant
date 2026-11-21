#!/bin/sh
# A program that is not peasant, started by a test with the server's trailing
# arguments (`sh program_with_server_args.sh web start --foreground --port P`).
# It starts no child and ends when its standard input closes.
read -r _
