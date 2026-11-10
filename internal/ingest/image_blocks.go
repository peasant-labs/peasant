package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

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

// extractedImage is one decoded image source ready to stage. Bytes are nil
// when the image is omitted. Identity is the content address of the decoded
// bytes, or of the raw input when the bytes do not decode.
type extractedImage struct {
	Row          attachments.Row
	Bytes        []byte
	RecordedSize int
	Identity     string
}

// ImageFactReader joins session_attachments for one session. A reader that
// cannot do this leaves image media unset.
type ImageFactReader interface {
	ImageFacts(ctx context.Context, sessionID SessionID) (map[string]ImageFacts, error)
}

// ImageFacts is the attachment-row half of one image. Projection joins it by
// SourceRef. The entry extra is not a second copy of media, size, or dimensions.
type ImageFacts struct {
	SourceRef     string
	SHA256        string
	MediaType     string
	ByteSize      int
	Width         int
	Height        int
	OmittedReason string
	PartitionID   string
}

// imageStager collects images during parse. It does not write rows. Activation
// installs the batch only after the generation is accepted.
type imageStager interface {
	StageImage(pending attachments.Pending)
}

type imageStageKey struct{}

// imageBatch is the per-parse collector. A rejected, held, or skipped attempt
// drops the batch, so it cannot overwrite the active generation's rows.
type imageBatch struct {
	mu      sync.Mutex
	pending []attachments.Pending
}

func (b *imageBatch) StageImage(pending attachments.Pending) {
	if b == nil {
		return
	}
	copied := pending
	if pending.Bytes != nil {
		copied.Bytes = append([]byte(nil), pending.Bytes...)
	}
	b.mu.Lock()
	b.pending = append(b.pending, copied)
	b.mu.Unlock()
}

func (b *imageBatch) snapshot() []attachments.Pending {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]attachments.Pending, len(b.pending))
	copy(out, b.pending)
	return out
}

func withImageStaging(ctx context.Context, stager imageStager) context.Context {
	return context.WithValue(ctx, imageStageKey{}, stager)
}

func stageImage(ctx context.Context, pending attachments.Pending) {
	if ctx == nil {
		return
	}
	stager, _ := ctx.Value(imageStageKey{}).(imageStager)
	if stager == nil {
		return
	}
	stager.StageImage(pending)
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
		return extractedImage{
			Row:      attachments.Row{OmittedReason: string(schema.OmittedReasonUnsupported)},
			Identity: contentAddress([]byte(data)),
		}
	}
	row, payload := attachments.Classify(mediaType, decoded)
	image := extractedImage{Row: row, Bytes: payload, RecordedSize: len(decoded), Identity: contentAddress(decoded)}
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

// claudeLinePartition is the partition known while parsing one Claude line.
// The JSONL record does not name a generation partition. Activation stamps the
// partition from the generation layout (main or the earlier section), so the
// parse does not invent "main" for a block that may land in earlier history.
func claudeLinePartition(_ claudeIndexLine) string {
	return ""
}

func claudeLineImages(line claudeIndexLine) (content []imageSourceFields, attached []imageSourceFields) {
	raw := line.Message.Content
	if len(raw) == 0 {
		raw = line.Content
	}
	return contentImages(raw), attachmentImages(line.Attachment)
}

func stageExtractedImage(ctx context.Context, sessionID SessionID, sourceRef, partition string, image extractedImage) {
	row := image.Row
	row.SessionID = string(sessionID)
	row.SourceEntryRef = sourceRef
	row.PartitionID = partition
	if image.Row.OmittedReason == "" {
		row.ByteSize = image.Row.ByteSize
	} else if image.Row.OmittedReason == string(schema.OmittedReasonTooLarge) {
		row.ByteSize = image.RecordedSize
	}
	stageImage(ctx, attachments.Pending{Row: row, Bytes: image.Bytes})
}

func contentAddress(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func claudeLineHasImage(line claudeIndexLine) bool {
	content, attached := claudeLineImages(line)
	return len(content) > 0 || len(attached) > 0
}

// imageChildEntry builds one depth-1 image entry and stages its bytes. The
// entry extra keeps only the content-addressed ref. Media, size, and dimensions
// are written to session_attachments when the generation is activated, and the
// projection joins that row. A parse that is later held or rejected writes no row.
func (idx *ClaudeIndexer) imageChildEntry(ctx context.Context, sessionID SessionID, entryIndex, parentIndex int, role Role, partition string, source imageSourceFields) (schema.SessionEntry, error) {
	image := classifyImageSource(source.MediaType, source.Data)
	identity := image.Identity
	if identity == "" {
		identity = contentAddress([]byte(source.Data))
	}
	ref, err := imageSourceRef(identity)
	if err != nil {
		return schema.SessionEntry{}, err
	}
	stageExtractedImage(ctx, sessionID, string(ref), partition, image)
	extra, err := imageRefExtra(string(ref))
	if err != nil {
		return schema.SessionEntry{}, err
	}
	desc := image.descriptor(identity)
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
// existing carrier-plus-skip behavior is unchanged. facts is the attachment-row
// join; nil leaves media and the omission reason unset. folded is the set of
// child indexes consumed by the fold, so the turn projection does not keep a
// second copy of that child rule.
func imagePartsByParent(entries []schema.SessionEntry, facts map[string]ImageFacts) (map[int][]TurnPart, map[int]struct{}) {
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
	folded := map[int]struct{}{}
	for parent, children := range grouped {
		if !hasImage[parent] {
			continue
		}
		parts := make([]TurnPart, 0, len(children))
		for _, child := range children {
			folded[child.EntryIndex] = struct{}{}
			if child.EntryType == schema.EntryTypeImage {
				part := imagePartFromJoin(child, facts)
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
	return out, folded
}

func imagePartFromJoin(child schema.SessionEntry, facts map[string]ImageFacts) schema.ImagePart {
	sourceRef := imageExtraRef(child.Extra)
	fact, ok := facts[sourceRef]
	if !ok {
		// No row was joined. The part stays a valid local omission and carries
		// no media, size, or content address. Publication replaces it with
		// not_published. A mounted read joins the row before this fold.
		return schema.ImagePart{OmittedReason: schema.OmittedReasonUnsupported}
	}
	desc := imageDescriptor{
		Ref:           fact.SHA256,
		MediaType:     fact.MediaType,
		ByteSize:      fact.ByteSize,
		Width:         fact.Width,
		Height:        fact.Height,
		OmittedReason: fact.OmittedReason,
	}
	if fact.OmittedReason == "" {
		desc.Ref = fact.SHA256
		desc.ByteSize = fact.ByteSize
	}
	return desc.part()
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

func imageRefExtra(ref string) (*string, error) {
	encoded, err := json.Marshal(struct {
		Ref string `json:"ref"`
	}{Ref: ref})
	if err != nil {
		return nil, fmt.Errorf("encode image ref: %w", err)
	}
	text := string(encoded)
	return &text, nil
}

func imageExtraRef(extra *string) string {
	desc, ok := decodeImageDescriptor(extra)
	if !ok {
		return ""
	}
	return desc.Ref
}

// imageSourceRef is the content-addressed source ref img-<sha256>. The same
// bytes share one row. A different image, including one at the same entry index
// in another partition or a later rejected attempt, gets a different ref and
// cannot overwrite this row.
// assignImagePartitions stamps each staged image with the partition the
// generation layout actually gave its ref. A format-1 write has one relational
// partition, named main because that is the only entry table. A generation
// block keeps the layout's partition, including an earlier-history section, so
// two images at the same entry index in different partitions stay distinct rows.
func assignImagePartitions(candidate *GenerationCandidate, pending []attachments.Pending) {
	byRef := map[string]string{}
	if candidate != nil {
		stamp := func(entries []schema.SessionEntry, partition string) {
			for _, entry := range entries {
				if ref := imageExtraRef(entry.Extra); ref != "" {
					byRef[ref] = partition
				}
			}
		}
		stamp(candidate.Result.Generation.Main.Entries, "main")
		for i, section := range candidate.Result.Generation.Earlier {
			partition := string(section.State)
			if partition == "" {
				partition = fmt.Sprintf("earlier/%d", i)
			}
			stamp(section.Content.Entries, partition)
		}
	}
	for i := range pending {
		if part, ok := byRef[pending[i].Row.SourceEntryRef]; ok {
			pending[i].Row.PartitionID = part
			continue
		}
		if pending[i].Row.PartitionID == "" && candidate == nil {
			pending[i].Row.PartitionID = "main"
		}
	}
}

func imageSourceRef(identity string) (schema.SourceEntryRef, error) {
	raw := "img-" + identity
	ref, err := schema.NewSourceEntryRef(raw)
	if err != nil {
		return "", fmt.Errorf("claude indexer: image source ref %q is not a public ref: %w", raw, err)
	}
	return ref, nil
}
