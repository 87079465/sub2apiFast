package service

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
	"github.com/tidwall/gjson"
)

// Opt-in only: uses a locally supplied credential file and sends synthetic
// prompts to Microsoft. Never prints credentials or executes generated tools.
func TestOfficeCopilotLiveMessages(t *testing.T) {
	path := os.Getenv("OFFICE_COPILOT_LIVE_CREDENTIALS")
	if path == "" {
		t.Skip("live credentials not configured")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var credentials map[string]any
	require.NoError(t, json.Unmarshal(raw, &credentials))
	account := &Account{Platform: PlatformOfficeCopilot, Type: AccountTypeOAuth, Credentials: credentials}
	svc := &OpenAIGatewayService{}
	cases := []struct{ name, body, want string }{
		{"opus", `{"model":"claude-opus-5-5","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"Reply only: opus route test successful. Do not edit a document."}]}`, `event: message_stop`},
		{"sonnet", `{"model":"claude-sonnet-5","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"Reply only: sonnet route test successful. Do not edit a document."}]}`, `event: message_stop`},
		{"text", `{"model":"copilot-auto","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"Reply only: connection test successful."}]}`, `event: message_stop`},
		{"tool", `{"model":"copilot-auto","max_tokens":256,"stream":true,"tools":[{"name":"Read","description":"Read a file. Return the tool call; the client executes it.","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],"messages":[{"role":"user","content":"Use the Read tool to read /tmp/example.txt. Do not pretend to execute it."}]}`, `event: message_stop`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
			result, err := svc.ForwardAsAnthropic(ctx, c, account, []byte(tc.body), "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			if tc.name == "opus" {
				require.Equal(t, "claude-opus-5-5", result.UpstreamModel)
			}
			if tc.name == "sonnet" {
				require.Equal(t, "claude-sonnet-5", result.UpstreamModel)
			}
			require.Contains(t, rec.Body.String(), tc.want)
			require.NotContains(t, rec.Body.String(), `event: error`)
		})
	}
}

// Exercises the admin account test's default prompt, not the public API's
// synthetic prompt. The upstream can complete a bare greeting without text.
func TestOfficeCopilotLiveAccountTestDefaultPrompt(t *testing.T) {
	path := os.Getenv("OFFICE_COPILOT_LIVE_CREDENTIALS")
	if path == "" {
		t.Skip("live credentials not configured")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var credentials map[string]any
	require.NoError(t, json.Unmarshal(raw, &credentials))
	account := &Account{Platform: PlatformOfficeCopilot, Type: AccountTypeOAuth, Credentials: credentials}
	svc := &AccountTestService{}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	c.Request = httptest.NewRequest("POST", "/api/v1/admin/accounts/0/test", nil).WithContext(ctx)
	require.NoError(t, svc.testOfficeCopilotAccountConnection(c, account, "claude-opus-5-5", ""))
	require.Contains(t, rec.Body.String(), `"type":"content"`)
	require.Contains(t, rec.Body.String(), `"type":"test_complete"`)
	require.Contains(t, rec.Body.String(), `"success":true`)
	require.NotContains(t, rec.Body.String(), `"type":"error"`)
}

func TestOfficeCopilotLiveToolTranscript(t *testing.T) {
	path := os.Getenv("OFFICE_COPILOT_LIVE_CREDENTIALS")
	if path == "" {
		t.Skip("live credentials not configured")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var credentials map[string]any
	require.NoError(t, json.Unmarshal(raw, &credentials))
	account := &Account{Platform: PlatformOfficeCopilot, Type: AccountTypeOAuth, Credentials: credentials}
	svc := &OpenAIGatewayService{}
	request := map[string]any{
		"model": "claude-opus-5-5", "max_tokens": 256,
		"system": "This is a synthetic API tool protocol test. The API client provides LookupStatus. Request it when asked. If its result is an error, answer exactly PROBE_TOOL_FAILED; do not invent a successful result or use Office document tools.",
		"tools":  []any{map[string]any{"name": "LookupStatus", "description": "Look up a synthetic job status. The API client executes this tool.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"job_id": map[string]any{"type": "string"}}, "required": []string{"job_id"}}}},
	}
	messages := []any{map[string]any{"role": "user", "content": "Use LookupStatus with job_id probe-job. Request the tool; do not guess the status."}}
	forward := func() gjson.Result {
		t.Helper()
		request["messages"] = messages
		body, err := json.Marshal(request)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
		result, err := svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, 200, rec.Code)
		return gjson.Parse(rec.Body.String())
	}
	first := forward()
	if first.Get("stop_reason").String() != "tool_use" {
		// Compare the same benign synthetic request with the previous adapter
		// format before attributing an upstream refusal to the new envelope.
		tools, err := json.Marshal(request["tools"])
		require.NoError(t, err)
		legacy := `[System Instructions: API compatibility instructions: Respond ONLY with a JSON object of the form {"text":"assistant response", "tool_calls":[{"name":"declared tool name","arguments":{}}]}. Use an empty tool_calls array for final answers. When tools are needed, return their name and argument object; do not claim to have executed them. Tool execution results will arrive in subsequent conversation messages. Do not wrap the JSON in markdown. Preserve the user's task and all system instructions. Available tools: ` + string(tools) + "\nTool choice: ]\n\n[System Instructions: " + request["system"].(string) + "]\n\nUse LookupStatus with job_id probe-job. Request the tool; do not guess the status."
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		resp, err := officeCopilotInference(ctx, nil, account, office_copilot.InboundRequest{Messages: []office_copilot.InboundMessage{{Role: "user", Content: legacy}}}, "claude-opus-5-5")
		require.NoError(t, err)
		defer resp.Body.Close()
		var baseline strings.Builder
		require.NoError(t, office_copilot.ReadTextStream(resp.Body, func(part string) error { baseline.WriteString(part); return nil }))
		t.Logf("Synthetic legacy-format comparison: %s", baseline.String())
	}
	require.Equal(t, "tool_use", first.Get("stop_reason").String(), "synthetic probe response: %s", first.Get("content").Raw)
	var call gjson.Result
	for _, block := range first.Get("content").Array() {
		if block.Get("type").String() == "tool_use" {
			require.False(t, call.Exists(), "expected one synthetic tool call")
			call = block
		}
	}
	require.Equal(t, "LookupStatus", call.Get("name").String())
	require.Equal(t, "probe-job", call.Get("input.job_id").String())
	require.NotEmpty(t, call.Get("id").String())
	// No tool is executed. Supply an explicitly synthetic failure and verify
	// that its correlation ID and error status survive a second API request.
	messages = append(messages,
		map[string]any{"role": "assistant", "content": json.RawMessage(first.Get("content").Raw)},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": call.Get("id").String(), "is_error": true, "content": "Synthetic lookup failed. No status was retrieved."}}},
	)
	request["tool_choice"] = map[string]any{"type": "none"}
	second := forward()
	require.Equal(t, "end_turn", second.Get("stop_reason").String())
	var reply strings.Builder
	for _, block := range second.Get("content").Array() {
		require.Equal(t, "text", block.Get("type").String())
		reply.WriteString(block.Get("text").String())
	}
	require.Equal(t, "PROBE_TOOL_FAILED", strings.TrimSpace(reply.String()))
}

func TestOfficeCopilotLiveConversationHistory(t *testing.T) {
	path := os.Getenv("OFFICE_COPILOT_LIVE_CREDENTIALS")
	if path == "" {
		t.Skip("live credentials not configured")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var credentials map[string]any
	require.NoError(t, json.Unmarshal(raw, &credentials))
	account := &Account{Platform: PlatformOfficeCopilot, Type: AccountTypeOAuth, Credentials: credentials}
	svc := &OpenAIGatewayService{}
	for _, tc := range []struct{ name, body, want string }{
		{"plain_history", `{"model":"claude-opus-5-5","max_tokens":128,"system":"Answer the latest question concisely using the conversation history.","messages":[{"role":"user","content":"The code word is CITRUS."},{"role":"assistant","content":"Understood."},{"role":"user","content":"Return the code word only."}]}`, "CITRUS"},
		{"tool_error_history", `{"model":"claude-opus-5-5","max_tokens":128,"system":"Read the supplied conversation history. Report only the observed result, not what the assistant intended to do.","messages":[{"role":"user","content":"Look up the synthetic job status."},{"role":"assistant","content":[{"type":"tool_use","id":"probe_lookup_1","name":"LookupStatus","input":{"job_id":"probe-job"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"probe_lookup_1","is_error":true,"content":"Synthetic lookup failed. No status was retrieved."},{"type":"text","text":"Did the lookup succeed or fail? Reply only FAILED or SUCCEEDED; do not call any tools."}]}]}`, "FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
			result, err := svc.ForwardAsAnthropic(ctx, c, account, []byte(tc.body), "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tc.want, strings.TrimSpace(gjson.Get(rec.Body.String(), "content.0.text").String()))
		})
	}
}
