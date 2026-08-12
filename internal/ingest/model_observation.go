package ingest

import "strings"

func validObservedModel(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}
