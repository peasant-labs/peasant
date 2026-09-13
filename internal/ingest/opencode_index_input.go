package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
)

// captureJSONInput reads only this session's native message/part files. Decoding
// is separate so indexing can release managed-file ownership before parsing.
func (idx *OpenCodeIndexer) captureJSONInput(ctx context.Context, session DiscoveredSession) (*openCodeJSONInput, error) {
	storageRoot := resolveStorageRoot(session)
	messageDirectory := filepath.Join(storageRoot, defaults.OpenCodeDirMessage.String(), string(session.SessionID))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := idx.fs.ReadDir(messageDirectory)
	if err != nil {
		return nil, fmt.Errorf("read message directory %s: %w", messageDirectory, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), defaults.ExtJSON.String()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	input := &openCodeJSONInput{Messages: make([]openCodeJSONMessage, 0, len(names))}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := idx.fs.ReadFile(filepath.Join(messageDirectory, name))
		if err != nil {
			return nil, fmt.Errorf("read message %s: %w", filepath.Join(messageDirectory, name), err)
		}
		message := openCodeJSONMessage{File: openCodeJSONFile{Name: name, Data: bytes.Clone(data)}}
		messageID := strings.TrimSuffix(name, defaults.ExtJSON.String())
		parts, err := listPartFilenamesWithError(idx.fs, storageRoot, messageID)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("read part directory for message %s: %w", messageID, err)
			}
			message.PartsMissing = true
		}
		for _, part := range parts {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			path := filepath.Join(storageRoot, defaults.OpenCodeDirPart.String(), messageID, part)
			data, err := idx.fs.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read part %s: %w", path, err)
			}
			message.Parts = append(message.Parts, openCodeJSONFile{Name: part, Data: bytes.Clone(data)})
		}
		input.Messages = append(input.Messages, message)
	}
	return input, nil
}

func (idx *OpenCodeIndexer) indexJSONInput(ctx context.Context, session DiscoveredSession, input *openCodeJSONInput) (indexformat.Result, error) {
	completion := &indexCompletion{ctx: ctx, session: session}
	if input == nil {
		return completion.result(nil, fmt.Errorf("native message/part input was not captured"))
	}
	messages, err := parseOpenCodeJSONInput(input, completion)
	if err != nil {
		return completion.result(nil, err)
	}
	return completion.result(idx.indexSemanticMessages(session.SessionID, messages), nil)
}

func parseOpenCodeJSONInput(input *openCodeJSONInput, completion *indexCompletion) ([]openCodeSemanticMessage, error) {
	messages := make([]openCodeSemanticMessage, 0, len(input.Messages))
	for _, captured := range input.Messages {
		completion.line++
		if err := completion.record(captured.File.Data); err != nil {
			return nil, fmt.Errorf("message %s: %w", captured.File.Name, err)
		}
		messageID := strings.TrimSuffix(captured.File.Name, defaults.ExtJSON.String())
		message, err := parseOpenCodeSemanticMessage(messageID, 0, captured.File.Data)
		if err != nil {
			return nil, fmt.Errorf("decode message %s: %w", captured.File.Name, err)
		}
		if !Role(message.Data.Role).IsValid() {
			return nil, fmt.Errorf("message %s has unsupported role %q", captured.File.Name, message.Data.Role)
		}
		if err := validateIndexContent(message.Data.Content); err != nil {
			return nil, fmt.Errorf("message %s inline content: %w", captured.File.Name, err)
		}
		completion.recognized++
		if captured.PartsMissing {
			inline := bytes.TrimSpace(message.Data.Content)
			if len(inline) == 0 || bytes.Equal(inline, []byte("null")) {
				return nil, fmt.Errorf("read required part directory for message %s: %w; only an absent directory with explicit inline content is supported", messageID, fs.ErrNotExist)
			}
		}
		for _, capturedPart := range captured.Parts {
			if err := completion.record(capturedPart.Data); err != nil {
				return nil, fmt.Errorf("part %s: %w", capturedPart.Name, err)
			}
			partID := strings.TrimSuffix(capturedPart.Name, defaults.ExtJSON.String())
			part, err := parseOpenCodeSemanticPart(partID, 0, capturedPart.Data)
			if err != nil {
				return nil, fmt.Errorf("decode part %s: %w", capturedPart.Name, err)
			}
			if part.Data.Type == "" {
				return nil, fmt.Errorf("part %s lacks its type", capturedPart.Name)
			}
			message.Parts = append(message.Parts, part)
		}
		messages = append(messages, message)
	}
	return messages, nil
}
