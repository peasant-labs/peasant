package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
)

// Scan runs discovery for each configured provider. It returns the provider
// inventory, the root-session listing later stages consume, and the subagent
// relation over the whole discovered set. Errors are silenced: a failed
// provider is recorded on the inventory instead of failing the scan.
//
// known and evidence are injected. A nil known index resolves every session
// from git. A nil evidence cache mines every Claude transcript again. Neither
// value is a database type.
func Scan(
	ctx context.Context,
	cfg *config.Config,
	fs ingest.FileSystem,
	git ingest.GitResolver,
	known ingest.KnownSessionIndex,
	evidence ingest.ClaudeEvidenceCache,
	progress ScanProgress,
) (ProviderInventory, []SessionListing, SubagentRelation) {
	inventory := ProviderInventory{}
	var sessions []SessionListing
	subagents := SubagentRelation{}
	if cfg == nil {
		return inventory, sessions, subagents
	}
	staleness := cfg.StalenessWindow()

	for provider, factory := range ingest.DefaultAdapterRegistry {
		src, ok := configuredSource(cfg, provider)
		if !ok {
			continue
		}
		discovery := ProviderDiscovery{Enabled: src.Enabled}
		inventory[provider] = discovery
		if len(src.Paths) == 0 {
			discovery.State = DiscoveryUnavailable
			discovery.Detail = "no transcript paths are configured"
			inventory[provider] = discovery
			continue
		}
		// Inventory available data before the user chooses providers. The saved
		// selection still controls which providers ingestion enables.
		src.Enabled = true
		setPhase(progress, DiscoveringPhase(provider))
		adapter := factory(fs, git, salt.Salt{})
		ingest.AttachClaudeEvidenceCache(adapter, evidence)
		discovered, err := adapter.Discover(ctx, src)
		if err != nil {
			discovery.State = DiscoveryFailed
			discovery.Detail = err.Error()
			inventory[provider] = discovery
			continue
		}
		childMap := BuildChildMap(discovered)
		for _, d := range discovered {
			subagents[string(d.SessionID)] = childMap[string(d.SessionID)]
		}

		roots := FilterRootSessions(discovered)
		rootCount := len(roots)
		setPhase(progress, ResolvingPhase(provider))
		setProgress(progress, 0, rootCount)
		for idx, d := range roots {
			setProgress(progress, idx+1, rootCount)
			date := d.ModTime
			if !d.CreatedAt.IsZero() {
				date = d.CreatedAt
			}
			projectName := d.ProjectName
			title := d.Title
			workingDir := d.CWD

			if provider == defaults.HarnessClaudeCode {
				slug := filepath.Base(filepath.Dir(string(d.SourcePath)))
				if decoded := DecodeClaudeSlugToPath(slug); decoded != "" && projectName == "" {
					projectName = ShortenPath(decoded)
				}
				if projectName == "" {
					projectName = ClaudeSlugToProjectName(slug)
				}
			}

			record, stored := reusable(known, d, staleness)
			if stored {
				if title == "" {
					title = record.Title
				}
				if workingDir == "" {
					workingDir = record.WorkingDirectory()
				}
			}
			// The import filter asks the same functions with the same index, so the
			// remote and the branch a session is listed under are the remote and
			// the branch it is judged by.
			gitRemote := ingest.SessionRemoteFor(ctx, git, d, record, stored)
			branchName := ingest.SessionBranchFor(ctx, git, d, record, stored)

			sessions = append(sessions, SessionListing{
				Harness:     provider.String(),
				ProjectName: projectName,
				GitRemote:   gitRemote,
				Branch:      branchName,
				Title:       title,
				Agent:       d.Agent,
				Date:        date,
				SessionID:   string(d.SessionID),
				SubagentIDs: childMap[string(d.SessionID)],
				WorkingDir:  workingDir,
				Origin:      ResolveListingOrigin(d.Origin),
				Source:      ListingSource(d),
			})
		}
		discovery.SessionCount = rootCount
		if reporter, ok := adapter.(ingest.DiscoveryDiagnosticReporter); ok {
			if diagnostics := reporter.DiscoveryDiagnostics(); len(diagnostics) > 0 {
				skipped := make([]string, 0, len(diagnostics))
				for _, diagnostic := range diagnostics {
					skipped = append(skipped, fmt.Sprintf("%s (%s)", diagnostic.Location, diagnostic.Summary))
				}
				discovery.Detail = fmt.Sprintf("%d source(s) could not be fully read: %s", len(diagnostics), strings.Join(skipped, "; "))
			}
		}
		inventory[provider] = discovery
	}
	return inventory, sessions, subagents
}

// DiscoveringPhase is what a scan says while it lists a harness's sessions. It
// is written in the lowercase chrome of the surfaces that show it: the terminal
// spinner and the setup page.
func DiscoveringPhase(harness defaults.Harness) string {
	return fmt.Sprintf("discovering %s sessions...", HarnessLabel(harness.String()))
}

// ResolvingPhase is what a scan says while it resolves each listed session's
// project.
func ResolvingPhase(harness defaults.Harness) string {
	return fmt.Sprintf("resolving %s project info...", HarnessLabel(harness.String()))
}

func reusable(known ingest.KnownSessionIndex, session ingest.DiscoveredSession, staleness time.Duration) (ingest.KnownSession, bool) {
	if known == nil {
		return ingest.KnownSession{}, false
	}
	return known.Reusable(session, staleness)
}

func setPhase(progress ScanProgress, phase string) {
	if progress == nil {
		return
	}
	progress.SetPhase(phase)
}

func setProgress(progress ScanProgress, done, total int) {
	if progress == nil {
		return
	}
	progress.SetProgress(done, total)
}

func configuredSource(cfg *config.Config, harness defaults.Harness) (ingest.SourceConfig, bool) {
	configured, ok := cfg.Sources.Provider(harness)
	if !ok {
		return ingest.SourceConfig{}, false
	}
	paths := make([]ingest.ResolvedPath, 0, len(configured.Paths))
	for _, path := range configured.Paths {
		resolved, err := ingest.NewResolvedPath(path)
		if err != nil {
			continue
		}
		paths = append(paths, resolved)
	}
	return ingest.SourceConfig{Paths: paths, Enabled: configured.Enabled}, true
}

// FilterRootSessions returns only root sessions. A subagent session is never
// selectable or ingestable on its own.
func FilterRootSessions(discovered []ingest.DiscoveredSession) []ingest.DiscoveredSession {
	var roots []ingest.DiscoveredSession
	for _, d := range discovered {
		if d.ParentUUID == nil {
			roots = append(roots, d)
		}
	}
	return roots
}

// BuildChildMap maps a parent session ID to the child session IDs discovery
// found beneath it.
func BuildChildMap(discovered []ingest.DiscoveredSession) map[string][]string {
	childMap := make(map[string][]string)
	for _, d := range discovered {
		if d.ParentUUID != nil {
			parentID := string(*d.ParentUUID)
			childMap[parentID] = append(childMap[parentID], string(d.SessionID))
		}
	}
	return childMap
}

// ExpandAllowedSessionIDs builds the allowlist from selected sessions, including
// each parent's subagent children. A nil result means no sessions were selected,
// which the pipeline treats as allow-all.
func ExpandAllowedSessionIDs(selected []SessionListing) map[ingest.SessionID]bool {
	if len(selected) == 0 {
		return nil
	}
	allowed := make(map[ingest.SessionID]bool, len(selected))
	for _, s := range selected {
		if sid, err := ingest.NewSessionID(s.SessionID); err == nil {
			allowed[sid] = true
		}
		for _, childID := range s.SubagentIDs {
			if sid, err := ingest.NewSessionID(childID); err == nil {
				allowed[sid] = true
			}
		}
	}
	return allowed
}

// ResolveListingOrigin turns discovery's typed origin into the listing value.
// Missing evidence resolves to the visible fail-safe Unknown.
func ResolveListingOrigin(discovered sessionorigin.Origin) sessionorigin.Origin {
	token := discovered.String()
	if token == "" {
		token = sessionorigin.Unknown.String()
	}
	resolved, err := sessionorigin.Parse(token)
	if err != nil {
		return sessionorigin.Unknown
	}
	return resolved
}

// DecodeClaudeSlugToPath decodes a Claude project slug to a filesystem path
// when that path is an existing directory.
func DecodeClaudeSlugToPath(encoded string) string {
	return ingest.DecodeClaudeSlugOnDisk(encoded)
}

// ShortenPath replaces the home directory prefix with "~" for display.
func ShortenPath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// ClaudeSlugToProjectName converts a Claude project slug to a readable name.
func ClaudeSlugToProjectName(slug string) string {
	if slug == "" {
		return ""
	}
	knownDirs := []string{"github", "documents", "projects", "repos", "src", "code", "dev", "home"}
	stripped := strings.TrimPrefix(slug, "-")
	segments := strings.Split(stripped, "-")
	lastKnownIdx := -1
	for i, seg := range segments {
		for _, known := range knownDirs {
			if strings.EqualFold(seg, known) {
				lastKnownIdx = i
				break
			}
		}
	}
	if lastKnownIdx >= 0 && lastKnownIdx < len(segments)-1 {
		return strings.Join(segments[lastKnownIdx+1:], "-")
	}
	return slug
}
