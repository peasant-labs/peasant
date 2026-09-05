package ingest

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type openCodeV2ToolStatus string

const (
	openCodeV2ToolStreamingStatus openCodeV2ToolStatus = "streaming"
	openCodeV2ToolRunningStatus   openCodeV2ToolStatus = "running"
	openCodeV2ToolCompletedStatus openCodeV2ToolStatus = "completed"
	openCodeV2ToolErrorStatus     openCodeV2ToolStatus = "error"
)

type openCodeV2ShellStatus string

const (
	openCodeV2ShellRunning openCodeV2ShellStatus = "running"
	openCodeV2ShellExited  openCodeV2ShellStatus = "exited"
	openCodeV2ShellTimeout openCodeV2ShellStatus = "timeout"
	openCodeV2ShellKilled  openCodeV2ShellStatus = "killed"
)

type openCodeV2CompactionStatus string

const (
	openCodeV2CompactionRunning   openCodeV2CompactionStatus = "running"
	openCodeV2CompactionCompleted openCodeV2CompactionStatus = "completed"
	openCodeV2CompactionFailed    openCodeV2CompactionStatus = "failed"
)

type openCodeV2FileSourceType string

const (
	openCodeV2FileInline openCodeV2FileSourceType = "inline"
	openCodeV2FileURI    openCodeV2FileSourceType = "uri"
)

// Native V2 shapes follow packages/schema/src at upstream commit
// 52685d451777fbccc99736ab31ffc37de8475a45. They normalize into the existing
// managed representation; legacy payloads keep their original strict decoder.
type openCodeV2Mention struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type openCodeV2SkillAttachment struct {
	ID      string             `json:"id"`
	Name    string             `json:"name"`
	Text    string             `json:"text,omitempty"`
	Mention *openCodeV2Mention `json:"mention,omitempty"`
}

type openCodeV2Retry struct {
	Attempt int                          `json:"attempt"`
	At      int64                        `json:"at"`
	Error   *openCodeCurrentUnknownError `json:"error"`
}

type openCodeV2SkillMessage struct {
	openCodeCurrentBase
	Skill string `json:"skill"`
	Name  string `json:"name"`
	Text  string `json:"text"`
}

type openCodeV2File struct {
	Data   string `json:"data"`
	MIME   string `json:"mime"`
	Source struct {
		Type openCodeV2FileSourceType `json:"type"`
		URI  string                   `json:"uri,omitempty"`
	} `json:"source"`
	Name        string             `json:"name,omitempty"`
	Description string             `json:"description,omitempty"`
	Mention     *openCodeV2Mention `json:"mention,omitempty"`
}

func decodeOpenCodeV2File(raw json.RawMessage) (openCodeCurrentFile, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return openCodeCurrentFile{}, err
	}
	if _, native := fields["data"]; !native {
		var file openCodeCurrentFile
		err := decodeOpenCodeCurrentJSON(raw, &file)
		return file, err
	}
	var file openCodeV2File
	if err := decodeOpenCodeCurrentJSON(raw, &file); err != nil {
		return openCodeCurrentFile{}, err
	}
	if err := requireOpenCodeCurrentFields(raw, "data", "mime", "source"); err != nil {
		return openCodeCurrentFile{}, err
	}
	if _, err := base64.StdEncoding.Strict().DecodeString(file.Data); err != nil {
		return openCodeCurrentFile{}, fmt.Errorf("native file attachment data is not base64: %w", err)
	}
	if file.Source.Type != openCodeV2FileInline && file.Source.Type != openCodeV2FileURI {
		return openCodeCurrentFile{}, errors.New("native file source type must be inline or uri")
	}
	if file.Source.Type == openCodeV2FileURI && file.Source.URI == "" {
		return openCodeCurrentFile{}, errors.New("native uri file source requires uri")
	}
	if file.Source.Type == openCodeV2FileInline && file.Source.URI != "" {
		return openCodeCurrentFile{}, errors.New("native inline file source cannot contain uri")
	}
	return openCodeCurrentFile{URI: file.Source.URI, MIME: file.MIME, Name: file.Name, Description: file.Description}, nil
}

func validateOpenCodeV2Error(value *openCodeCurrentUnknownError) error {
	if value == nil {
		return nil
	}
	if value.Type == "" || value.Message == "" {
		return errors.New("native error requires type and message")
	}
	if value.Status != nil && (*value.Status < 100 || *value.Status > 599) {
		return errors.New("native error HTTP status must be between 100 and 599")
	}
	return nil
}

type openCodeV2ToolStreaming struct {
	Status openCodeV2ToolStatus `json:"status"`
	Input  string               `json:"input"`
}
type openCodeV2ToolRunning struct {
	Status   openCodeV2ToolStatus       `json:"status"`
	Input    map[string]json.RawMessage `json:"input"`
	Metadata map[string]json.RawMessage `json:"metadata"`
}
type openCodeV2ToolCompleted struct {
	Status   openCodeV2ToolStatus       `json:"status"`
	Input    map[string]json.RawMessage `json:"input"`
	Content  []json.RawMessage          `json:"content"`
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
}
type openCodeV2ToolError struct {
	Status   openCodeV2ToolStatus         `json:"status"`
	Input    map[string]json.RawMessage   `json:"input"`
	Error    *openCodeCurrentUnknownError `json:"error"`
	Content  []json.RawMessage            `json:"content,omitempty"`
	Metadata map[string]json.RawMessage   `json:"metadata,omitempty"`
}

type openCodeV2ToolTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type openCodeV2ToolFileContent struct {
	Type string `json:"type"`
	URI  string `json:"uri"`
	MIME string `json:"mime"`
	Name string `json:"name,omitempty"`
}

func decodeOpenCodeV2ToolState(raw json.RawMessage) (openCodeCurrentToolState, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return openCodeCurrentToolState{}, true, err
	}
	// These fields uniquely identify historical tool states. Do not relax the
	// old requirements merely because a historical payload is incomplete.
	if fields["structured"] != nil || fields["attachments"] != nil || fields["outputPaths"] != nil || fields["result"] != nil {
		return openCodeCurrentToolState{}, false, nil
	}
	var status openCodeV2ToolStatus
	if err := json.Unmarshal(fields["status"], &status); err != nil {
		return openCodeCurrentToolState{}, true, err
	}
	state := openCodeCurrentToolState{Status: string(status), Structured: map[string]json.RawMessage{}, Content: []openCodeCurrentToolContent{}}
	if status == "pending" {
		return state, false, nil
	}
	if err := requireOpenCodeCurrentFields(raw, "status", "input"); err != nil {
		return state, true, err
	}
	var contents []json.RawMessage
	switch status {
	case openCodeV2ToolStreamingStatus:
		var value openCodeV2ToolStreaming
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return state, true, err
		}
		state.Status, state.Input = "pending", fields["input"]
	case openCodeV2ToolRunningStatus:
		var value openCodeV2ToolRunning
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return state, true, fmt.Errorf("native running tool requires object input and metadata: %w", err)
		}
		if value.Input == nil || value.Metadata == nil {
			return state, true, errors.New("native running tool requires object input and metadata")
		}
		state.Input, state.Structured = fields["input"], value.Metadata
	case openCodeV2ToolCompletedStatus:
		var value openCodeV2ToolCompleted
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return state, true, fmt.Errorf("native completed tool requires object input: %w", err)
		}
		if value.Input == nil || len(value.Content) == 0 {
			return state, true, errors.New("native completed tool requires object input and nonempty content")
		}
		state.Input, contents = fields["input"], value.Content
		if value.Metadata != nil {
			state.Structured = value.Metadata
		}
	case openCodeV2ToolErrorStatus:
		var value openCodeV2ToolError
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return state, true, err
		}
		if value.Input == nil || value.Error == nil {
			return state, true, errors.New("native error tool requires object input and error")
		}
		if err := validateOpenCodeV2Error(value.Error); err != nil {
			return state, true, err
		}
		if fields["content"] != nil && len(value.Content) == 0 {
			return state, true, errors.New("native error tool content must be nonempty when present")
		}
		state.Input, state.Error, contents = fields["input"], value.Error, value.Content
		if value.Metadata != nil {
			state.Structured = value.Metadata
		}
	default:
		return state, true, fmt.Errorf("native tool state status %q is unsupported", status)
	}
	for _, content := range contents {
		var value openCodeCurrentToolContent
		if err := decodeOpenCodeCurrentJSON(content, &value); err != nil {
			return state, true, err
		}
		switch value.Type {
		case "text":
			var typed openCodeV2ToolTextContent
			if err := decodeOpenCodeCurrentJSON(content, &typed); err != nil {
				return state, true, err
			}
			if err := requireOpenCodeCurrentFields(content, "text"); err != nil {
				return state, true, err
			}
		case "file":
			var typed openCodeV2ToolFileContent
			if err := decodeOpenCodeCurrentJSON(content, &typed); err != nil {
				return state, true, err
			}
			if err := requireOpenCodeCurrentFields(content, "uri", "mime"); err != nil {
				return state, true, err
			}
		default:
			return state, true, fmt.Errorf("native tool content type %q is unsupported", value.Type)
		}
		state.Content = append(state.Content, value)
	}
	// Text-only completed output has the same user-visible meaning as the
	// historical result string. Keep file/mixed output structured and preserve
	// the established precedence of an error over any partial content.
	if status == openCodeV2ToolCompletedStatus {
		var output strings.Builder
		textOnly := len(state.Content) > 0
		for _, content := range state.Content {
			if content.Type != "text" {
				textOnly = false
				break
			}
			output.WriteString(content.Text)
		}
		if textOnly {
			result, err := json.Marshal(output.String())
			if err != nil {
				return state, true, err
			}
			state.Result = result
		}
	}
	return state, true, nil
}

type openCodeV2Shell struct {
	openCodeCurrentBase
	ShellID string                 `json:"shellID"`
	Command string                 `json:"command"`
	Status  openCodeV2ShellStatus  `json:"status"`
	Exit    *float64               `json:"exit,omitempty"`
	Output  *openCodeV2ShellOutput `json:"output,omitempty"`
}
type openCodeV2ShellOutput struct {
	Output    string `json:"output"`
	Cursor    int64  `json:"cursor"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}
type openCodeV2Compaction struct {
	openCodeCurrentBase
	Status        openCodeV2CompactionStatus   `json:"status"`
	Reason        string                       `json:"reason"`
	Summary       string                       `json:"summary,omitempty"`
	Recent        string                       `json:"recent,omitempty"`
	Model         *openCodeCurrentModel        `json:"model,omitempty"`
	ProviderState map[string]json.RawMessage   `json:"providerState,omitempty"`
	Error         *openCodeCurrentUnknownError `json:"error,omitempty"`
}

func normalizeOpenCodeV2ShellState(value openCodeCurrentShell, fields map[string]json.RawMessage) (openCodeCurrentToolState, error) {
	var status openCodeV2ShellStatus
	if err := json.Unmarshal(fields["status"], &status); err != nil {
		return openCodeCurrentToolState{}, err
	}
	input, err := json.Marshal(map[string]string{"command": value.Command})
	if err != nil {
		return openCodeCurrentToolState{}, err
	}
	state := openCodeCurrentToolState{Status: "completed", Input: input, Structured: map[string]json.RawMessage{"status": fields["status"]}, Content: []openCodeCurrentToolContent{}}
	if status == openCodeV2ShellRunning {
		state.Status = "running"
	}
	if fields["exit"] != nil {
		state.Structured["exit"] = fields["exit"]
	}
	if value.Output != "" {
		state.Content = append(state.Content, openCodeCurrentToolContent{Type: "text", Text: value.Output})
	}
	return state, nil
}

func normalizeOpenCodeV2StructuralRow(row OpenCodeCurrentMessageRow, raw []byte, fields map[string]json.RawMessage) ([]byte, error) {
	switch row.Type.String() {
	case "shell":
		if fields["shellID"] == nil {
			return raw, nil
		}
		var value openCodeV2Shell
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return nil, err
		}
		if err := requireOpenCodeCurrentFields(raw, "shellID", "command", "status", "time"); err != nil {
			return nil, err
		}
		if value.ShellID == "" {
			return nil, errors.New("native shell requires shellID")
		}
		switch value.Status {
		case openCodeV2ShellRunning, openCodeV2ShellExited, openCodeV2ShellTimeout, openCodeV2ShellKilled:
		default:
			return nil, fmt.Errorf("native shell status %q is unsupported", value.Status)
		}
		output := ""
		if value.Output != nil {
			if err := requireOpenCodeCurrentFields(fields["output"], "output", "cursor", "size", "truncated"); err != nil {
				return nil, err
			}
			if value.Output.Cursor < 0 || value.Output.Size < 0 {
				return nil, errors.New("native shell output cursor and size must be nonnegative")
			}
			output = value.Output.Output
		}
		return json.Marshal(openCodeCurrentShell{openCodeCurrentBase: value.openCodeCurrentBase, CallID: value.ShellID, Command: value.Command, Output: output})
	case "compaction":
		if fields["status"] == nil {
			return raw, nil
		}
		var value openCodeV2Compaction
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return nil, err
		}
		if err := requireOpenCodeCurrentFields(raw, "status", "reason", "time"); err != nil {
			return nil, err
		}
		switch value.Status {
		case openCodeV2CompactionRunning, openCodeV2CompactionCompleted:
			if err := requireOpenCodeCurrentFields(raw, "summary", "recent"); err != nil {
				return nil, err
			}
			if value.Error != nil {
				return nil, errors.New("native successful compaction cannot carry error")
			}
			if value.Status == openCodeV2CompactionRunning && (value.Model != nil || value.ProviderState != nil) {
				return nil, errors.New("native running compaction cannot carry completed metadata")
			}
		case openCodeV2CompactionFailed:
			if value.Error == nil {
				return nil, errors.New("native failed compaction requires error")
			}
			if err := validateOpenCodeV2Error(value.Error); err != nil {
				return nil, err
			}
			if fields["summary"] != nil || fields["recent"] != nil || fields["model"] != nil || fields["providerState"] != nil {
				return nil, errors.New("native failed compaction cannot carry summary or completed metadata")
			}
			value.Summary = value.Error.Message
		default:
			return nil, fmt.Errorf("native compaction status %q is unsupported", value.Status)
		}
		return json.Marshal(openCodeCurrentCompaction{openCodeCurrentBase: value.openCodeCurrentBase, Reason: value.Reason, Summary: value.Summary, Recent: value.Recent})
	}
	return raw, nil
}
