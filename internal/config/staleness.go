package config

import (
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// StalenessWindow is the window inside which a source file counts as still
// being written. Ingest, a re-scan and the setup scan read it the same way, so
// they agree on which sessions are active. It is the configured
// output.stalenessThresholdSec, or the packaged default when that is unset (zero)
// or the configuration is nil.
func (c *Config) StalenessWindow() time.Duration {
	if c == nil || c.Output.StalenessThresholdSec == 0 {
		return time.Duration(defaults.ConfigStalenessThresholdSec) * time.Second
	}
	return time.Duration(c.Output.StalenessThresholdSec) * time.Second
}
