package push

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// publishImages rewrites a detail into the published image shape and strips
// local image fields from its entries. Text parts stay. Image parts become
// {type:image, omittedReason:not_published}. Bytes, refs, media, size, and
// dimensions are removed from entries, extras, parts, earlier history, and
// retained unknown payloads.
func publishImages(detail *schema.SessionDetailPayload, entries []schema.SessionEntry) []schema.SessionEntry {
	if detail != nil {
		publishDetailImages(detail)
	}
	if len(entries) == 0 {
		return entries
	}
	out := make([]schema.SessionEntry, len(entries))
	for i, entry := range entries {
		entry.Extra = ingest.StripPublishedImageExtra(entry.Extra)
		entry.ContentPreview = stripRecordedSizePreview(entry.ContentPreview)
		out[i] = entry
	}
	return out
}

// cloneSessionDetail copies the slices publication rewrites so the caller's
// detail is left unchanged. Scalar fields are copied by the struct assignment.
func cloneSessionDetail(in *schema.SessionDetailPayload) schema.SessionDetailPayload {
	if in == nil {
		return schema.SessionDetailPayload{}
	}
	detail := *in
	detail.Turns = cloneTurns(in.Turns)
	if in.EarlierHistory != nil {
		detail.EarlierHistory = make([]schema.EarlierHistorySection, len(in.EarlierHistory))
		for i, section := range in.EarlierHistory {
			section.Turns = cloneTurns(section.Turns)
			detail.EarlierHistory[i] = section
		}
	}
	if in.RetainedUnknown != nil {
		detail.RetainedUnknown = append([]schema.RetainedUnknownRecord(nil), in.RetainedUnknown...)
	}
	return detail
}

func cloneTurns(turns []schema.TurnDetail) []schema.TurnDetail {
	if turns == nil {
		return nil
	}
	out := make([]schema.TurnDetail, len(turns))
	for i, turn := range turns {
		if turn.ContentParts != nil {
			turn.ContentParts = append([]schema.TurnContentPart(nil), turn.ContentParts...)
			for j, part := range turn.ContentParts {
				if part.Image != nil {
					copied := *part.Image
					turn.ContentParts[j].Image = &copied
				}
			}
		}
		out[i] = turn
	}
	return out
}

func stripRecordedSizePreview(text *string) *string {
	if text == nil || !strings.HasPrefix(*text, "image omitted:") {
		return text
	}
	return nil
}

func publishDetailImages(detail *schema.SessionDetailPayload) {
	publishTurns(detail.Turns)
	for i := range detail.EarlierHistory {
		publishTurns(detail.EarlierHistory[i].Turns)
	}
	for i := range detail.RetainedUnknown {
		detail.RetainedUnknown[i].Payload = stripRetainedImagePayload(detail.RetainedUnknown[i].Payload)
	}
}

func publishTurns(turns []schema.TurnDetail) {
	for i := range turns {
		if strings.HasPrefix(turns[i].Content, "image omitted:") {
			turns[i].Content = ""
		}
		if len(turns[i].ContentParts) == 0 {
			continue
		}
		next := make([]schema.TurnContentPart, len(turns[i].ContentParts))
		for j, part := range turns[i].ContentParts {
			if part.Type != schema.TurnPartTypeImage {
				if part.Type == schema.TurnPartTypeText && strings.HasPrefix(part.Text, "image omitted:") {
					part.Text = ""
				}
				next[j] = part
				continue
			}
			next[j] = schema.TurnContentPart{
				Type:  schema.TurnPartTypeImage,
				Image: &schema.ImagePart{OmittedReason: schema.OmittedReasonNotPublished},
			}
		}
		turns[i].ContentParts = next
	}
}

func stripRetainedImagePayload(raw string) string {
	if raw == "" {
		return raw
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return raw
	}
	if !imageValuePresent(value) {
		return raw
	}
	stripped := stripImageValue(value)
	encoded, err := json.Marshal(stripped)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func imageValuePresent(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if imageSource(typed) || imageDescriptorMap(typed) {
			return true
		}
		for _, child := range typed {
			if imageValuePresent(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if imageValuePresent(child) {
				return true
			}
		}
	}
	return false
}

func stripImageValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		if imageSource(typed) {
			delete(typed, "data")
		}
		if imageDescriptorMap(typed) {
			for _, key := range []string{"ref", "mediaType", "media_type", "byteSize", "width", "height", "recordedSize", "data"} {
				delete(typed, key)
			}
		}
		for key, child := range typed {
			typed[key] = stripImageValue(child)
		}
		return typed
	case []any:
		for i, child := range typed {
			typed[i] = stripImageValue(child)
		}
		return typed
	default:
		return value
	}
}

func imageSource(fields map[string]any) bool {
	data, _ := fields["data"].(string)
	if data == "" {
		return false
	}
	media, _ := fields["media_type"].(string)
	if media == "" {
		media, _ = fields["mediaType"].(string)
	}
	return strings.HasPrefix(strings.ToLower(media), "image/")
}

func imageDescriptorMap(fields map[string]any) bool {
	if reason, _ := fields["omittedReason"].(string); reason != "" {
		return true
	}
	ref, _ := fields["ref"].(string)
	return len(ref) == 64
}
