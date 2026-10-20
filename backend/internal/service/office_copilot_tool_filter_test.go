package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestOfficeCopilotToolFilter(t *testing.T) {
	for _, tc := range []struct {
		name, tools, choice string
		remaining           int
		wantError           bool
	}{
		{"anthropic", `[{"name":"web_search"},{"name":"shell","input_schema":{"type":"object"},"custom":42}]`, `{"type":"any"}`, 1, false},
		{"openai", `[{"type":"function","function":{"name":"web_search"}},{"type":"function","function":{"name":"shell","parameters":{"type":"object"}},"custom":42}]`, `"required"`, 1, false},
		{"last tool", `[{"name":"web_search"}]`, `{"type":"any"}`, 0, false},
		{"last openai tool", `[{"type":"function","function":{"name":"web_search"}}]`, `"required"`, 0, false},
		{"forced anthropic", `[{"name":"web_search"}]`, `{"type":"tool","name":"web_search"}`, 0, true},
		{"forced openai", `[{"type":"function","function":{"name":"web_search"}}]`, `{"type":"function","function":{"name":"web_search"}}`, 0, true},
		{"force remaining", `[{"name":"web_search"},{"name":"shell"}]`, `{"type":"tool","name":"shell"}`, 1, false},
		{"similar name", `[{"name":"web_search_preview"}]`, `"auto"`, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-opus-5-5","system":"keep system","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"old","name":"web_search","input":{"query":"old"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"old","content":"prior search result"},{"type":"text","text":"你好"}]}],"metadata":{"keep":true},"tools":` + tc.tools + `,"tool_choice":` + tc.choice + `}`)
			original := bytes.Clone(body)
			filtered, err := filterOfficeCopilotTools(body)
			if !bytes.Equal(body, original) {
				t.Fatal("mutated input")
			}
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "web_search") {
					t.Fatalf("expected explicit unsupported tool error, got %v", err)
				}
				if _, err := officeCopilotRequest(body, true); err == nil {
					t.Fatal("conversion accepted forced disabled tool")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"system", "messages", "metadata"} {
				if gjson.GetBytes(filtered, path).Raw != gjson.GetBytes(body, path).Raw {
					t.Fatalf("changed %s", path)
				}
			}
			remaining := gjson.GetBytes(filtered, "tools").Array()
			if len(remaining) != tc.remaining {
				t.Fatalf("remaining tools: %s", filtered)
			}
			if tc.remaining == 0 {
				if gjson.GetBytes(filtered, "tools").Exists() || gjson.GetBytes(filtered, "tool_choice").Exists() || officeCopilotToolInstruction(filtered) != "" {
					t.Fatal("dangling tools or choice")
				}
			} else {
				if gjson.GetBytes(filtered, "tool_choice").Raw != tc.choice {
					t.Fatal("changed remaining tool choice")
				}
				originalTools := gjson.Parse(tc.tools).Array()
				var a, b any
				json.Unmarshal([]byte(originalTools[len(originalTools)-1].Raw), &a)
				json.Unmarshal([]byte(remaining[0].Raw), &b)
				aj, _ := json.Marshal(a)
				bj, _ := json.Marshal(b)
				if !bytes.Equal(aj, bj) {
					t.Fatal("changed retained definition")
				}
			}
			again, err := filterOfficeCopilotTools(filtered)
			if err != nil || !bytes.Equal(again, filtered) {
				t.Fatal("filter not idempotent")
			}
			request, err := officeCopilotRequest(body, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(gjson.ParseBytes(request.Tools).Array()) != tc.remaining {
				t.Fatal("conversion tools differ")
			}
			if _, err := parseOfficeCopilotToolResponse(`{"text":"","tool_calls":[{"name":"web_search","arguments":{"query":"test"}}]}`, filtered); err == nil {
				t.Fatal("accepted disabled tool call")
			}
			if tc.remaining > 0 && tc.name != "similar name" {
				if _, err := parseOfficeCopilotToolResponse(`{"text":"","tool_calls":[{"name":"shell","arguments":{}}]}`, filtered); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestOfficeCopilotToolFilterValidationAndNoop(t *testing.T) {
	for _, body := range []string{`{`, `{"tools":{}}`, `{"tools":"web_search"}`} {
		if _, err := filterOfficeCopilotTools([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, body := range []string{`{"messages":[]}`, `{"tools":null}`, `{"tools":[]}`, `{"tools":[{"name":"shell"}],"tool_choice":"required"}`} {
		filtered, err := filterOfficeCopilotTools([]byte(body))
		if err != nil || string(filtered) != body {
			t.Fatalf("changed unrelated body %s: %v", body, err)
		}
	}
}

func TestOfficeCopilotForcedWebSearchFailsBeforeInference(t *testing.T) {
	for _, anthropic := range []bool{true, false} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		svc := &OpenAIGatewayService{}
		account := &Account{Platform: PlatformOfficeCopilot, Type: AccountTypeOAuth}
		choice := `{"type":"tool","name":"web_search"}`
		if !anthropic {
			choice = `{"type":"function","function":{"name":"web_search"}}`
		}
		body := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"web_search"}],"tool_choice":` + choice + `}`)
		result, err := svc.forwardOfficeCopilot(context.Background(), c, account, body, "", anthropic)
		if err == nil || result != nil || rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "web_search") {
			t.Fatalf("expected explicit HTTP 400: result=%v err=%v code=%d body=%s", result, err, rec.Code, rec.Body.String())
		}
	}
}
