//go:build !linux && !darwin

package ingest

import (
	"context"
	"fmt"
	"os"
)

const artifactLocksSupported = false
const artifactNonblockFlag = 0

func lockArtifactFile(context.Context, *os.File, ArtifactLockMode) error {
	return fmt.Errorf("managed artifact publication requires an OS advisory lock; this platform is unsupported; no artifact was changed")
}

func unlockArtifactFile(*os.File) error { return nil }
