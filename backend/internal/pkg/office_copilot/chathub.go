package office_copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ChatHub uses the SignalR JSON hub protocol, not HTTP POST inference. The
// official client skips negotiation and authenticates its WebSocket URL.
func (c *Client) sendChatHub(ctx context.Context, body []byte) (*http.Response, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid ChatHub request")
	}
	var payload struct {
		Source string `json:"source"`
	}
	_ = json.Unmarshal(body, &payload)
	source, origin := "excel", "https://fa000000124.resources.office.net"
	if payload.Source == "word" {
		source, origin = "word", "https://fa000000125.resources.office.net"
	}
	endpoint, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Office Copilot endpoint")
	}
	switch endpoint.Scheme {
	case "https":
		endpoint.Scheme = "wss"
	case "http":
		endpoint.Scheme = "ws"
	default:
		return nil, fmt.Errorf("invalid Office Copilot endpoint scheme")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/m365Copilot/ChatHub"
	query := endpoint.Query()
	query.Set("access_token", c.AccessToken)
	query.Set("ClientRequestId", GenerateUUID())
	query.Set("source", source)
	query.Set("scenario", c.Scenario)
	if c.imageConversationID != "" {
		// Word resolves ImageFile annotations only on its image-enabled agent
		// path. Bind the uploaded files to this connection, not to message text.
		query.Set("ConversationId", c.imageConversationID)
		query.Set("variants", "agt_worddraftingagent_enable_image_rich_response")
	}
	endpoint.RawQuery = query.Encode()
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	if c.HTTPClient != nil {
		if transport, ok := c.HTTPClient.Transport.(*http.Transport); ok {
			dialer.Proxy = transport.Proxy
			dialer.NetDialContext = transport.DialContext
			if transport.TLSClientConfig != nil {
				// UploadFile may initialize HTTP/2 on the shared HTTP transport.
				// Gorilla's Upgrade handshake requires HTTP/1.1. Clone so we do not
				// change the HTTP client's ALPN configuration (or race with it).
				dialer.TLSClientConfig = transport.TLSClientConfig.Clone()
				dialer.TLSClientConfig.NextProtos = []string{"http/1.1"}
			}
		}
	}
	conn, response, err := dialer.DialContext(ctx, endpoint.String(), http.Header{"Origin": {origin}})
	if err != nil {
		// Dial errors can embed access_token in the URL. Never wrap them in logs.
		if response != nil {
			if response.Body != nil {
				response.Body.Close()
			}
			return nil, fmt.Errorf("Office Copilot WebSocket handshake returned HTTP %d", response.StatusCode)
		}
		return nil, fmt.Errorf("Office Copilot WebSocket connection failed")
	}
	conn.SetReadLimit(8 * 1024 * 1024)
	deadline := time.Now().Add(120 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetReadDeadline(deadline)
	conn.SetWriteDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	cleanup := func() { stop(); conn.Close() }
	if err := conn.WriteMessage(websocket.TextMessage, []byte("{\"protocol\":\"json\",\"version\":1}\x1e")); err != nil {
		cleanup()
		return nil, fmt.Errorf("Office Copilot handshake write failed")
	}
	_, handshake, err := conn.ReadMessage()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("Office Copilot handshake response failed")
	}
	records := strings.Split(string(handshake), "\x1e")
	var ack map[string]json.RawMessage
	if err := json.Unmarshal([]byte(records[0]), &ack); err != nil || ack["error"] != nil {
		cleanup()
		return nil, fmt.Errorf("Office Copilot rejected the SignalR handshake")
	}
	request, _ := json.Marshal(map[string]any{"type": 4, "invocationId": "0", "target": "chat", "arguments": []json.RawMessage{body}})
	request = append(request, 0x1e)
	if err := conn.WriteMessage(websocket.TextMessage, request); err != nil {
		cleanup()
		return nil, fmt.Errorf("Office Copilot chat invocation failed")
	}
	// Keepalives and local-plugin failure replies share the single WS writer.
	var writeMu sync.Mutex
	writeFrame := func(frame []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, frame)
	}
	pingCtx, cancelPing := context.WithCancel(ctx)
	closeConnection := cleanup
	cleanup = func() { cancelPing(); closeConnection() }
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				if err := writeFrame([]byte("{\"type\":6}\x1e")); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	reader, writer := io.Pipe()
	go func() {
		defer cleanup()
		state := chatHubStream{previous: map[string]string{}}
		defer func() {
			if c.OnQuota != nil && state.quota.HasObservation() {
				c.OnQuota(state.quota)
			}
		}()
		pluginCalls := 0
		emit := func(event StreamEvent) error {
			data, _ := json.Marshal(event)
			_, err := fmt.Fprintf(writer, "data: %s\n\n", data)
			return err
		}
		pending := strings.Join(records[1:], "\x1e")
		for {
			if pending == "" {
				_, frame, readErr := conn.ReadMessage()
				if readErr != nil {
					writer.CloseWithError(fmt.Errorf("Office Copilot connection ended before completion"))
					return
				}
				pending = string(frame)
			}
			for _, record := range strings.Split(pending, "\x1e") {
				if strings.TrimSpace(record) == "" {
					continue
				}
				if reply, handled, err := unavailableLocalPlugin([]byte(record)); handled {
					pluginCalls++
					if err != nil || pluginCalls > 16 {
						writer.CloseWithError(fmt.Errorf("Office Copilot local plugin request is invalid or exceeded the limit"))
						return
					}
					if err := writeFrame(reply); err != nil {
						writer.CloseWithError(fmt.Errorf("Office Copilot local plugin reply failed"))
						return
					}
					continue
				}
				done, err := state.processEvents([]byte(record), emit)
				if err != nil {
					writer.CloseWithError(err)
					return
				}
				if done {
					_, err = io.WriteString(writer, "data: [DONE]\n\n")
					writer.CloseWithError(err)
					return
				}
			}
			pending = ""
		}
	}()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &chatHubBody{PipeReader: reader, cleanup: cleanup}}, nil
}

// An API session has no Office canvas. Reply using the official local-plugin
// failure envelope, without executing any document or filesystem operation.
func unavailableLocalPlugin(record []byte) ([]byte, bool, error) {
	var frame struct {
		Type         int    `json:"type"`
		Target       string `json:"target"`
		InvocationID string `json:"invocationId"`
		Arguments    []struct {
			Schema        string `json:"schema_version"`
			CorrelationID string `json:"correlation_id"`
		} `json:"arguments"`
	}
	if json.Unmarshal(record, &frame) != nil || frame.Type != 1 || frame.Target != "invoke_local_plugin" {
		return nil, false, nil
	}
	if frame.InvocationID == "" || len(frame.Arguments) != 1 || frame.Arguments[0].CorrelationID == "" {
		return nil, true, fmt.Errorf("invalid local plugin invocation")
	}
	arg := frame.Arguments[0]
	// The upstream SignalR completion reader requires type before result.
	// A Go map sorts keys and puts type last, causing a type-7 connection close.
	response, err := json.Marshal(struct {
		Type         int    `json:"type"`
		InvocationID string `json:"invocationId"`
		Result       any    `json:"result"`
	}{3, frame.InvocationID, map[string]any{
		"schema_version": arg.Schema, "correlation_id": arg.CorrelationID,
		"response": map[string]string{"status": "Fail", "message": "No Office document is connected to this API session. Document tools are unavailable."},
	}})
	return append(response, 0x1e), true, err
}

type chatHubBody struct {
	*io.PipeReader
	cleanup func()
}

func (b *chatHubBody) Close() error { b.cleanup(); return b.PipeReader.Close() }

type chatHubMessage struct {
	Text                string `json:"text"`
	Author              string `json:"author"`
	MessageType         string `json:"messageType"`
	MessageID           string `json:"messageId"`
	ContentType         string `json:"contentType"`
	ContentOrigin       string `json:"contentOrigin"`
	AddToChainOfThought bool   `json:"addToChainOfThought"`
}
type chatHubUpdate struct {
	MeteringInformation json.RawMessage  `json:"meteringInformation"`
	Throttling          json.RawMessage  `json:"throttling"`
	Messages            []chatHubMessage `json:"messages"`
	StreamingMode       string           `json:"streamingMode"`
	Result              *struct {
		Value               string          `json:"value"`
		MeteringInformation json.RawMessage `json:"meteringInformation"`
	} `json:"result"`
}
type chatHubStream struct {
	quota        QuotaSnapshot
	previous     map[string]string
	succeeded    bool
	hasText      bool
	contentBytes int
}

func (s *chatHubStream) process(data []byte, emit func(string) error) (bool, error) {
	return s.processEvents(data, func(event StreamEvent) error {
		if event.Kind == EventText {
			return emit(event.Text)
		}
		return nil
	})
}

func (s *chatHubStream) processEvents(data []byte, emit func(StreamEvent) error) (bool, error) {
	var frame struct {
		Type         int             `json:"type"`
		Target       string          `json:"target"`
		InvocationID string          `json:"invocationId"`
		Error        string          `json:"error"`
		Arguments    []chatHubUpdate `json:"arguments"`
		Item         chatHubUpdate   `json:"item"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return false, fmt.Errorf("invalid Office Copilot SignalR frame")
	}
	if frame.Error != "" {
		if frame.Type == 2 && frame.InvocationID == "0" {
			s.observeUpdate(frame.Item)
		}
		if err := s.quota.RestrictionError(); err != nil {
			return false, err
		}
		return false, fmt.Errorf("Office Copilot reported a SignalR error")
	}
	updates := frame.Arguments
	switch frame.Type {
	case 1:
		if frame.Target != "update" {
			return false, nil
		}
	case 2:
		if frame.InvocationID != "0" {
			return false, nil
		}
		updates = []chatHubUpdate{frame.Item}
	case 3:
		if frame.InvocationID != "0" {
			return false, nil
		}
		if !s.succeeded {
			if err := s.quota.RestrictionError(); err != nil {
				return false, err
			}
			return false, fmt.Errorf("Office Copilot completed without a successful response")
		}
		if !s.hasText {
			if err := s.quota.RestrictionError(); err != nil {
				return false, err
			}
			return false, fmt.Errorf("Office Copilot completed successfully but returned no assistant text; retry with an explicit text-only prompt")
		}
		return true, nil
	case 6:
		return false, nil
	case 7:
		return false, fmt.Errorf("Office Copilot closed the chat session")
	default:
		return false, nil
	}
	for _, update := range updates {
		s.observeUpdate(update)
		if update.Result != nil {
			if update.Result.Value != "Success" {
				if err := s.quota.RestrictionError(); err != nil {
					return false, err
				}
				return false, fmt.Errorf("Office Copilot inference failed (%s)", update.Result.Value)
			}
			s.succeeded = true
		}
		for _, message := range update.Messages {
			kind := classifyMessage(message)
			if kind == "" || message.Text == "" {
				continue
			}
			// Progress and Office-native tools are not client answers or tools.
			// Their payloads must never enter the JSON tool bridge or usage estimate.
			if kind == EventProgress || kind == EventTool {
				continue
			}
			if s.previous == nil {
				s.previous = make(map[string]string)
			}
			key := string(kind) + ":" + message.MessageID
			old := s.previous[key]
			text := message.Text
			if update.StreamingMode != "Delta" {
				if text == old {
					continue
				}
				if !strings.HasPrefix(text, old) {
					if kind == EventReasoning {
						// Public progress summaries can replace the current stage.
						// Keep a new stage separate; do not fail a valid text answer.
						text = "\n" + text
					} else {
						return false, fmt.Errorf("Office Copilot revised an already streamed message")
					}
				} else {
					text = strings.TrimPrefix(text, old)
				}
				s.previous[key] = message.Text
			} else {
				s.previous[key] = old + text
			}
			if text != "" {
				s.contentBytes += len(text)
				if s.contentBytes > 8*1024*1024 {
					return false, fmt.Errorf("Office Copilot response exceeds size limit")
				}
				if err := emit(StreamEvent{Kind: kind, Text: text}); err != nil {
					return false, err
				}
				if kind == EventText {
					s.hasText = true
				}
			}
		}
	}
	return false, nil
}

func (s *chatHubStream) observeUpdate(update chatHubUpdate) {
	s.quota.observe(update.Throttling)
	s.quota.observeMetering(update.MeteringInformation)
	if update.Result != nil {
		s.quota.observeMetering(update.Result.MeteringInformation)
	}
}
