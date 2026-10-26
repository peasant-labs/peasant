//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestBackgroundProcAttrDetachesOnWindows pins the Windows process attributes:
// CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS, so the backgrounded server has
// no console to lend its children.
func TestBackgroundProcAttrDetachesOnWindows(t *testing.T) {
	got := backgroundProcAttr()
	want := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	if got.CreationFlags != want {
		t.Fatalf("backgroundProcAttr() CreationFlags = %#x, want %#x", got.CreationFlags, want)
	}
	if got.HideWindow || got.NoInheritHandles {
		t.Fatalf("backgroundProcAttr() = %#v, want only the detach creation flags", got)
	}
}
