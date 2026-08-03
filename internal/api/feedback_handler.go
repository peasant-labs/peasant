package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

const (
	feedbackDirName       = "llm"
	feedbackFileName      = "ui-feedback.md"
	feedbackCommentMaxLen = 4000
	feedbackSnippetMaxLen = 800
)

type feedbackRequest struct {
	Route    string `json:"route"`
	Anchor   string `json:"anchor"`
	Selector string `json:"selector"`
	Snippet  string `json:"snippet"`
	Comment  string `json:"comment"`
}

// handleLocalFeedback appends a markdown block describing a UI annotation to
// llm/ui-feedback.md at the nearest git repo root (or cwd if not inside a
// repo). Always available because the binary is local-first by design — the
// dashboard's floating annotator POSTs here.
func (s *Server) handleLocalFeedback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	comment := strings.TrimSpace(req.Comment)
	if comment == "" {
		http.Error(w, `{"error":"comment required"}`, http.StatusBadRequest)
		return
	}
	if len(comment) > feedbackCommentMaxLen {
		comment = comment[:feedbackCommentMaxLen]
	}

	target, err := resolveFeedbackPath()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"resolve path: %v"}`, err), http.StatusInternalServerError)
		return
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"mkdir: %v"}`, err), http.StatusInternalServerError)
		return
	}

	block := formatFeedbackBlock(req, comment)

	f, err := os.OpenFile(target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"open: %v"}`, err), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"write: %v"}`, err), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": target})
}

// resolveFeedbackPath picks <git-root>/llm/ui-feedback.md if cwd is inside a
// git repo, otherwise <cwd>/llm/ui-feedback.md.
func resolveFeedbackPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return filepath.Join(root, feedbackDirName, feedbackFileName), nil
		}
	}
	return filepath.Join(cwd, feedbackDirName, feedbackFileName), nil
}

func formatFeedbackBlock(req feedbackRequest, comment string) string {
	ts := time.Now().Format("2006-01-02 15:04")

	route := req.Route
	if route == "" {
		route = "/"
	}
	anchor := req.Anchor
	if anchor == "" {
		anchor = "(no anchor)"
	}
	selector := req.Selector
	if selector == "" {
		selector = "(no selector)"
	}

	var quoted strings.Builder
	for i, line := range strings.Split(comment, "\n") {
		if i > 0 {
			quoted.WriteByte('\n')
		}
		quoted.WriteString("> ")
		quoted.WriteString(line)
	}

	snippet := req.Snippet
	if len(snippet) > feedbackSnippetMaxLen {
		snippet = snippet[:feedbackSnippetMaxLen]
	}
	snippetBlock := ""
	if snippet != "" {
		snippetBlock = fmt.Sprintf("\n<details><summary>DOM snippet</summary>\n\n```html\n%s\n```\n</details>\n", snippet)
	}

	return fmt.Sprintf("\n## %s — `%s`\n**Anchor:** %s\n**Selector:** `%s`\n\n%s\n%s\n---\n",
		ts, route, anchor, selector, quoted.String(), snippetBlock)
}
