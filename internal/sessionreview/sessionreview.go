// Package sessionreview finds what redaction leaves behind in a session's
// entries. It only reports: it never produces replacement text, and a finding
// never carries the matched value, only a masked preview.
//
// A pattern that should be redacted belongs in github.com/peasant-labs/redact.
// When such a rule ships, the matching detector here is deleted.
package sessionreview

import (
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// FindingKind names what a detector found. It is a closed set.
type FindingKind string

const (
	// KindCredentialAssignment is a secret-named value that no rule redacted.
	KindCredentialAssignment FindingKind = "credential_assignment"
	// KindCredentialFileOutput is a tool result that printed credentials.
	KindCredentialFileOutput FindingKind = "credential_file_output"
	// KindForeignEmail is an address that survived redaction and is not the
	// owner's.
	KindForeignEmail FindingKind = "foreign_email"
	// KindShareableDocumentLink is a URL that grants access to anyone holding
	// it.
	KindShareableDocumentLink FindingKind = "shareable_document_link"
	// KindRedactionResidue is an advisory hit from the redact module's own
	// residue rules.
	KindRedactionResidue FindingKind = "redaction_residue"
)

// AllFindingKinds is the closed set of finding kinds.
var AllFindingKinds = []FindingKind{
	KindCredentialAssignment,
	KindCredentialFileOutput,
	KindForeignEmail,
	KindShareableDocumentLink,
	KindRedactionResidue,
}

// NewFindingKind validates raw against the closed set.
func NewFindingKind(raw string) (FindingKind, error) {
	kind := FindingKind(raw)
	if !slices.Contains(AllFindingKinds, kind) {
		return "", fmt.Errorf("unknown finding kind %q", raw)
	}
	return kind, nil
}

// Rule ids of the detectors. A residue finding carries the redact rule id
// instead, and a link finding carries its host table entry's Rule.
const (
	ruleCredentialNameValue = "credential_name_value"
	ruleCredentialFileName  = "credential_file_name"
	ruleSecretEnvListing    = "secret_env_listing"
	ruleForeignEmail        = "foreign_email"
)

// redactResidueEmailRule is left out of the passthrough: an address that
// survives redaction is reported by foreign_email, which knows the owner set.
const redactResidueEmailRule = "residue_email"

// Finding is one thing a detector found. Preview is masked and at most
// redact.PreviewMaxLength bytes.
type Finding struct {
	Kind       FindingKind
	EntryIndex int
	Role       schema.Role
	Rule       string
	Preview    string
}

// Options configures a review.
type Options struct {
	// OwnerEmails are the addresses that count as the owner's.
	OwnerEmails []string
}

// Detector thresholds.
const (
	minAssignmentValueLength = 20
	minAssignmentEntropy     = 3.5
	minEnvListingLines       = 2
	minEnvListingValueLength = 12
	maxPreviewNameLength     = 20
	maskedName               = "<name>"
	masked                   = "****"
)

// secretNameParts is the closed list of words that make a name secret-shaped.
var secretNameParts = []string{"key", "secret", "token", "password", "credential"}

// CredentialFileNames is the closed list of file names whose content is a
// credential. Names are compared in lower case.
var CredentialFileNames = []string{
	".credentials.json",
	".env",
	".netrc",
	".npmrc",
	".pgpass",
	".pypirc",
	"application_default_credentials.json",
	"auth.json",
	"credentials",
	"credentials.json",
	"id_ed25519",
	"id_rsa",
	"secrets.json",
	"secrets.yaml",
	"secrets.yml",
	"service-account.json",
	"service_account.json",
	"token.json",
}

// ShareableHost is one document host whose URLs of a path shape grant access
// to anyone holding the link. The first path segment of every shape is a fixed
// word, so the masked preview never shows a document id.
type ShareableHost struct {
	Rule string
	// Host is the exact host, or a suffix when it starts with a dot.
	Host string
	// Path matches the URL path and query.
	Path *regexp.Regexp
}

// ShareableHosts is the closed list of link-shareable document hosts.
var ShareableHosts = []ShareableHost{
	{Rule: "share_google_docs", Host: "docs.google.com", Path: regexp.MustCompile(`^/(document|spreadsheets|presentation|forms)/d/[^/?]+`)},
	{Rule: "share_google_drive", Host: "drive.google.com", Path: regexp.MustCompile(`^/(file/d/[^/?]+|drive/folders/[^/?]+|open\?(.*&)?id=)`)},
	{Rule: "share_dropbox", Host: "dropbox.com", Path: regexp.MustCompile(`^/(s|sh|scl/fi|scl/fo)/[^/?]+`)},
	{Rule: "share_dropbox", Host: "www.dropbox.com", Path: regexp.MustCompile(`^/(s|sh|scl/fi|scl/fo)/[^/?]+`)},
	{Rule: "share_onedrive", Host: "1drv.ms", Path: regexp.MustCompile(`^/[a-z](/[a-z])?/[^/?]+`)},
	{Rule: "share_sharepoint", Host: ".sharepoint.com", Path: regexp.MustCompile(`^/:[a-z]:/`)},
	{Rule: "share_box", Host: "app.box.com", Path: regexp.MustCompile(`^/s/[^/?]+`)},
}

var (
	assignmentPattern = regexp.MustCompile(`["']?([A-Za-z][A-Za-z0-9_.-]{0,63})["']?\s*[:=]\s*["']?([A-Za-z0-9+/=_.~-]{20,})`)
	envLinePattern    = regexp.MustCompile(`(?m)^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(\S+)\s*$`)
	// The address pattern is wider than the redact rule on purpose: it holds
	// any letter, so it sees the addresses that rule cannot match.
	emailPattern    = regexp.MustCompile(`[\p{L}\p{N}._%+\-]+@[\p{L}\p{N}.\-]+\.\p{L}{2,}`)
	urlPattern      = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)
	pathTokenSplits = regexp.MustCompile(`[\s"'=:,;()]+`)
)

// span is a byte range of a redacted text that a detector has reported.
type span struct{ start, end int }

// reviewer holds one Review call's state.
type reviewer struct {
	redactor redact.Redactor
	residue  redact.ResidueDetector
	owners   map[string]bool
	seen     map[Finding]bool
	findings []Finding
}

// Review scans entries after r has redacted them. It never returns matched
// text, and it keeps the redacted text in memory only.
func Review(entries []schema.SessionEntry, r redact.Redactor, opts Options) []Finding {
	rv := &reviewer{
		redactor: r,
		residue:  redact.NewResidueDetector(),
		owners:   make(map[string]bool, len(opts.OwnerEmails)),
		seen:     map[Finding]bool{},
	}
	for _, email := range opts.OwnerEmails {
		rv.owners[strings.ToLower(strings.TrimSpace(email))] = true
	}
	calls := rv.toolCalls(entries)
	for i := range entries {
		rv.reviewEntry(&entries[i], calls)
	}
	return rv.findings
}

// toolCalls maps each tool call id to the redacted input of the call, so a
// result can be paired with the call that produced it.
func (rv *reviewer) toolCalls(entries []schema.SessionEntry) map[string]schema.SessionEntry {
	calls := map[string]schema.SessionEntry{}
	for _, entry := range entries {
		if entry.ToolCallID == nil || entry.ToolInput == nil {
			continue
		}
		redacted := rv.redactor.RedactText(*entry.ToolInput)
		entry.ToolInput = &redacted
		calls[*entry.ToolCallID] = entry
	}
	return calls
}

func (rv *reviewer) reviewEntry(entry *schema.SessionEntry, calls map[string]schema.SessionEntry) {
	if entry.ContentPreview != nil {
		rv.scan(entry, rv.redactor.RedactText(*entry.ContentPreview), entry.Role == schema.RoleUser, false)
	}
	if entry.ToolInput != nil {
		rv.scan(entry, rv.redactor.RedactText(*entry.ToolInput), false, false)
	}
	if entry.ToolOutput == nil {
		return
	}
	output := rv.redactor.RedactText(*entry.ToolOutput)
	covered := false
	if finding, ok := credentialFileFinding(entry, output, calls); ok {
		rv.add(finding)
		covered = true
	}
	rv.scan(entry, output, true, covered)
}

// scan runs the text detectors on one redacted text. covered says a finding
// already accounts for the whole text, so its secret-shaped lines are not
// reported a second time.
func (rv *reviewer) scan(entry *schema.SessionEntry, text string, scanEmails, covered bool) {
	if text == "" {
		return
	}
	var spans []span
	if !covered {
		spans = append(spans, rv.assignments(entry, text)...)
	}
	if scanEmails {
		rv.emails(entry, text)
	}
	spans = append(spans, rv.links(entry, text)...)
	if !covered {
		rv.residuePassthrough(entry, text, spans)
	}
}

func (rv *reviewer) add(finding Finding) {
	finding.Preview = clampPreview(finding.Preview)
	if rv.seen[finding] {
		return
	}
	rv.seen[finding] = true
	rv.findings = append(rv.findings, finding)
}

func newFinding(entry *schema.SessionEntry, kind FindingKind, rule, preview string) Finding {
	return Finding{Kind: kind, EntryIndex: entry.EntryIndex, Role: entry.Role, Rule: rule, Preview: preview}
}

func (rv *reviewer) assignments(entry *schema.SessionEntry, text string) []span {
	var spans []span
	for _, match := range assignmentPattern.FindAllStringSubmatchIndex(text, -1) {
		name, value := text[match[2]:match[3]], text[match[4]:match[5]]
		if !secretNamed(name) || !secretShapedValue(value) {
			continue
		}
		preview := previewName(name) + "=" + masked + " " + lengthClass(len(value))
		rv.add(newFinding(entry, KindCredentialAssignment, ruleCredentialNameValue, preview))
		spans = append(spans, span{match[0], match[1]})
	}
	return spans
}

func secretNamed(name string) bool {
	lower := strings.ToLower(name)
	return slices.ContainsFunc(secretNameParts, func(part string) bool { return strings.Contains(lower, part) })
}

// secretShapedValue is long, mixes letters and digits, and is high in entropy.
func secretShapedValue(value string) bool {
	if len(value) < minAssignmentValueLength || shannonEntropy(value) < minAssignmentEntropy {
		return false
	}
	return strings.ContainsAny(value, "0123456789") && strings.IndexFunc(value, unicode.IsLetter) >= 0
}

func shannonEntropy(value string) float64 {
	counts := map[rune]int{}
	total := 0
	for _, r := range value {
		counts[r]++
		total++
	}
	entropy := 0.0
	for _, count := range counts {
		p := float64(count) / float64(total)
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// previewName is the name of an assignment as a preview may show it. A long or
// random-looking name may itself be a value, so it is replaced.
func previewName(name string) string {
	if len(name) > maxPreviewNameLength || shannonEntropy(name) >= minAssignmentEntropy+0.5 {
		return maskedName
	}
	return name
}

func lengthClass(length int) string {
	switch {
	case length < 32:
		return "(len<32)"
	case length < 64:
		return "(len 32-63)"
	default:
		return "(len 64+)"
	}
}

// credentialFileFinding reports a tool result that printed credentials: its
// call read a credential-shaped file, or the output lists secret-named values.
func credentialFileFinding(entry *schema.SessionEntry, output string, calls map[string]schema.SessionEntry) (Finding, bool) {
	if entry.IsError || strings.TrimSpace(output) == "" {
		return Finding{}, false
	}
	if entry.ToolCallID != nil {
		if call, ok := calls[*entry.ToolCallID]; ok {
			if name, found := credentialFileRead(call); found {
				return newFinding(entry, KindCredentialFileOutput, ruleCredentialFileName, name+": content="+masked), true
			}
		}
	}
	if lines, ok := secretEnvListing(output); ok {
		return newFinding(entry, KindCredentialFileOutput, ruleSecretEnvListing, fmt.Sprintf("%d secret-named lines=%s", lines, masked)), true
	}
	return Finding{}, false
}

// credentialFileRead returns the credential file name a tool call read. A call
// that writes a file does not print it, so it never counts.
func credentialFileRead(call schema.SessionEntry) (string, bool) {
	if call.ToolInput == nil {
		return "", false
	}
	if call.ToolKind != nil && (*call.ToolKind == schema.ToolCallKindEdit || *call.ToolKind == schema.ToolCallKindDelete) {
		return "", false
	}
	for _, token := range pathTokenSplits.Split(*call.ToolInput, -1) {
		base := strings.ToLower(path.Base(strings.ReplaceAll(token, `\`, "/")))
		if slices.Contains(CredentialFileNames, base) {
			return base, true
		}
	}
	return "", false
}

// secretEnvListing counts the secret-named NAME=value lines of an output. A
// redacted value still counts as a line, but the listing is reported only when
// a value survived redaction: otherwise nothing is left to find.
func secretEnvListing(output string) (int, bool) {
	lines, survived := 0, false
	for _, match := range envLinePattern.FindAllStringSubmatch(output, -1) {
		if !secretNamed(match[1]) {
			continue
		}
		lines++
		if len(match[2]) >= minEnvListingValueLength && !strings.ContainsAny(match[2], "<>") {
			survived = true
		}
	}
	return lines, lines >= minEnvListingLines && survived
}

func (rv *reviewer) emails(entry *schema.SessionEntry, text string) {
	for _, address := range emailPattern.FindAllString(text, -1) {
		lower := strings.ToLower(address)
		if rv.owners[lower] || strings.HasPrefix(lower, "git@") {
			continue
		}
		rv.add(newFinding(entry, KindForeignEmail, ruleForeignEmail, maskEmail(address)))
	}
}

func maskEmail(address string) string {
	local, domain, _ := strings.Cut(address, "@")
	first, _ := utf8.DecodeRuneInString(local)
	return string(first) + "***@" + domain
}

func (rv *reviewer) links(entry *schema.SessionEntry, text string) []span {
	var spans []span
	for _, loc := range urlPattern.FindAllStringIndex(text, -1) {
		parsed, err := url.Parse(strings.TrimRight(text[loc[0]:loc[1]], ".,;"))
		if err != nil {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		target := parsed.EscapedPath()
		if parsed.RawQuery != "" {
			target += "?" + parsed.RawQuery
		}
		index := slices.IndexFunc(ShareableHosts, func(h ShareableHost) bool { return h.matches(host, target) })
		if index < 0 {
			continue
		}
		rv.add(newFinding(entry, KindShareableDocumentLink, ShareableHosts[index].Rule, host+"/"+firstSegment(parsed.EscapedPath())+"/"+masked))
		spans = append(spans, span{loc[0], loc[1]})
	}
	return spans
}

func (h ShareableHost) matches(host, target string) bool {
	if strings.HasPrefix(h.Host, ".") {
		return strings.HasSuffix(host, h.Host) && h.Path.MatchString(target)
	}
	return host == h.Host && h.Path.MatchString(target)
}

func firstSegment(escapedPath string) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(escapedPath, "/"), "/")
	return segment
}

// residuePassthrough reports each residue rule that hit. The module's own
// preview is context around the match, so it is never printed: the finding
// shows the rule alone.
func (rv *reviewer) residuePassthrough(entry *schema.SessionEntry, text string, spans []span) {
	for _, warning := range rv.residue.Scan(text) {
		if warning.RuleID == redactResidueEmailRule {
			continue
		}
		if slices.ContainsFunc(spans, func(s span) bool { return warning.Location >= s.start && warning.Location < s.end }) {
			continue
		}
		rv.add(newFinding(entry, KindRedactionResidue, warning.RuleID, warning.RuleID+": "+masked))
	}
}

// clampPreview cuts a preview to redact.PreviewMaxLength bytes on a rune
// boundary.
func clampPreview(preview string) string {
	if len(preview) <= redact.PreviewMaxLength {
		return preview
	}
	end := redact.PreviewMaxLength
	for end > 0 && !utf8.RuneStart(preview[end]) {
		end--
	}
	return preview[:end]
}
