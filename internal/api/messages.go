package api

import (
	"time"

	schema "github.com/peasant-labs/schema"
)

// --- Type aliases migrated to the github.com/peasant-labs/schema module ---
// These aliases maintain backward compatibility: existing code using api.X
// continues to work while the canonical definitions now live in that module.

// MessageType represents the type discriminator for WebSocket messages.
type MessageType = schema.MessageType

const (
	MsgSubscribe          = schema.MsgSubscribe
	MsgUnsubscribe        = schema.MsgUnsubscribe
	MsgDashboard          = schema.MsgDashboard
	MsgSessions           = schema.MsgSessions
	MsgSessionDetail      = schema.MsgSessionDetail
	MsgTrends             = schema.MsgTrends
	MsgQuality            = schema.MsgQuality
	MsgAnnotations        = schema.MsgAnnotations
	MsgProjectFamiliarity = schema.MsgProjectFamiliarity
	MsgConnected          = schema.MsgConnected
	MsgError              = schema.MsgError
)

// ChannelTopic identifies a subscribable data stream.
type ChannelTopic = schema.ChannelTopic

const (
	TopicDashboard          = schema.TopicDashboard
	TopicSessions           = schema.TopicSessions
	TopicSessionDetail      = schema.TopicSessionDetail
	TopicTrends             = schema.TopicTrends
	TopicQuality            = schema.TopicQuality
	TopicAnnotations        = schema.TopicAnnotations
	TopicProjectFamiliarity = schema.TopicProjectFamiliarity
)

// AnnotationAxis is the subscription dimension for annotation channels.
type AnnotationAxis = schema.AnnotationAxis

const (
	AxisType    = schema.AxisType
	AxisSession = schema.AxisSession
	AxisProject = schema.AxisProject
)

// ChannelSubscription describes a single channel subscription.
type ChannelSubscription = schema.ChannelSubscription

// ValidChannels is the set of valid channel topics for subscription.
var ValidChannels = map[ChannelTopic]bool{
	TopicDashboard:          true,
	TopicSessions:           true,
	TopicSessionDetail:      true,
	TopicTrends:             true,
	TopicQuality:            true,
	TopicAnnotations:        true,
	TopicProjectFamiliarity: true,
}

// ValidateSubscription returns true if sub.Topic is known and all required
// topic-specific fields are present.
func ValidateSubscription(sub ChannelSubscription) bool {
	if !ValidChannels[sub.Topic] {
		return false
	}
	switch sub.Topic {
	case TopicSessionDetail:
		return sub.ID != ""
	case TopicAnnotations:
		return sub.Axis != "" && sub.ID != ""
	case TopicProjectFamiliarity:
		return sub.ID != ""
	default:
		return true
	}
}

// ClientMessage is a message sent from the browser to the server.
type ClientMessage = schema.ClientMessage

// ServerMessage is a message sent from the server to the browser.
type ServerMessage = schema.ServerMessage

// DashboardPayload is the data sent on the dashboard channel.
type DashboardPayload = schema.DashboardPayload

// SessionSummary is a session without turns, used in the sessions list.
type SessionSummary = schema.SessionSummary

// SessionsPayload is the data sent on the sessions channel.
type SessionsPayload = schema.SessionsPayload

// TurnDetail is a turn with full content for the detail view.
type TurnDetail = schema.TurnDetail

// ToolCallDetail is a tool call in the detail view.
type ToolCallDetail = schema.ToolCallDetail

// SessionDetailPayload is the data sent on the session_detail channel.
type SessionDetailPayload = schema.SessionDetailPayload

// ChildSessionRef is a lightweight reference to a child (subagent) session.
type ChildSessionRef = schema.ChildSessionRef

// DayStats holds aggregated data for a single day in trends.
type DayStats = schema.DayStats

// TrendsPayload is the data sent on the trends channel.
type TrendsPayload = schema.TrendsPayload

// QualityPayload is the data sent on the quality channel.
type QualityPayload = schema.QualityPayload

// QualitySession is the analytics payload for the quality dashboard.
type QualitySession = schema.QualitySession

// AnnotationsPayload is the data sent on the annotations channel.
type AnnotationsPayload = schema.AnnotationsPayload

// FamiliarityPayload is the data sent on the project_familiarity channel.
type FamiliarityPayload = schema.FamiliarityPayload

// FileFamiliarity represents familiarity data for a single file.
type FileFamiliarity = schema.FileFamiliarity

// WalkthroughTrail is the file path extracted from a single session.
type WalkthroughTrail = schema.WalkthroughTrail

// WalkthroughStep is a single file visit in a session trail.
type WalkthroughStep = schema.WalkthroughStep

// ReviewSuggestion nudges the user to revisit a fading file.
type ReviewSuggestion = schema.ReviewSuggestion

// ProjectSummariesPayload backs the home project picker.
type ProjectSummariesPayload = schema.ProjectSummariesPayload

// ProjectSummary is one row of the home project picker.
type ProjectSummary = schema.ProjectSummary

// MapGraphPayload is the full map graph for one project (Map surface).
type MapGraphPayload = schema.MapGraphPayload

// MapNodeDetailPayload backs the Map node rail panel.
type MapNodeDetailPayload = schema.MapNodeDetailPayload

// ProjectTasksPayload backs the Map Tasks lens.
type ProjectTasksPayload = schema.ProjectTasksPayload

// ReviewListPayload lists a project's changes (Review surface).
type ReviewListPayload = schema.ReviewListPayload

// ChangeDetailPayload backs the Review change-detail surface.
type ChangeDetailPayload = schema.ChangeDetailPayload

// QualityFilter constrains which sessions are returned for quality analysis.
type QualityFilter struct {
	Projects  []string   // empty = all projects
	DateRange *DateRange // nil = no date filtering
}

// DateRange specifies a half-open time range [Start, End) for filtering.
type DateRange struct {
	Start time.Time
	End   time.Time
}
