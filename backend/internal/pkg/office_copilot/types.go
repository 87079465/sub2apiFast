package office_copilot

// TokenResponse 表示 Microsoft OAuth 2.0 Token 接口返回。
type TokenResponse struct {
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	ExtExpiresIn int64  `json:"ext_expires_in"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
}

// ConversationInfo 表示从 Substrate 获取的 Copilot 活跃会话信息。
type ConversationInfo struct {
	ConversationID string `json:"conversationId"`
	ChatName       string `json:"chatName"`
	CreateTimeUTC  int64  `json:"createTimeUtc"`
	UpdateTimeUTC  int64  `json:"updateTimeUtc"`
	ThreadID       string `json:"threadId"`
}

// GetChatsResponse 表示 /m365Copilot/GetChats 的响应体。
type GetChatsResponse struct {
	Chats []ConversationInfo `json:"chats"`
}

// MessageItem 表示发送或接收的单条消息内容。
type MessageItem struct {
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	QueryID   string `json:"queryId,omitempty"`
}

// ChatRequest 表示通过 Office Copilot 发起推理的通用请求结构。
type ChatRequest struct {
	ConversationID string        `json:"conversationId"`
	Scenario       string        `json:"scenario"`
	Messages       []MessageItem `json:"messages"`
	Model          string        `json:"model,omitempty"`
	Stream         bool          `json:"stream"`
}

// DeltaChoice 表示流式增量结构。
type DeltaChoice struct {
	Index int `json:"index"`
	Delta struct {
		Role    string `json:"role,omitempty"`
		Content string `json:"content,omitempty"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason,omitempty"`
}

// StreamChunk 表示标准 OpenAI 格式的流式 chunk。
type StreamChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []DeltaChoice `json:"choices"`
}

// ChatCompletionResponse 表示非流式的 OpenAI 标准补全响应。
type ChatCompletionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}
