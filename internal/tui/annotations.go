package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/peasant-labs/peasant/internal/defaults"
	schema "github.com/peasant-labs/schema"
)

// annotationBadgeStyle returns a Lipgloss style for an annotation badge using
// the Okabe-Ito colorblind-safe palette, cycling by the given index.
func annotationBadgeStyle(idx int) lipgloss.Style {
	color := defaults.AnnotationBadgeColors[idx%len(defaults.AnnotationBadgeColors)]
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(defaults.ColorDark.String())).
		Background(lipgloss.Color(color.String())).
		Bold(true).
		Padding(0, 1)
}

// renderAnnotationBadge renders a single annotation as a colored inline badge.
// Format: "[TypeName confidence]" e.g. "[Frustration Signal 0.82]" or "[Session Outcome]".
// Falls back to TypeID when TypeName is empty.
func renderAnnotationBadge(ann schema.AnnotationSummary, colorIdx int) string {
	label := ann.TypeID
	if ann.TypeName != "" {
		label = ann.TypeName
	}
	text := label
	if ann.Confidence != nil {
		text = fmt.Sprintf("%s %.2f", label, *ann.Confidence)
	}
	return annotationBadgeStyle(colorIdx).Render("[" + text + "]")
}

// renderAnnotationBadges renders a space-separated row of annotation badges.
// Returns an empty string when no annotations are provided.
func renderAnnotationBadges(annotations []schema.AnnotationSummary) string {
	if len(annotations) == 0 {
		return ""
	}
	badges := make([]string, len(annotations))
	for i, ann := range annotations {
		badges[i] = renderAnnotationBadge(ann, i)
	}
	return strings.Join(badges, " ")
}

// annotationsForEntry returns annotations whose target matches the given entry index.
// Session-level annotations (TargetEntryIndex == nil) are not included.
func annotationsForEntry(annotations []schema.AnnotationSummary, entryIdx int) []schema.AnnotationSummary {
	var result []schema.AnnotationSummary
	for _, ann := range annotations {
		if ann.TargetEntryIndex != nil && *ann.TargetEntryIndex == entryIdx {
			result = append(result, ann)
		}
	}
	return result
}

// sessionLevelAnnotations returns annotations that target the session as a whole
// (TargetEntryIndex == nil and TargetKind is not entry-level).
func sessionLevelAnnotations(annotations []schema.AnnotationSummary) []schema.AnnotationSummary {
	var result []schema.AnnotationSummary
	for _, ann := range annotations {
		if ann.TargetEntryIndex == nil {
			result = append(result, ann)
		}
	}
	return result
}
