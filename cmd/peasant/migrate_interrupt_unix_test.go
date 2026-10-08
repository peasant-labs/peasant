//go:build unix

package main

import (
	"os"
	"syscall"
	"testing"
)

func TestMigrateSIGINTPartialReport(t *testing.T) {
	for _, c := range loadMigrateInterruptFixtures(t) {
		if c.Mode != "sigint" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			runMigrateInterruptCase(t, c, func() error { return syscall.Kill(os.Getpid(), syscall.SIGINT) })
		})
	}
}
