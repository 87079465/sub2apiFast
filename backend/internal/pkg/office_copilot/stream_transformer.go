package office_copilot

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// StreamProcessor 处理上游返回的 SSE 数据流并转为 OpenAI 格式。
type StreamProcessor struct {
	ID      string
	Model   string
	Created int64
}

// NewStreamProcessor 创建新的流式处理器。
func NewStreamProcessor(model string) *StreamProcessor {
	if model == "" {
		model = "copilot-auto"
	}
	return &StreamProcessor{
		ID:      "chatcmpl-" + GenerateUUID(),
		Model:   model,
		Created: time.Now().Unix(),
	}
}

// FormatChunk 生成标准的 OpenAI SSE 格式行。
func (sp *StreamProcessor) FormatChunk(deltaText string, finishReason *string) string {
	chunk := StreamChunk{
		ID:      sp.ID,
		Object:  "chat.completion.chunk",
		Created: sp.Created,
		Model:   sp.Model,
		Choices: []DeltaChoice{
			{
				Index:        0,
				FinishReason: finishReason,
			},
		},
	}
	if deltaText != "" {
		chunk.Choices[0].Delta.Content = deltaText
	}

	bytesData, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(bytesData))
}

// ProcessStream 读取上游响应并写入客户端目标 writer。
func (sp *StreamProcessor) ProcessStream(src io.Reader, dst io.Writer) error {
	err := ReadTextStream(src, func(text string) error {
		_, err := io.WriteString(dst, sp.FormatChunk(text, nil))
		if f, ok := dst.(interface{ Flush() }); ok {
			f.Flush()
		}
		return err
	})
	if err != nil {
		return err
	}
	stopReason := "stop"
	if _, err := io.WriteString(dst, sp.FormatChunk("", &stopReason)); err != nil {
		return err
	}
	_, err = io.WriteString(dst, "data: [DONE]\n\n")
	return err
}

// ReadTextStream requires a completed, nonempty inference response. In particular,
// an HTML error page, an upstream error event, or EOF must never become success.
func ReadTextStream(src io.Reader, emit func(string) error) error {
	return ReadEventStream(src, func(event StreamEvent) error {
		if event.Kind == EventText {
			return emit(event.Text)
		}
		return nil
	})
}

// ReadEventStream keeps summaries separate from text; summaries alone do not
// make an otherwise empty or interrupted inference successful.
func ReadEventStream(src io.Reader, emit func(StreamEvent) error) error {
	scanner := bufio.NewScanner(src)
	// 适当增加 buffer 大小以应对可能的大帧
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	hasText := false
	completed := false
	contentBytes := 0
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// 处理 SSE 格式：data: ...
		if strings.HasPrefix(trimmed, "data:") {
			dataContent := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if dataContent == "[DONE]" {
				completed = true
				break
			}

			// 尝试解析文本或结构化数据
			var rawMap map[string]any
			if err := json.Unmarshal([]byte(dataContent), &rawMap); err == nil {
				if rawMap["error"] != nil || rawMap["type"] == "error" {
					return fmt.Errorf("Office Copilot returned an upstream error event")
				}
				// 提取潜在的内容字段
				var textPiece string
				if content, ok := rawMap["content"].(string); ok {
					textPiece = content
				} else if text, ok := rawMap["text"].(string); ok {
					textPiece = text
				} else if delta, ok := rawMap["delta"].(string); ok {
					textPiece = delta
				}

				if textPiece != "" {
					kind, _ := rawMap["kind"].(string)
					if kind == "" {
						kind = string(EventText)
					}
					if kind != string(EventText) && kind != string(EventReasoning) {
						continue
					}
					contentBytes += len(textPiece)
					if contentBytes > 8*1024*1024 {
						return fmt.Errorf("Office Copilot response exceeds size limit")
					}
					if kind == string(EventText) {
						hasText = true
					}
					if wErr := emit(StreamEvent{Kind: EventKind(kind), Text: textPiece}); wErr != nil {
						return wErr
					}
				}
			} else {
				return fmt.Errorf("Office Copilot returned an invalid stream event")
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	if !completed {
		return fmt.Errorf("Office Copilot stream ended without completion")
	}
	if !hasText {
		return fmt.Errorf("Office Copilot returned no generated text")
	}
	return nil
}
