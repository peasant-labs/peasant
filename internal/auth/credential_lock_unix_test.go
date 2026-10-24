//go:build unix

package auth

import "testing"

func TestCredentialLockUnix(t *testing.T) { testCredentialLockDescriptors(t) }
