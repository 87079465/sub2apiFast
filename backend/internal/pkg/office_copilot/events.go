package office_copilot

type EventKind string

const (
	EventText      EventKind = "text"
	EventReasoning EventKind = "reasoning"
	EventProgress  EventKind = "progress"
	EventTool      EventKind = "tool"
)

// StreamEvent contains only explicitly public assistant text or summaries.
// Office-native tool invocations are handled separately by unavailableLocalPlugin.
type StreamEvent struct {
	Kind EventKind `json:"kind"`
	Text string    `json:"text"`
}

func classifyMessage(m chatHubMessage) EventKind {
	if m.Author != "bot" {
		return ""
	}
	if m.ContentType == "ToolCall" || m.MessageType == "ToolCall" || m.MessageType == "TriggerPlugin" {
		return EventTool
	}
	if m.ContentOrigin == "ChainOfThoughtSummary" || m.AddToChainOfThought {
		return EventReasoning
	}
	if m.ContentType == "SearchResults" || m.ContentType == "Code" {
		return EventProgress
	}
	if m.MessageType == "" || m.MessageType == "Chat" {
		return EventText
	}
	return EventProgress
}
