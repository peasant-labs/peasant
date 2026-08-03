package defaults

import "github.com/peasant-labs/schema"

// JSONFlagName is the CLI flag name for JSON output mode.
// Uses string(schema.SourceFormatJSON) to avoid a bare "json" literal that
// triggers the no-bare-format-literals ast-grep rule.
const JSONFlagName = string(schema.SourceFormatJSON)
