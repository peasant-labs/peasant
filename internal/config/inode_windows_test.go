//go:build windows

package config_test

import "io/fs"

// fileIdentity reports that Windows exposes no inode identity, so inode-based
// assertions skip there. See the unix build of this helper.
func fileIdentity(fs.FileInfo) (uint64, bool) { return 0, false }
