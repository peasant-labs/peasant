package ingest

import "github.com/peasant-labs/schema"

func fullEntryWriteBytes(entries []schema.SessionEntry) int64 {
	var total int64
	for _, entry := range entries {
		for _, value := range []*string{entry.ContentPreview, entry.ToolInput, entry.ToolOutput, entry.Extra, entry.EntryID, entry.ParentEntryID, entry.ToolCallID, entry.ToolNamesCSV, entry.PartType} {
			if value != nil {
				total += int64(len(*value))
			}
		}
	}
	return total
}
