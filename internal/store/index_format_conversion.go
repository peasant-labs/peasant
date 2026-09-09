package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// IndexFormatConversion is one concrete, lossless upgrade between supported
// representations. Convert reads the source on the supplied snapshot and returns
// the target's concrete result. It must not run a harness parser, obtain another
// connection, or commit. The common Store writer owns persistence and rollback.
type IndexFormatConversion struct {
	FromVersion int
	ToVersion   int
	Convert     func(context.Context, *sqlite.Conn, schema.SessionID) (indexformat.Result, error)
}

type indexConversionKey struct{ from, to int }

// WithIndexFormatConversions registers only the concrete upgrade edges the
// caller supplies. Production has no implicit conversions or historical parser
// matrix. Registration errors are reported before the database is opened.
func WithIndexFormatConversions(conversions ...IndexFormatConversion) OpenOption {
	owned := append([]IndexFormatConversion(nil), conversions...)
	return func(options *openOptions) { options.indexConversions = append(options.indexConversions, owned...) }
}

func newIndexFormatConversions(conversions []IndexFormatConversion, formats map[int]IndexFormat) (map[indexConversionKey]IndexFormatConversion, error) {
	result := make(map[indexConversionKey]IndexFormatConversion, len(conversions))
	for _, conversion := range conversions {
		key := indexConversionKey{from: conversion.FromVersion, to: conversion.ToVersion}
		if conversion.Convert == nil || key.from < 1 || key.to <= key.from {
			return nil, fmt.Errorf("store: invalid index format conversion %d to %d before opening database; supply one concrete lossless upgrade with positive increasing versions, not a downgrade", key.from, key.to)
		}
		if _, ok := formats[key.from]; !ok {
			return nil, fmt.Errorf("store: conversion source format %d has no handler; register supported source and target formats before opening database", key.from)
		}
		if _, ok := formats[key.to]; !ok {
			return nil, fmt.Errorf("store: conversion target format %d has no handler; register supported source and target formats before opening database", key.to)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("store: duplicate index format conversion %d to %d; keep one authoritative upgrade before opening database", key.from, key.to)
		}
		result[key] = conversion
	}
	return result, nil
}

// ConvertIndexFormat upgrades one existing index through an explicitly
// registered edge. It preserves the actual producer and parser-run timestamp,
// including newer producer revisions that this build cannot reproduce. It does
// not append a parser attempt log, select other sessions, or read native files.
func (s *Store) ConvertIndexFormat(ctx context.Context, sessionID schema.SessionID, target int) (err error) {
	if target < 1 {
		return fmt.Errorf("store: index conversion target %d is invalid; no data changed; select a positive supported format", target)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take index conversion connection: %w", err)
	}
	defer s.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	defer end(&err)
	before, err := readIndexStateOnConn(conn, sessionID)
	if err != nil {
		return err
	}
	if before == nil || before.IndexVersion == nil {
		return fmt.Errorf("store: session %s has no recorded index representation to convert; nothing was changed; complete indexing before requesting a format conversion", sessionID)
	}
	if !s.SupportsIndexFormat(*before.IndexVersion) {
		return &UnsupportedIndexFormatError{SessionID: sessionID, Version: *before.IndexVersion}
	}
	if !s.SupportsIndexFormat(target) {
		return &UnsupportedIndexFormatError{SessionID: sessionID, Version: target}
	}
	if *before.IndexVersion == target {
		return nil
	}
	conversion, exists := s.indexConversions[indexConversionKey{from: *before.IndexVersion, to: target}]
	if !exists {
		return fmt.Errorf("store: no supported index conversion from %d to %d for session %s; existing representation and parser history were preserved; use a build with a concrete lossless upgrade for this pair", *before.IndexVersion, target, sessionID)
	}
	output, err := conversion.Convert(ctx, conn, sessionID)
	if err != nil {
		return fmt.Errorf("store: convert session %s index from %d to %d: %w; prior index was preserved", sessionID, conversion.FromVersion, target, err)
	}
	current, err := readIndexStateOnConn(conn, sessionID)
	if err != nil {
		return err
	}
	if !sameIndexState(before, current) {
		return fmt.Errorf("store: index conversion for session %s changed source format, input evidence or parser history while preparing its result; the transaction was refused; correct the conversion to preserve captured source state, producer revision and run time", sessionID)
	}
	capture, _, err := readCapture(conn, ingest.SessionID(sessionID))
	if err != nil {
		return err
	}
	stmts := newSessionEntryWriteStatements(conn)
	defer func() { err = errors.Join(err, stmts.Close()) }()
	// The conversion carries the session's OWN capture context, not a fresh
	// parser claim: its publication revision, so the write re-stamps the same
	// binding instead of failing the revision check or leaving the index
	// unbound; its capture row, so a complete capture cannot be silently
	// downgraded to a preview; and no indexer revision, because no parser ran.
	_, err, _ = s.indexSessionEntryWriteSavepoint(ctx, conn, ingest.SessionEntryWrite{
		SessionID:          sessionID,
		Result:             output,
		IndexVersion:       target,
		Mode:               ingest.SessionEntryWriteFormatConversion,
		CaptureRevision:    before.PublicationCaptureRevision,
		RequireFullContent: capture.Status == ingest.ContentCaptureComplete,
		ContentCapture: ingest.SessionContentCaptureWrite{
			PublicationCaptureRevision: capture.PublicationCaptureRevision,
			Status:                     capture.Status,
			SourceAuthority:            capture.SourceAuthority,
			TranscriptOrigin:           capture.TranscriptOrigin,
			CaptureFormat:              capture.CaptureFormat,
			CapturedAtMs:               capture.CapturedAtMs,
			FailureCode:                capture.FailureCode,
			FailureMessage:             capture.FailureMessage,
		},
		ExpectedState: before,
	}, stmts, &conversion)
	return err
}
