package service

import (
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The external web_search definition triggers refusals on the Office text
// bridge. Disable only that tool; preserve prompts, history and other tools.
// Use the resulting body for both inference and tool response validation.
func filterOfficeCopilotTools(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("invalid JSON request")
	}
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && tools.Type != gjson.Null && !tools.IsArray() {
		return nil, fmt.Errorf("Office Copilot tools must be an array")
	}
	choice := gjson.GetBytes(body, "tool_choice")
	forcedName := choice.Get("name").String()
	if choice.Get("type").String() == "function" {
		forcedName = choice.Get("function.name").String()
	}
	if forcedName == "web_search" {
		return nil, fmt.Errorf("Office Copilot does not support the external web_search tool; remove the forced tool_choice or use another provider")
	}
	kept := make([]json.RawMessage, 0, len(tools.Array()))
	removed := false
	for _, tool := range tools.Array() {
		name := tool.Get("name").String()
		if name == "" {
			name = tool.Get("function.name").String()
		}
		if name == "web_search" {
			removed = true
			continue
		}
		kept = append(kept, json.RawMessage(tool.Raw))
	}
	if !removed {
		return body, nil
	}
	if len(kept) == 0 {
		filtered, err := sjson.DeleteBytes(body, "tools")
		if err != nil {
			return nil, err
		}
		return sjson.DeleteBytes(filtered, "tool_choice")
	}
	return sjson.SetBytes(body, "tools", kept)
}
