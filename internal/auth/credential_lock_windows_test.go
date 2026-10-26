//go:build windows

package auth

import "testing"

func TestCredentialLockWindows(t *testing.T) { testCredentialLockDescriptors(t) }
