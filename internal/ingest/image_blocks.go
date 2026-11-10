package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/attachments"
	"github.com/peasant-labs/schema"
)

const imagePartType = "image"

// imageDescriptor is the local metadata stored on an image entry. Bytes are
// never included. A stored image names its content address; an omitted image
// names why. RecordedSize is the decoded size of an omitted image so a
// placeholder can state the size without putting that size on the wire part.
type imageDescriptor struct {
	Ref           string `json:"ref,omitempty"`
	MediaType     string `json:"mediaType,omitempty"`
	ByteSize      int    `json:"byteSize,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	OmittedReason string `json:"omittedReason,omitempty"`
	RecordedSize  int    `json:"recordedSize,omitempty"`
}

// extractedImage is one decoded image source ready to persist.
type extractedImage struct {
	Row          attachments.Row
	Bytes        []byte
	RecordedSize int
}

// ImagePersister stores one extracted image during indexing. A nil persister
// still emits the entry; the file is written when a store is attached.
type ImagePersister interface {
	PersistImage(ctx context.Context, pending attachments.Pending) error
}

type imagePersistKey struct{}

// WithImagePersister returns a context the Claude indexer reads while parsing.
func WithImagePersister(ctx context.Context, persister ImagePersister) context.Context {
	return context.WithValue(ctx, imagePersistKey{}, persister)
}

func imagePersisterFrom(ctx context.Context) ImagePersister {
	if ctx == nil {
		return nil
	}
	persister, _ := ctx.Value(imagePersistKey{}).(ImagePersister)
	return persister
}

func decodeImageDescriptor(extra *string) (imageDescriptor, bool) {
	if extra == nil || *extra == "" {
		return imageDescriptor{}, false
	}
	var probe struct {
		Ref           string `json:"ref"`
		OmittedReason string `json:"omittedReason"`
	}
	if json.Unmarshal([]byte(*extra), &probe) != nil || (probe.Ref == "" && probe.OmittedReason == "") {
		return imageDescriptor{}, false
	}
	var desc imageDescriptor
	if json.Unmarshal([]byte(*extra), &desc) != nil {
		return imageDescriptor{}, false
	}
	return desc, true
}

func (d imageDescriptor) encode() (*string, error) {
	encoded, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("encode image descriptor: %w", err)
	}
	text := string(encoded)
	return &text, nil
}

func (d imageDescriptor) part() schema.ImagePart {
	part := schema.ImagePart{
		Ref:           d.Ref,
		MediaType:     schema.ImageMediaType(d.MediaType),
		ByteSize:      d.ByteSize,
		Width:         d.Width,
		Height:        d.Height,
		OmittedReason: schema.OmittedReason(d.OmittedReason),
	}
	if part.OmittedReason == schema.OmittedReasonTooLarge || part.OmittedReason == schema.OmittedReasonUnsupported || part.OmittedReason == schema.OmittedReasonMissing {
		part.ByteSize = 0
	}
	if part.OmittedReason == schema.OmittedReasonUnsupported {
		part.MediaType = ""
		part.Ref = ""
	}
	if part.OmittedReason == schema.OmittedReasonTooLarge {
		part.Ref = ""
	}
	return part
}

func imagePlaceholder(desc imageDescriptor) string {
	switch schema.OmittedReason(desc.OmittedReason) {
	case schema.OmittedReasonTooLarge:
		size := desc.RecordedSize
		if size == 0 {
			size = desc.ByteSize
		}
		return fmt.Sprintf("image omitted: %d bytes exceeds the %d byte limit", size, attachments.MaxImageBytes)
	case schema.OmittedReasonUnsupported:
		return "image omitted: this build records png and jpeg only"
	case schema.OmittedReasonMissing:
		return "image omitted: the stored file is missing"
	default:
		return ""
	}
}

func cleanBase64(data string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, data)
}

// classifyImageSource decodes one base64 image source and classifies it.
// Unsupported wins over too-large. The returned bytes are nil when the image
// is not stored.
func classifyImageSource(mediaType, data string) extractedImage {
	decoded, err := base64.StdEncoding.DecodeString(cleanBase64(data))
	if err != nil {
		return extractedImage{Row: attachments.Row{OmittedReason: string(schema.OmittedReasonUnsupported)}}
	}
	row, payload := attachments.Classify(mediaType, decoded)
	image := extractedImage{Row: row, Bytes: payload, RecordedSize: len(decoded)}
	if row.OmittedReason != "" {
		image.Bytes = nil
		image.Row.ByteSize = 0
		if row.OmittedReason == string(schema.OmittedReasonTooLarge) {
			image.Row.MediaType = row.MediaType
			image.Row.Width = row.Width
			image.Row.Height = row.Height
		}
	}
	return image
}

func (image extractedImage) descriptor(sha string) imageDescriptor {
	desc := imageDescriptor{
		MediaType:     image.Row.MediaType,
		Width:         image.Row.Width,
		Height:        image.Row.Height,
		OmittedReason: image.Row.OmittedReason,
		RecordedSize:  image.RecordedSize,
	}
	if image.Row.OmittedReason == "" {
		desc.Ref = sha
		desc.ByteSize = image.Row.ByteSize
		desc.RecordedSize = 0
	}
	if image.Row.OmittedReason == string(schema.OmittedReasonUnsupported) {
		desc.MediaType = ""
		desc.Width = 0
		desc.Height = 0
	}
	return desc
}

// imageSourceFields is the nested or flat source object an image block carries.
type imageSourceFields struct {
	MediaType string
	Data      string
	OK        bool
}

func imageSourceFromRaw(raw json.RawMessage) imageSourceFields {
	if len(raw) == 0 {
		return imageSourceFields{}
	}
	var source map[string]json.RawMessage
	if json.Unmarshal(raw, &source) != nil {
		return imageSourceFields{}
	}
	data := jsonString(source, "data")
	if data == "" {
		return imageSourceFields{}
	}
	media := jsonString(source, "media_type")
	if media == "" {
		media = jsonString(source, "mediaType")
	}
	if media == "" || !strings.HasPrefix(strings.ToLower(media), "image/") {
		return imageSourceFields{}
	}
	return imageSourceFields{MediaType: media, Data: data, OK: true}
}

func imageSourceFromFields(fields map[string]json.RawMessage) imageSourceFields {
	raw, ok := fields["source"]
	if !ok {
		return imageSourceFields{}
	}
	var source map[string]json.RawMessage
	if json.Unmarshal(raw, &source) != nil {
		return imageSourceFields{}
	}
	data := jsonString(source, "data")
	if data == "" {
		return imageSourceFields{}
	}
	media := jsonString(source, "media_type")
	if media == "" {
		media = jsonString(source, "mediaType")
	}
	if media == "" || !strings.HasPrefix(strings.ToLower(media), "image/") {
		return imageSourceFields{}
	}
	return imageSourceFields{MediaType: media, Data: data, OK: true}
}

func jsonString(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// contentImages returns image sources from a message content array, in order.
func contentImages(raw json.RawMessage) []imageSourceFields {
	if len(raw) == 0 {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var found []imageSourceFields
	for _, block := range blocks {
		if jsonString(block, "type") != imagePartType {
			continue
		}
		if source := imageSourceFromFields(block); source.OK {
			found = append(found, source)
		}
	}
	return found
}

// attachmentImages returns image sources from an attachment record, in order.
func attachmentImages(raw json.RawMessage) []imageSourceFields {
	if len(raw) == 0 {
		return nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	if nested, ok := payload["attachment"]; ok {
		var attachment map[string]json.RawMessage
		if json.Unmarshal(nested, &attachment) == nil {
			if source := imageSourceFromFields(attachment); source.OK {
				return []imageSourceFields{source}
			}
		}
	}
	if source := imageSourceFromFields(payload); source.OK {
		return []imageSourceFields{source}
	}
	return nil
}

func claudeLineImages(line claudeIndexLine) (content []imageSourceFields, attached []imageSourceFields) {
	raw := line.Message.Content
	if len(raw) == 0 {
		raw = line.Content
	}
	return contentImages(raw), attachmentImages(line.Attachment)
}

func (idx *ClaudeIndexer) persistImage(ctx context.Context, sessionID SessionID, sourceRef string, partition string, image extractedImage, sha string) error {
	persister := imagePersisterFrom(ctx)
	if persister == nil {
		return nil
	}
	row := image.Row
	row.SessionID = string(sessionID)
	row.SourceEntryRef = sourceRef
	row.PartitionID = partition
	if image.Row.OmittedReason == "" {
		row.SHA256 = sha
		row.ByteSize = image.Row.ByteSize
	} else if image.Row.OmittedReason == string(schema.OmittedReasonTooLarge) {
		row.ByteSize = image.RecordedSize
	}
	pending := attachments.Pending{Row: row, Bytes: image.Bytes}
	if err := persister.PersistImage(ctx, pending); err != nil {
		return fmt.Errorf("claude indexer: persist image for session %s ref %s: %w", sessionID, sourceRef, err)
	}
	return nil
}

func contentAddress(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func claudeLineHasImage(line claudeIndexLine) bool {
	content, attached := claudeLineImages(line)
	return len(content) > 0 || len(attached) > 0
}

// imageChildEntry builds one depth-1 image entry and persists its bytes when a
// persister is attached. The entry Extra carries the descriptor, never the bytes.
func (idx *ClaudeIndexer) imageChildEntry(ctx context.Context, sessionID SessionID, entryIndex, parentIndex int, role Role, partition string, source imageSourceFields) (schema.SessionEntry, error) {
	image := classifyImageSource(source.MediaType, source.Data)
	ref, err := imageSourceRef(sessionID, entryIndex)
	if err != nil {
		return schema.SessionEntry{}, err
	}
	var sha string
	if len(image.Bytes) > 0 {
		sha = contentAddress(image.Bytes)
	}
	if err := idx.persistImage(ctx, sessionID, string(ref), partition, image, sha); err != nil {
		return schema.SessionEntry{}, err
	}
	desc := image.descriptor(sha)
	extra, err := desc.encode()
	if err != nil {
		return schema.SessionEntry{}, err
	}
	part := imagePartType
	entry := schema.SessionEntry{
		SessionID:      sessionID,
		EntryIndex:     entryIndex,
		Harness:        HarnessClaudeCode,
		Role:           role,
		EntryType:      schema.EntryTypeImage,
		Depth:          1,
		ParentIndex:    &parentIndex,
		PartType:       &part,
		SourceEntryRef: ref,
		Extra:          extra,
	}
	if note := imagePlaceholder(desc); note != "" {
		entry.ContentPreview = &note
	}
	return entry, nil
}

// imagePartsByParent folds depth-1 text and image children of an image-bearing
// parent into ordered turn parts. Text-only parents are left alone so their
// existing carrier-plus-skip behavior is unchanged.
func imagePartsByParent(entries []schema.SessionEntry) map[int][]TurnPart {
	grouped := map[int][]schema.SessionEntry{}
	hasImage := map[int]bool{}
	for _, entry := range entries {
		if entry.Depth != 1 || entry.ParentIndex == nil {
			continue
		}
		if entry.EntryType == schema.EntryTypeImage {
			hasImage[*entry.ParentIndex] = true
			grouped[*entry.ParentIndex] = append(grouped[*entry.ParentIndex], entry)
			continue
		}
		if entry.EntryType == schema.EntryTypeText && entry.PartType != nil && *entry.PartType == "text" {
			grouped[*entry.ParentIndex] = append(grouped[*entry.ParentIndex], entry)
		}
	}
	out := map[int][]TurnPart{}
	for parent, children := range grouped {
		if !hasImage[parent] {
			continue
		}
		parts := make([]TurnPart, 0, len(children))
		for _, child := range children {
			if child.EntryType == schema.EntryTypeImage {
				desc, ok := decodeImageDescriptor(child.Extra)
				if !ok {
					desc = imageDescriptor{OmittedReason: string(schema.OmittedReasonUnsupported)}
				}
				part := desc.part()
				note := ""
				if child.ContentPreview != nil {
					note = *child.ContentPreview
				}
				parts = append(parts, TurnPart{Kind: TurnPartImage, Text: note, ImageRef: part.Ref, Image: &part})
				continue
			}
			text := ""
			if child.ContentPreview != nil {
				text = *child.ContentPreview
			}
			parts = append(parts, TurnPart{Kind: TurnPartText, Text: text})
		}
		out[parent] = parts
	}
	return out
}

func joinedPartText(parts []TurnPart) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Kind == TurnPartText && part.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(part.Text)
		}
		if part.Kind == TurnPartImage && part.Text != "" && b.Len() == 0 {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// StripPublishedImageExtra removes a local image descriptor from an entry Extra.
// A control-record extra that only records a stripped byte size is left alone.
func StripPublishedImageExtra(extra *string) *string {
	if _, ok := decodeImageDescriptor(extra); !ok {
		return extra
	}
	return nil
}

func imageSourceRef(sessionID SessionID, entryIndex int) (schema.SourceEntryRef, error) {
	raw := fmt.Sprintf("img-%s-%d", sessionID, entryIndex)
	if len(raw) > 96 {
		raw = fmt.Sprintf("img-%d", entryIndex)
	}
	ref, err := schema.NewSourceEntryRef(raw)
	if err != nil {
		return "", fmt.Errorf("claude indexer: image source ref for session %s entry %d is not a public ref: %w", sessionID, entryIndex, err)
	}
	return ref, nil
}
