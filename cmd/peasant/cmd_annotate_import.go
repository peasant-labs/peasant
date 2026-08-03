package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/annotations"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// buildAnnotateImportCommand creates the `annotate import` subcommand.
// Imports annotations from JSONL files produced by `peasant export annotations`.
func buildAnnotateImportCommand() *cobra.Command {
	var (
		fromFile string
		fromDir  string
	)

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import annotations from JSONL files",
		Long: `Import annotations from JSONL files produced by 'peasant export annotations'.

Each line in the JSONL file must be a valid ExportedAnnotation record.
Invalid records are skipped with a warning; valid records are imported.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAnnotateImport(cmd, fromFile, fromDir)
		},
	}

	cmd.Flags().StringVar(&fromFile, "from-file", "", "Path to JSONL file to import")
	cmd.Flags().StringVar(&fromDir, "from-dir", "", "Path to directory containing *--annotations.jsonl files")
	cmd.Flags().String("model-id", "", "Model ID for agent annotators (e.g. claude-opus-4-6)")
	cmd.Flags().String("provider", "", "Provider key for agent annotators (e.g. anthropic)")

	return cmd
}

// runAnnotateImport validates flags and dispatches import for file or directory mode.
func runAnnotateImport(cmd *cobra.Command, fromFile, fromDir string) error {
	// Validate flags: exactly one of --from-file or --from-dir must be set.
	fileSet := cmd.Flags().Changed("from-file")
	dirSet := cmd.Flags().Changed("from-dir")

	if fileSet && dirSet {
		return fmt.Errorf(
			"annotate import: conflicting flags\n" +
				"What went wrong: both --from-file and --from-dir were provided.\n" +
				"Why: exactly one import source is required.\n" +
				"Fix: provide --from-file <path> for a single JSONL file, or --from-dir <dir> for a directory of JSONL files, not both.",
		)
	}
	if !fileSet && !dirSet {
		return fmt.Errorf(
			"annotate import: missing required flag\n" +
				"What went wrong: neither --from-file nor --from-dir was provided.\n" +
				"Why: an import source is required.\n" +
				"Fix: provide --from-file <path> for a single JSONL file, or --from-dir <dir> for a directory of *--annotations.jsonl files.",
		)
	}

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := cmd.Context()
	registry := annotations.NewTypeReader(db)

	var files []string
	if fileSet {
		files = []string{fromFile}
	} else {
		matches, globErr := filepath.Glob(filepath.Join(fromDir, "*--annotations.jsonl"))
		if globErr != nil {
			return fmt.Errorf(
				"annotate import: glob %q: %w\n"+
					"What went wrong: failed to glob for annotation JSONL files.\n"+
					"Where: directory %q.\n"+
					"Fix: verify the directory path is valid and accessible.",
				fromDir, globErr, fromDir,
			)
		}
		if len(matches) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no *--annotations.jsonl files found in directory")
			return nil
		}
		files = matches
	}

	var totalImported, totalRecords, totalSkipped, totalDedupSkipped int

	for _, path := range files {
		f, openErr := os.Open(path)
		if openErr != nil {
			return fmt.Errorf(
				"annotate import: open %q: %w\n"+
					"What went wrong: could not open JSONL file for reading.\n"+
					"Where: %s.\n"+
					"Fix: verify the file exists and is readable.",
				path, openErr, path,
			)
		}

		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Bytes()

			totalRecords++

			var rec export.ExportedAnnotation
			if jsonErr := json.Unmarshal(line, &rec); jsonErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%d: malformed JSON: %v\n", path, lineNum, jsonErr)
				totalSkipped++
				continue
			}

			// Validate annotator_kind.
			annotatorKind := schema.AnnotatorKind(rec.AnnotatorKind)
			if !annotatorKind.IsValid() {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%d: invalid annotator_kind %q\n", path, lineNum, rec.AnnotatorKind)
				totalSkipped++
				continue
			}

			// Validate type_id via registry lookup.
			typeSummary, typeErr := registry.GetType(ctx, rec.TypeID)
			if typeErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%d: unknown type_id %q: %v\n", path, lineNum, rec.TypeID, typeErr)
				totalSkipped++
				continue
			}

			// Validate value against the type's domain.
			if valErr := schema.ValidateAnnotationValue(typeSummary.ValueDomain, rec.Value); valErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%d: invalid value %q for type %q: %v\n",
					path, lineNum, rec.Value, rec.TypeID, valErr)
				totalSkipped++
				continue
			}

			// Resolve model info: JSONL fields take priority over CLI flags.
			modelID := rec.ModelID
			if modelID == "" {
				modelID, _ = cmd.Flags().GetString("model-id")
			}
			providerKey := rec.ProviderKey
			if providerKey == "" {
				providerKey, _ = cmd.Flags().GetString("provider")
			}
			var modelPtr, providerPtr *string
			if modelID != "" {
				modelPtr = &modelID
			}
			if providerKey != "" {
				providerPtr = &providerKey
			}

			// Resolve annotator: look up by name, create if not found.
			annotatorID, annotatorErr := resolveOrCreateAnnotator(ctx, db, rec.Annotator, annotatorKind, modelPtr, providerPtr)
			if annotatorErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%d: annotator %q: %v\n",
					path, lineNum, rec.Annotator, annotatorErr)
				totalSkipped++
				continue
			}

			// Build create params.
			params := store.CreateAnnotationParams{
				AnnotatorID:      annotatorID,
				AnnotationTypeID: typeSummary.ID,
				Value:            rec.Value,
				Confidence:       rec.Confidence,
			}

			// Set target based on whether the annotation has entry-level range.
			if rec.StartEntry != nil {
				endIdx := 0
				if rec.EndEntry != nil {
					endIdx = *rec.EndEntry
				}
				params.EntryTarget = &store.EntryTarget{
					SessionID:  rec.SessionID,
					EntryIndex: *rec.StartEntry,
					EndIndex:   endIdx,
				}
			} else {
				sid := rec.SessionID
				params.SessionID = &sid
			}

			// Set optional reason.
			if rec.Reason != "" {
				reason := rec.Reason
				params.Reason = &reason
			}

			// Compute content hash for dedup.
			// params.Reason, params.Confidence are already set above.
			// hashSID is always the record-level SessionID — shared across both the session
			// and entry target arms — so the hash key is stable regardless of target type.
			hashSID := rec.SessionID
			var entryIdx *int
			var endIdx *int
			if rec.StartEntry != nil {
				entryIdx = rec.StartEntry
			}
			if rec.EndEntry != nil {
				endIdx = rec.EndEntry
			}
			newHash := schema.ComputeAnnotationHash(
				typeSummary.ID, annotatorID, rec.Value,
				&hashSID, entryIdx, endIdx,
				rec.Confidence, params.Reason, nil,
			)

			// Dedup: check for existing annotation at same target.
			// Hash-based 3-way logic:
			//   - No existing → create new, set content_hash.
			//   - Same hash   → true duplicate, skip.
			//   - Hash mismatch → supersede old with new (atomic transaction).
			findParams := ingest.FindAnnotationParams{
				AnnotationTypeID: typeSummary.ID,
				AnnotatorID:      annotatorID,
			}
			if params.EntryTarget != nil {
				entrySessionID := params.EntryTarget.SessionID
				idx := params.EntryTarget.EntryIndex
				findParams.SessionID = &entrySessionID
				findParams.EntryIndex = &idx
			} else if params.SessionID != nil {
				findParams.SessionID = params.SessionID
			}
			existing, findErr := db.FindExistingAnnotation(ctx, findParams)
			if findErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%d: dedup lookup: %v\n",
					path, lineNum, findErr)
				totalSkipped++
				continue
			}

			switch {
			case existing == nil:
				newID, createErr := db.CreateAnnotation(ctx, params)
				if createErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%d: create annotation: %v\n",
						path, lineNum, createErr)
					totalSkipped++
					continue
				}
				if hashErr := db.UpdateContentHash(ctx, newID, newHash); hashErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%d: update content hash: %v\n",
						path, lineNum, hashErr)
				}
				totalImported++

			case existing.ContentHash == newHash:
				// Same content — true duplicate, skip.
				totalDedupSkipped++

			default:
				// Hash mismatch — supersede old with new (atomic transaction).
				ingestParams := ingest.CreateAnnotationParams{
					AnnotatorID:      params.AnnotatorID,
					AnnotationTypeID: params.AnnotationTypeID,
					Value:            params.Value,
					Confidence:       params.Confidence,
					Reason:           params.Reason,
				}
				if params.EntryTarget != nil {
					ingestParams.EntryTarget = &ingest.EntryTarget{
						SessionID:  params.EntryTarget.SessionID,
						EntryIndex: params.EntryTarget.EntryIndex,
						EndIndex:   params.EntryTarget.EndIndex,
					}
				} else {
					ingestParams.SessionID = params.SessionID
				}
				if _, createErr := db.CreateAnnotationAndSupersede(ctx, ingestParams, existing.ID, newHash); createErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%d: supersede annotation: %v\n",
						path, lineNum, createErr)
					totalSkipped++
					continue
				}
				totalImported++
			}
		}

		if scanErr := scanner.Err(); scanErr != nil {
			f.Close()
			return fmt.Errorf(
				"annotate import: read %q: %w\n"+
					"What went wrong: I/O error while scanning JSONL file.\n"+
					"Where: %s at line %d.\n"+
					"Fix: verify the file is not corrupt or being written concurrently.",
				path, scanErr, path, lineNum,
			)
		}

		f.Close()
	}

	fmt.Fprintf(cmd.OutOrStdout(), "imported %d of %d annotations (%d duplicates skipped, %d errors)\n",
		totalImported, totalRecords, totalDedupSkipped, totalSkipped)
	return nil
}

// resolveOrCreateAnnotator looks up an annotator by name. If found, returns its ID.
// If not found, creates a new annotator with the given name and kind.
//
// For agent annotators, the DB name is composed as "baseName:modelID" to provide
// per-model provenance. The model row is ensured via INSERT OR IGNORE before creating
// the annotator (FK constraint: annotators.model_id -> models.model_id).
func resolveOrCreateAnnotator(
	ctx context.Context, db *store.Store,
	baseName string, kind schema.AnnotatorKind,
	modelID, providerKey *string,
) (string, error) {
	// For agent annotators: compose name as "baseName:modelID".
	name := baseName
	if kind == schema.AnnotatorAgent {
		if modelID == nil || *modelID == "" || providerKey == nil || *providerKey == "" {
			return "", fmt.Errorf(
				"agent annotator %q requires model info\n"+
					"What went wrong: no model_id/provider_key in JSONL record and no --model-id/--provider CLI flags.\n"+
					"Where: resolveOrCreateAnnotator during annotate import.\n"+
					"Fix: provide --model-id and --provider flags, or include model_id/provider_key in the JSONL record.",
				baseName,
			)
		}
		name = baseName + ":" + *modelID
	}

	existing, err := db.GetAnnotator(ctx, name)
	if err != nil {
		return "", fmt.Errorf(
			"lookup annotator %q: %w\n"+
				"What went wrong: database query for annotator failed.\n"+
				"Fix: verify the database is accessible and not locked.",
			name, err,
		)
	}
	if existing != nil {
		return existing.ID, nil
	}

	// For agent annotators, ensure model row exists (FK constraint).
	if kind == schema.AnnotatorAgent {
		if ensureErr := db.EnsureModelMinimal(ctx, *modelID, *providerKey); ensureErr != nil {
			return "", fmt.Errorf(
				"ensure model for annotator %q: %w\n"+
					"What went wrong: could not insert minimal model row for %s/%s.\n"+
					"Fix: verify the database is writable.",
				name, ensureErr, *modelID, *providerKey,
			)
		}
	}

	displayName := baseName
	if kind == schema.AnnotatorAgent {
		displayName = baseName + " (" + *modelID + ")"
	}

	// Create a new annotator for the import.
	id, err := db.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        kind,
		Name:        name,
		DisplayName: displayName,
		ModelID:     modelID,
		ProviderKey: providerKey,
		Description: fmt.Sprintf("Auto-created during annotate import for %s annotator %q.", kind.String(), name),
	})
	if err != nil {
		return "", fmt.Errorf(
			"create annotator %q: %w\n"+
				"What went wrong: could not insert a new annotator row.\n"+
				"Fix: verify the database is writable and the annotator name is unique.",
			name, err,
		)
	}
	return id, nil
}
