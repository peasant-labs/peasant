package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/pflag"
)

// portFlag is an int flag that accepts only a TCP port a server can listen on
// and a client can reach (1-65535). Port 0 asks the OS for a random port, which
// `web stop` and `open` could then never find.
type portFlag struct{ port *int }

var _ pflag.Value = portFlag{}

// portFlagVar registers --port on flags, bound to p, with the range check.
func portFlagVar(flags *pflag.FlagSet, p *int, value int, usage string) {
	*p = value
	flags.Var(portFlag{port: p}, "port", usage)
}

func (f portFlag) String() string {
	if f.port == nil {
		return ""
	}
	return strconv.Itoa(*f.port)
}

func (f portFlag) Set(raw string) error {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%q is not a TCP port; use a number from 1-65535", raw)
	}
	*f.port = port
	return nil
}

func (portFlag) Type() string { return "int" }
