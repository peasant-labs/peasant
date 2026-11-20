//go:build !unix

package executable

// No ForkLock protection is needed outside Unix.
func lockFork() func() { return func() {} }
