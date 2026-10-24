package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func testCredentialLockDescriptors(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential.lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := tryCredentialLock(first.Fd()); err != nil {
		t.Fatal(err)
	}
	if err := tryCredentialLock(second.Fd()); !credentialLockBusy(err) {
		t.Fatalf("independent descriptor acquired a held lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tryCredentialLock(second.Fd()); err != nil {
		t.Fatalf("closed descriptor retained lock: %v", err)
	}
}
