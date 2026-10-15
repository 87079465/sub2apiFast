package service

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
)

type officeCopilotToolEnvelope struct {
	Text  string `json:"text"`
	Calls []struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_calls"`
}

// Format errors are eligible for one upstream formatting retry. Validation
// errors (undeclared tools / tool_choice violations) are never repaired.
type officeCopilotToolFormatError struct{}

func (*officeCopilotToolFormatError) Error() string {
	return "Office Copilot returned an invalid tool response; no tool calls were delivered"
}

var officeCopilotToolCallsKey = regexp.MustCompile(`"tool_calls"\s*:`)

func officeCopilotToolRepairRequest(request office_copilot.InboundRequest, invalidOutput string) office_copilot.InboundRequest {
	// Copy before appending so shared request/history slices stay untouched.
	request.Messages = append([]office_copilot.InboundMessage(nil), request.Messages...)
	request.Messages = append(request.Messages,
		office_copilot.InboundMessage{Role: "assistant", Content: invalidOutput},
		office_copilot.InboundMessage{Role: "user", Content: "The API could not decode the previous response. No tool calls from that response were delivered or executed. Re-encode it as one valid JSON object with exactly text (a string) and tool_calls (an array of name and arguments objects). Put explanations in text. Preserve the intended arguments and commands, correctly JSON-escaping quotes, backslashes and newlines; do not add operations or claim execution. Use only the declared tools and obey tool_choice. If the previous text was only an example or explanation, put it in text and use an empty tool_calls array. Return the JSON object only."},
	)
	return request
}

func decodeOfficeCopilotToolEnvelope(raw string) (officeCopilotToolEnvelope, error) {
	var envelope officeCopilotToolEnvelope
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || len(fields) != 2 || fields["text"] == nil || fields["tool_calls"] == nil {
		return envelope, &officeCopilotToolFormatError{}
	}
	if strings.TrimSpace(string(fields["text"])) == "null" || !strings.HasPrefix(strings.TrimSpace(string(fields["tool_calls"])), "[") {
		return envelope, &officeCopilotToolFormatError{}
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil {
		return envelope, &officeCopilotToolFormatError{}
	}
	return envelope, nil
}

func officeCopilotJSONFence(line string) bool {
	line = strings.TrimSpace(line)
	return line == "```" || strings.EqualFold(line, "```json")
}

// Extract only a single terminal envelope, at an object/line boundary. Never
// search inside decoded objects, tool arguments, quoted strings or summaries,
// and never repair JSON/shell quoting heuristically.
func extractOfficeCopilotToolEnvelope(output string) (officeCopilotToolEnvelope, error) {
	output = strings.TrimSpace(strings.TrimPrefix(output, "\ufeff"))
	if first, rest, ok := strings.Cut(output, "\n"); ok && officeCopilotJSONFence(first) && strings.HasSuffix(rest, "\n```") {
		output = strings.TrimSpace(strings.TrimSuffix(rest, "\n```"))
	}
	if envelope, err := decodeOfficeCopilotToolEnvelope(output); err == nil {
		return envelope, nil
	}
	if output == "" || strings.HasPrefix(output, "{") || strings.HasPrefix(output, "[") {
		return officeCopilotToolEnvelope{}, &officeCopilotToolFormatError{}
	}
	// A plain refusal/final answer is still useful. The tool_calls property is
	// the discriminator: prose merely mentioning a tool name is not a command.
	if !officeCopilotToolCallsKey.MatchString(output) {
		return officeCopilotToolEnvelope{Text: output}, nil
	}
	for offset, attempts := 0, 0; offset < len(output) && attempts < 32; {
		next := strings.IndexByte(output[offset:], '\n')
		if next < 0 {
			next = len(output)
		} else {
			next += offset
		}
		line := output[offset:next]
		trimmed := strings.TrimLeft(line, " \t\r")
		start := offset + len(line) - len(trimmed)
		offset = next + 1
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		attempts++
		decoder := json.NewDecoder(strings.NewReader(output[start:]))
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			// Do not salvage an inner call from a broken outer object.
			return officeCopilotToolEnvelope{}, &officeCopilotToolFormatError{}
		}
		end := start + int(decoder.InputOffset())
		if end > next {
			offset = end
		}
		prefix, suffix := strings.TrimSpace(output[:start]), strings.TrimSpace(output[end:])
		lastLine := strings.LastIndexByte(prefix, '\n') + 1
		if officeCopilotJSONFence(prefix[lastLine:]) {
			if suffix != "```" {
				continue
			}
			prefix = strings.TrimSpace(prefix[:lastLine])
		} else if suffix != "" {
			continue
		}
		if officeCopilotToolCallsKey.MatchString(prefix) {
			return officeCopilotToolEnvelope{}, &officeCopilotToolFormatError{}
		}
		envelope, err := decodeOfficeCopilotToolEnvelope(string(raw))
		if err != nil {
			continue
		}
		if prefix != "" && prefix != strings.TrimSpace(envelope.Text) {
			if envelope.Text == "" {
				envelope.Text = prefix
			} else {
				envelope.Text = prefix + "\n\n" + envelope.Text
			}
		}
		return envelope, nil
	}
	return officeCopilotToolEnvelope{}, &officeCopilotToolFormatError{}
}
