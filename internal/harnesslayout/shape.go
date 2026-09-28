package harnesslayout

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// JSONType is the JSON type of one observed value.
type JSONType string

const (
	JSONObject JSONType = "object"
	JSONArray  JSONType = "array"
	JSONString JSONType = "string"
	JSONNumber JSONType = "number"
	JSONBool   JSONType = "boolean"
	JSONNull   JSONType = "null"
)

// MaxFieldPaths bounds the distinct field paths one artifact records, so a
// map keyed by identifiers cannot grow a shape without limit.
const MaxFieldPaths = 1024

// maxJSONLLine bounds one JSONL record read by ShapeJSONL.
const maxJSONLLine = 64 << 20

// FieldShape is one field path with the JSON types seen at it. Path uses
// "$" for the record, ".name" for an object key, and "[]" for array elements.
type FieldShape struct {
	Path  string     `json:"path"`
	Types []JSONType `json:"types"`
	Count int        `json:"count"`
}

// ArtifactShape is the value-free structure of one artifact.
type ArtifactShape struct {
	Artifact string `json:"artifact"`
	Path     string `json:"path,omitempty"`
	Format   Format `json:"format"`
	Role     Role   `json:"role"`
	Records  int    `json:"records"`
	// Kinds counts records by the kind the probe assigned, such as a JSONL
	// "type" field or a message role.
	Kinds map[string]int `json:"kinds,omitempty"`
	// Fields is ordered by path.
	Fields []FieldShape `json:"fields"`
	// Truncated reports that MaxFieldPaths was reached.
	Truncated bool `json:"truncated,omitempty"`
	// Malformed counts records that did not decode.
	Malformed int `json:"malformed,omitempty"`
}

// ShapeRecorder accumulates an ArtifactShape. The zero value is not usable;
// call NewShapeRecorder.
type ShapeRecorder struct {
	shape  ArtifactShape
	fields map[string]*fieldAcc
}

type fieldAcc struct {
	types map[JSONType]struct{}
	count int
}

// NewShapeRecorder starts the shape of one artifact.
func NewShapeRecorder(artifact Artifact, path string) *ShapeRecorder {
	return &ShapeRecorder{
		shape: ArtifactShape{
			Artifact: artifact.Name,
			Path:     path,
			Format:   artifact.Format,
			Role:     artifact.Role,
			Kinds:    map[string]int{},
		},
		fields: map[string]*fieldAcc{},
	}
}

// AddRecord records one decoded JSON record and its kind. An empty kind is
// not counted.
func (r *ShapeRecorder) AddRecord(kind string, value any) {
	r.shape.Records++
	if kind != "" {
		r.shape.Kinds[kind]++
	}
	r.walk("$", value)
}

// AddRaw decodes one JSON record and records it. A record that does not
// decode counts as malformed and returns the decode error.
func (r *ShapeRecorder) AddRaw(kind string, raw []byte) error {
	value, err := decodeValue(raw)
	if err != nil {
		r.shape.Malformed++
		return err
	}
	r.AddRecord(kind, value)
	return nil
}

// AddField records one named column or attribute that is not JSON, such as
// a SQLite column, under the given path.
func (r *ShapeRecorder) AddField(path string, typ JSONType) {
	r.observe(path, typ)
}

// Shape returns the accumulated shape.
func (r *ShapeRecorder) Shape() ArtifactShape {
	out := r.shape
	out.Kinds = make(map[string]int, len(r.shape.Kinds))
	for k, v := range r.shape.Kinds {
		out.Kinds[k] = v
	}
	if len(out.Kinds) == 0 {
		out.Kinds = nil
	}
	out.Fields = make([]FieldShape, 0, len(r.fields))
	for path, acc := range r.fields {
		types := make([]JSONType, 0, len(acc.types))
		for t := range acc.types {
			types = append(types, t)
		}
		sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
		out.Fields = append(out.Fields, FieldShape{Path: path, Types: types, Count: acc.count})
	}
	sort.Slice(out.Fields, func(i, j int) bool { return out.Fields[i].Path < out.Fields[j].Path })
	return out
}

func (r *ShapeRecorder) walk(path string, value any) {
	switch v := value.(type) {
	case map[string]any:
		r.observe(path, JSONObject)
		for key, child := range v {
			r.walk(path+"."+key, child)
		}
	case []any:
		r.observe(path, JSONArray)
		for _, child := range v {
			r.walk(path+"[]", child)
		}
	case string:
		r.observe(path, JSONString)
	case json.Number, float64:
		r.observe(path, JSONNumber)
	case bool:
		r.observe(path, JSONBool)
	case nil:
		r.observe(path, JSONNull)
	}
}

func (r *ShapeRecorder) observe(path string, typ JSONType) {
	acc, ok := r.fields[path]
	if !ok {
		if len(r.fields) >= MaxFieldPaths {
			r.shape.Truncated = true
			return
		}
		acc = &fieldAcc{types: map[JSONType]struct{}{}}
		r.fields[path] = acc
	}
	acc.types[typ] = struct{}{}
	acc.count++
}

func decodeValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	return value, nil
}

// KindFunc names the kind of one decoded record. It returns "" when the
// record has no kind.
type KindFunc func(record any) string

// KindField returns a KindFunc that reads a top-level string field.
func KindField(name string) KindFunc {
	return func(record any) string {
		object, ok := record.(map[string]any)
		if !ok {
			return ""
		}
		kind, _ := object[name].(string)
		return kind
	}
}

// ShapeJSONL records every non-blank line of a JSONL stream and calls visit,
// when non-nil, with each decoded record. A malformed line is counted and
// skipped: a tool that is still writing leaves a partial final line.
func ShapeJSONL(r io.Reader, rec *ShapeRecorder, kind KindFunc, visit func(any)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxJSONLLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		value, err := decodeValue(line)
		if err != nil {
			rec.shape.Malformed++
			continue
		}
		name := ""
		if kind != nil {
			name = kind(value)
		}
		rec.AddRecord(name, value)
		if visit != nil {
			visit(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read JSONL: %w", err)
	}
	return nil
}

// ShapeJSON records the fields of one JSON document. When each is non-nil it
// selects the elements that count as records, such as the messages of a
// conversation array; otherwise the whole document is one record. It returns
// the decoded document for metadata extraction.
func ShapeJSON(r io.Reader, rec *ShapeRecorder, each func(document any) []any, kind KindFunc) (any, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read JSON: %w", err)
	}
	document, err := decodeValue(raw)
	if err != nil {
		rec.shape.Malformed++
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if each == nil {
		name := ""
		if kind != nil {
			name = kind(document)
		}
		rec.AddRecord(name, document)
		return document, nil
	}
	rec.walk("$", document)
	for _, record := range each(document) {
		rec.shape.Records++
		if kind != nil {
			if name := kind(record); name != "" {
				rec.shape.Kinds[name]++
			}
		}
	}
	return document, nil
}
