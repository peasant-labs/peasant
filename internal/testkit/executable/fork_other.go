//go:build !unix

package executable

func lockFork() func() { return func() {} }
