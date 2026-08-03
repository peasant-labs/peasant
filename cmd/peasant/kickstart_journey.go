package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

func buildKickstartJourneyRunner(cmd *cobra.Command, configPath string, loaded *config.Config, snapshot []byte, existed bool, ingestRunner ftue.IngestRunnerFunc) ftue.JourneyRunner {
	return buildKickstartJourneyRunnerWithDeps(cmd, configPath, loaded, snapshot, existed, ingestRunner, kickstartJourneyDeps{
		loadCredentials:  auth.LoadCredentialsFrom,
		openReceiptStore: func(path string) (kickstartReceiptStore, error) { return store.Open(path) },
	})
}

type kickstartReceiptStore interface {
	AllPushableSessions(context.Context) ([]ingest.PushSessionRow, error)
	Publication(context.Context, string, string, schema.ProjectHash, string) (*store.PublicationRecord, error)
	Close() error
}

type kickstartJourneyDeps struct {
	loadCredentials  func(string) (*auth.Credentials, error)
	openReceiptStore func(string) (kickstartReceiptStore, error)
}

func buildKickstartJourneyRunnerWithDeps(cmd *cobra.Command, configPath string, loaded *config.Config, snapshot []byte, existed bool, ingestRunner ftue.IngestRunnerFunc, deps kickstartJourneyDeps) ftue.JourneyRunner {
	return ftue.JourneyRunnerFunc(func(ctx context.Context, request ftue.JourneyRequest) (ftue.JourneyResult, error) {
		operations := buildKickstartJourneyOperations(cmd, configPath, loaded, snapshot, existed, ingestRunner, deps)
		return (ftue.OrderedJourneyRunner{Operations: operations}).Run(ctx, request)
	})
}

func buildKickstartJourneyOperations(cmd *cobra.Command, configPath string, loaded *config.Config, snapshot []byte, existed bool, ingestRunner ftue.IngestRunnerFunc, deps kickstartJourneyDeps) map[ftue.ExecutionStage]ftue.StageOperation {
	operations := map[ftue.ExecutionStage]ftue.StageOperation{}

	operations[ftue.StageConfig] = func(ctx context.Context, request ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
		if !request.Answers.FinalConsent {
			return nil, []ftue.RetryTarget{{Stage: ftue.StageConfig}}, fmt.Errorf("save kickstart config: final consent was not recorded; no configuration was changed; return to Final Consent and confirm the displayed choices")
		}
		if err := ftue.SaveAnswers(configPath, loaded, snapshot, existed, request.Answers); err != nil {
			return nil, []ftue.RetryTarget{{Stage: ftue.StageConfig}}, err
		}
		if request.Answers.ClaudeRetentionDays > 0 {
			if err := ftue.WriteClaudeCleanupDays(request.Answers.ClaudeRetentionDays); err != nil {
				return []ftue.PersistedEffect{{Stage: ftue.StageConfig, Status: ftue.StatusPersisted, Detail: configPath}}, []ftue.RetryTarget{{Stage: ftue.StageConfig}}, fmt.Errorf("save Claude transcript retention after config %q was persisted: %w; the config remains saved", configPath, err)
			}
		}
		return []ftue.PersistedEffect{{Stage: ftue.StageConfig, Status: ftue.StatusPersisted, Detail: configPath}}, nil, nil
	}

	operations[ftue.StageIngest] = func(ctx context.Context, request ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
		ids := selectedJourneyIDs(request)
		if !request.Answers.WantImport || len(ids) == 0 {
			return []ftue.PersistedEffect{{Stage: ftue.StageIngest, Status: ftue.StatusSkipped, Detail: "no sessions were consented for import"}}, nil, nil
		}
		if ingestRunner == nil {
			return nil, []ftue.RetryTarget{{Stage: ftue.StageIngest, SessionIDs: ids}}, fmt.Errorf("run kickstart ingest: the production ingest runner is unavailable; config was saved but no transcript was imported; retry peasant kickstart")
		}
		answers := filterJourneyAnswers(request.Answers, ids)
		if _, err := ingestRunner(ctx, answers); err != nil {
			return nil, []ftue.RetryTarget{{Stage: ftue.StageIngest, SessionIDs: ids}}, err
		}
		effects := make([]ftue.PersistedEffect, 0, len(ids))
		for _, id := range ids {
			effects = append(effects, ftue.PersistedEffect{Stage: ftue.StageIngest, Status: ftue.StatusPersisted, SessionID: id, Detail: "ingested, indexed, and stored"})
		}
		return effects, nil, nil
	}

	operations[ftue.StagePublication] = func(ctx context.Context, request ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
		ids := selectedJourneyIDs(request)
		if request.Answers.Destination != ftue.DestinationVillage || len(ids) == 0 {
			return []ftue.PersistedEffect{{Stage: ftue.StagePublication, Status: ftue.StatusSkipped, Detail: "local-only destination"}}, nil, nil
		}
		cfg, err := loadConfig(configPath)
		if err != nil {
			return nil, retrySessions(ftue.StagePublication, ids), fmt.Errorf("load saved kickstart config for publication: %w", err)
		}
		creds, err := auth.LoadCredentialsFrom(configDirOverride(cmd))
		if err != nil || creds == nil || !creds.IsValid() {
			return nil, retrySessions(ftue.StagePublication, ids), fmt.Errorf("authenticate kickstart publication: valid Village credentials were not available after consent; imported sessions remain local; run 'peasant village login' and retry kickstart")
		}
		db, err := store.Open(string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd))))
		if err != nil {
			return nil, retrySessions(ftue.StagePublication, ids), fmt.Errorf("open authoritative publication store: %w", err)
		}
		defer db.Close()
		policy := config.ResolveRedactionPolicy(cfg.Redaction.Level)
		patterns, err := config.CustomPatternsToUserPatterns(cfg.Redaction.CustomPatterns)
		if err != nil {
			return nil, retrySessions(ftue.StagePublication, ids), fmt.Errorf("build kickstart publication redactor: %w", err)
		}
		redactor, err := redact.NewRedactor(policy.Effective, patterns, resolveXDGPaths(cmd))
		if err != nil {
			return nil, retrySessions(ftue.StagePublication, ids), fmt.Errorf("create kickstart publication redactor: %w", err)
		}
		selection := cfg.SelectionMatcher()
		runCfg := push.PipelineConfig{FilterSessionIDs: ids, Selection: &selection, Visibility: request.Answers.EffectiveVisibility, License: request.Answers.License, CommandBinding: githooks.Binding{ConfigPath: configPath, ConfigDir: configDirOverride(cmd), DataDir: dataDirOverride(cmd)}}
		client := village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)
		result, runErr := push.NewPipeline(db, client, creds, cfg, &ingest.OSFileSystem{}, runCfg, redactor, io.Discard).Run(ctx)
		return classifyKickstartPublication(ids, result, runErr)
	}

	operations[ftue.StageReceipt] = func(ctx context.Context, request ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
		ids := selectedJourneyIDs(request)
		if request.Answers.Destination != ftue.DestinationVillage {
			return []ftue.PersistedEffect{{Stage: ftue.StageReceipt, Status: ftue.StatusSkipped}}, nil, nil
		}
		creds, err := deps.loadCredentials(configDirOverride(cmd))
		if err != nil || creds == nil || !creds.IsValid() {
			return nil, retrySessions(ftue.StageReceipt, ids), fmt.Errorf("read kickstart receipts: valid Village credentials are unavailable; do not infer remote state; run 'peasant village login' and retry receipt verification")
		}
		db, err := deps.openReceiptStore(string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd))))
		if err != nil {
			return nil, retrySessions(ftue.StageReceipt, ids), fmt.Errorf("read kickstart receipts: open the authoritative publication store: %w; retry receipt verification after repairing the local data path", err)
		}
		defer db.Close()
		rows, err := db.AllPushableSessions(ctx)
		if err != nil {
			return nil, retrySessions(ftue.StageReceipt, ids), fmt.Errorf("read kickstart receipts: resolve local publication identities: %w; retry receipt verification", err)
		}
		publications := make(map[string]ingest.PushSessionRow, len(ids))
		wanted := make(map[string]bool, len(ids))
		for _, id := range ids {
			wanted[id] = true
		}
		for _, row := range rows {
			if wanted[row.SessionID] {
				publications[row.SessionID] = row
			}
		}
		effects, missing := []ftue.PersistedEffect{}, []string{}
		for _, id := range ids {
			row, ok := publications[id]
			if !ok {
				missing = append(missing, id)
				continue
			}
			hash, hashErr := schema.NewProjectHash(row.ProjectHash)
			if hashErr != nil {
				missing = append(missing, id)
				continue
			}
			record, readErr := db.Publication(ctx, creds.VillageURL, creds.UserID, hash, id)
			if readErr != nil || record == nil {
				missing = append(missing, id)
				continue
			}
			receipt := record.Receipt
			effects = append(effects, ftue.PersistedEffect{Stage: ftue.StageReceipt, Status: ftue.StatusPersisted, SessionID: id, URL: receipt.TranscriptURL, Visibility: receipt.Visibility, VillageOrigin: creds.VillageURL, OwnerUserID: creds.UserID, ProjectHash: hash, Receipt: &receipt, Detail: "authoritative receipt read from local store"})
		}
		if len(missing) > 0 {
			return effects, retrySessions(ftue.StageReceipt, missing), fmt.Errorf("verify authoritative publication receipts: no complete persisted receipt was found for %v; uploads are not claimed complete; retry only receipt verification", missing)
		}
		return effects, nil, nil
	}

	operations[ftue.StageHooks] = func(ctx context.Context, request ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
		consents := request.Answers.HookConsents
		if request.HookFilter != nil {
			consents = []ftue.HookConsent{{Repository: request.HookFilter.Repository, Events: append([]githooks.Event(nil), request.HookFilter.Events...)}}
		}
		if len(consents) == 0 {
			return []ftue.PersistedEffect{{Stage: ftue.StageHooks, Status: ftue.StatusSkipped, Detail: "no Git hook installation was consented"}}, nil, nil
		}
		lifecycle := githooks.New(githooks.NewExecGit())
		effects := []ftue.PersistedEffect{}
		for _, consent := range consents {
			report, err := lifecycle.Install(ctx, githooks.Request{Dir: consent.Repository, Events: consent.Events, Binding: githooks.Binding{ConfigPath: configPath, ConfigDir: configDirOverride(cmd), DataDir: dataDirOverride(cmd)}})
			if err != nil || report.Blocked() {
				return effects, []ftue.RetryTarget{{Stage: ftue.StageHooks, Repository: consent.Repository, Events: consent.Events}}, hookInstallFailure(consent.Repository, report.Blocked(), err)
			}
			for _, event := range consent.Events {
				effects = append(effects, ftue.PersistedEffect{Stage: ftue.StageHooks, Status: ftue.StatusPersisted, Repository: report.Repository.Root, RetryRepository: consent.Repository, HookEvent: event, Detail: githooks.EventList(consent.Events)})
			}
		}
		return effects, nil, nil
	}
	return operations
}

func hookInstallFailure(repository string, blocked bool, err error) error {
	reason := "the hook lifecycle returned no usable installation result"
	if blocked {
		reason = "the resolved hook path is occupied by content Peasant does not own"
	}
	if err != nil {
		reason = err.Error()
	}
	return fmt.Errorf("install explicitly consented Git hooks in %q: %s; Peasant left every blocked hook unchanged, publication remains complete, and retry requires following the reported manual integration instructions", repository, reason)
}

func classifyKickstartPublication(ids []string, result *push.PushResult, runErr error) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
	effects := []ftue.PersistedEffect{}
	failed := []string{}
	persisted := map[string]bool{}
	causes := []error{}
	if runErr != nil {
		causes = append(causes, runErr)
	}
	if result != nil {
		for _, session := range result.Sessions {
			switch session.Status {
			case push.PushStatusError:
				failed = append(failed, session.SessionID)
				if session.Error != nil {
					causes = append(causes, fmt.Errorf("session %q: %w", session.SessionID, session.Error))
				} else {
					causes = append(causes, fmt.Errorf("session %q reported a failed publication without an error detail", session.SessionID))
				}
			case push.PushStatusNew, push.PushStatusUpdated:
				persisted[session.SessionID] = true
				effects = append(effects, ftue.PersistedEffect{Stage: ftue.StagePublication, Status: ftue.StatusPersisted, SessionID: session.SessionID, Detail: session.Status.String()})
			}
		}
	}
	if runErr == nil && len(failed) == 0 {
		return effects, nil, nil
	}
	if runErr != nil {
		// A pipeline abort can leave sessions unattempted, so they have no result
		// row. Retry every selected session without durable success evidence, then
		// retain any explicit failure outside the selected set without duplicates.
		unresolved := make([]string, 0, len(ids))
		seen := make(map[string]bool, len(ids)+len(failed))
		for _, id := range ids {
			if !persisted[id] && !seen[id] {
				unresolved = append(unresolved, id)
				seen[id] = true
			}
		}
		for _, id := range failed {
			if !persisted[id] && !seen[id] {
				unresolved = append(unresolved, id)
				seen[id] = true
			}
		}
		failed = unresolved
	}
	if len(causes) == 0 {
		causes = append(causes, fmt.Errorf("publication failed for session IDs %v without an error detail", failed))
	}
	return effects, retrySessions(ftue.StagePublication, failed), fmt.Errorf("publish consented kickstart sessions: %w; successful uploads remain published and only failed session IDs are eligible for retry", errors.Join(causes...))
}

func selectedJourneyIDs(request ftue.JourneyRequest) []string {
	if len(request.SessionFilter) > 0 {
		return append([]string(nil), request.SessionFilter...)
	}
	sessions := request.Answers.EffectiveSelectedSessions()
	ids := make([]string, 0, len(sessions))
	seen := map[string]bool{}
	for _, session := range sessions {
		for _, id := range append([]string{session.SessionID}, session.SubagentIDs...) {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func filterJourneyAnswers(answers ftue.WizardAnswers, ids []string) ftue.WizardAnswers {
	if len(ids) == 0 {
		return answers
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	filtered := make([]ftue.SessionListing, 0, len(ids))
	for _, session := range answers.EffectiveSelectedSessions() {
		if wanted[session.SessionID] {
			filtered = append(filtered, session)
			continue
		}
		for _, id := range session.SubagentIDs {
			if wanted[id] {
				copy := session
				copy.SessionID = id
				copy.SubagentIDs = nil
				filtered = append(filtered, copy)
			}
		}
	}
	answers.SelectedSessions = filtered
	return answers
}

func retrySessions(stage ftue.ExecutionStage, ids []string) []ftue.RetryTarget {
	return []ftue.RetryTarget{{Stage: stage, SessionIDs: append([]string(nil), ids...)}}
}
