package onboarding

import "github.com/peasant-labs/peasant/internal/config"

// DeriveSelection exposes the production derivation to the external fixture tests.
func DeriveSelection(forest Forest, ids []string, autoIngest bool, residual Residual) (config.SelectionConfig, error) {
	return deriveSelection(forest, SelectedForest{IDs: ids}, autoIngest, residual)
}

// RestoreDocumentFormat exposes the formatting restore to its fixture test.
func RestoreDocumentFormat(original, encoded []byte) []byte {
	return restoreDocumentFormat(original, encoded)
}
