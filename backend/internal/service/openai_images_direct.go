package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type openAIImagesForceResponsesContextKey struct{}

func withOpenAIImagesForceResponses(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAIImagesForceResponsesContextKey{}, true)
}

func isOpenAIImagesForceResponses(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	forced, _ := ctx.Value(openAIImagesForceResponsesContextKey{}).(bool)
	return forced
}

// openAIImagesDirectRequestContextKey 标记当前出站请求为直连 Images 端点请求。
type openAIImagesDirectRequestContextKey struct{}

// withOpenAIImagesDirectRequest 标记请求体为直连 Images 载荷，避免被 Responses 专用规范化改写。
// 参数 ctx 为上游请求上下文；返回携带标记的新上下文。
func withOpenAIImagesDirectRequest(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIImagesDirectRequestContextKey{}, true)
}

// isOpenAIImagesDirectRequest 判断上下文是否为直连 Images 请求。
// 参数 ctx 为上游请求上下文；返回 true 表示请求体是 Images API 载荷而非 Responses 载荷。
func isOpenAIImagesDirectRequest(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	direct, _ := ctx.Value(openAIImagesDirectRequestContextKey{}).(bool)
	return direct
}

// usesMirasimDirectImages 判断 Mirasim 账号是否对该图片模型走中继直连 Images 端点。
// 线上实测中继直连端点仅支持 gpt-image-2（官方客户端也只使用该模型），
// gpt-image-2.5-flare / sunburst 直连返回 404，直接走 Responses 工具可省去一次必然失败的往返。
// 参数 model 为映射后的上游图片模型；返回 true 表示优先直连。
func usesMirasimDirectImages(model string) bool {
	return strings.TrimSpace(model) == "gpt-image-2"
}

// buildMirasimImagesDirectURL 拼接 Mirasim 中继的直连图片端点（与官方客户端一致，不经过对话主控模型）。
// 参数 baseURL 为已校验的中继基础地址，edits 表示是否为图片编辑；
// 返回 {base}/v1/images/generations 或 {base}/v1/images/edits，base 已带版本段时不重复追加 /v1。
func buildMirasimImagesDirectURL(baseURL string, edits bool) string {
	if edits {
		return buildOpenAIEndpointURL(baseURL, "/v1/images/edits")
	}
	return buildOpenAIEndpointURL(baseURL, "/v1/images/generations")
}

// 显式列出已接入的模型，不把未来模型或未知快照自动送到直调端点。
func usesCodexDirectImages(model string) bool {
	switch strings.TrimSpace(model) {
	case "gpt-image-1.5", "gpt-image-2",
		"gpt-image-2.5-flare", "gpt-image-2.5-sunburst",
		"gpt-image-2.5-flare-2026-09-08", "gpt-image-2.5-sunburst-2026-09-08":
		return true
	default:
		return false
	}
}

// 正式转发与后台测试共用同一份端点选择和请求构造。
func buildOpenAIImagesOAuthPayload(parsed *OpenAIImagesRequest, model string) ([]byte, string, error) {
	if parsed == nil {
		return nil, "", fmt.Errorf("parsed images request is required")
	}
	if !usesCodexDirectImages(model) {
		body, err := buildOpenAIImagesResponsesRequest(parsed, model)
		return body, chatgptCodexURL, err
	}
	if strings.TrimSpace(parsed.Prompt) == "" {
		return nil, "", fmt.Errorf("prompt is required")
	}
	// 只把已解析并校验的图片字段发送给上游，避免客户端注入任意 JSON 字段。
	payload := make(map[string]any, 16)
	payload["model"] = model
	prompt := parsed.Prompt
	if !parsed.Multipart && gjson.ValidBytes(parsed.Body) {
		if rawPrompt := gjson.GetBytes(parsed.Body, "prompt").String(); rawPrompt != "" {
			prompt = rawPrompt
		}
	}
	payload["prompt"] = prompt
	for _, field := range []struct {
		key   string
		value string
	}{
		{"size", parsed.Size}, {"quality", parsed.Quality},
		{"background", parsed.Background}, {"output_format", parsed.OutputFormat},
		{"moderation", parsed.Moderation}, {"input_fidelity", parsed.InputFidelity},
		{"style", parsed.Style},
	} {
		if value := strings.TrimSpace(field.value); value != "" {
			payload[field.key] = value
		}
	}
	if parsed.N > 1 {
		payload["n"] = parsed.N
	}
	if parsed.OutputCompression != nil {
		payload["output_compression"] = *parsed.OutputCompression
	}
	if parsed.PartialImages != nil {
		payload["partial_images"] = *parsed.PartialImages
	}
	if parsed.Stream {
		payload["stream"] = true
	}

	endpoint := "/images/generations"
	if parsed.IsEdits() {
		endpoint = "/images/edits"
		images := make([]map[string]string, 0, len(parsed.InputImageURLs)+len(parsed.Uploads))
		for _, imageURL := range parsed.InputImageURLs {
			if imageURL = strings.TrimSpace(imageURL); imageURL != "" {
				images = append(images, map[string]string{"image_url": imageURL})
			}
		}
		for _, upload := range parsed.Uploads {
			url, err := openAIImageUploadToDataURL(upload)
			if err != nil {
				return nil, "", err
			}
			images = append(images, map[string]string{"image_url": url})
		}
		if len(images) == 0 {
			return nil, "", fmt.Errorf("image input is required")
		}
		payload["images"] = images
		mask := strings.TrimSpace(parsed.MaskImageURL)
		if parsed.MaskUpload != nil {
			var err error
			mask, err = openAIImageUploadToDataURL(*parsed.MaskUpload)
			if err != nil {
				return nil, "", err
			}
		}
		if mask != "" {
			payload["mask"] = map[string]string{"image_url": mask}
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal Codex Images request: %w", err)
	}
	return body, strings.TrimSuffix(chatgptCodexURL, "/responses") + endpoint, nil
}

// 原生 JSON 响应与后台图片预览使用同一份校验，防止空 data 被计为成功。
func parseCodexDirectImagesResponse(body []byte) ([]openAIResponsesImageResult, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("invalid Images API JSON response")
	}
	if upstreamErr := openAIImagesUpstreamErrorFromSSEPayload(body); upstreamErr != nil {
		return nil, upstreamErr
	}
	root := gjson.ParseBytes(body)
	if upstreamErr := openAIImagesUpstreamErrorFromGJSON(root.Get("error"), ""); upstreamErr != nil {
		return nil, upstreamErr
	}
	var results []openAIResponsesImageResult
	for _, item := range root.Get("data").Array() {
		result := strings.TrimSpace(item.Get("b64_json").String())
		if result == "" {
			continue
		}
		meta := func(key string) string {
			if value := item.Get(key).String(); value != "" {
				return value
			}
			return root.Get(key).String()
		}
		results = append(results, openAIResponsesImageResult{
			Result: result, RevisedPrompt: item.Get("revised_prompt").String(),
			OutputFormat: meta("output_format"), Size: meta("size"),
			Quality: meta("quality"), Background: meta("background"), Model: meta("model"),
		})
	}
	if len(results) == 0 {
		return nil, &OpenAIImagesUpstreamError{StatusCode: 502, ErrorType: "upstream_error", Message: "Images API returned no image output"}
	}
	reconcileOpenAIResponsesImageResultSizes(results, nil)
	return results, nil
}

func codexDirectImageURL(body []byte, path, outputFormat string) []byte {
	if result := gjson.GetBytes(body, path+"b64_json").String(); result != "" {
		body, _ = sjson.SetBytes(body, path+"url", "data:"+openAIImageOutputMIMEType(outputFormat)+";base64,"+result)
		body, _ = sjson.DeleteBytes(body, path+"b64_json")
	}
	return body
}

func isOpenAIImagesMainModelError(status int, body []byte) bool {
	if !isOpenAICodexPlanGatedModelError(status, body) {
		return false
	}
	message := extractUpstreamErrorMessage(body)
	model := openAIImagesResponsesMainModelValue()
	return strings.Contains(message, "'"+model+"'") ||
		strings.Contains(message, `"`+model+`"`)
}

// Images 端点只输出图片；未提供输出分类时，output_tokens 全部是图片 token。
// 缓存图片数量只采信明确明细，不根据总缓存量猜测图文占比。
func codexDirectImagesUsage(body []byte) (OpenAIUsage, bool) {
	value := gjson.GetBytes(body, "usage")
	usage, ok := openAIUsageFromGJSON(value)
	if !ok {
		return usage, false
	}
	if !value.Get("output_tokens_details.image_tokens").Exists() {
		usage.ImageOutputTokens = usage.OutputTokens
	}
	cached := value.Get("input_tokens_details.cached_tokens_details")
	if !value.Get("input_tokens_details.cached_tokens").Exists() && cached.IsObject() {
		imageTokens, _ := boundedJSONNonNegativeInt(cached.Get("image_tokens"))
		textTokens, _ := boundedJSONNonNegativeInt(cached.Get("text_tokens"))
		usage.CacheReadInputTokens = min(imageTokens, max(usage.InputTokens, 0))
		usage.CacheReadInputTokens += min(textTokens, max(usage.InputTokens-usage.CacheReadInputTokens, 0))
	}
	imageCached, _ := boundedJSONNonNegativeInt(cached.Get("image_tokens"))
	usage.ImageCacheReadTokens = min(imageCached, max(usage.ImageInputTokens, 0), max(usage.CacheReadInputTokens, 0))
	return usage, true
}

func (s *OpenAIGatewayService) handleCodexDirectImagesNonStreamingResponse(resp *http.Response, c *gin.Context, parsed *OpenAIImagesRequest) (OpenAIUsage, int, []string, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		if shouldClassifyOpenAIUpstreamStreamReadError(err) {
			err = newOpenAIUpstreamStreamReadError(err)
		}
		return OpenAIUsage{}, 0, nil, err
	}
	results, err := parseCodexDirectImagesResponse(body)
	if err != nil {
		return OpenAIUsage{}, 0, nil, err
	}
	usage, _ := codexDirectImagesUsage(body)
	if observer := upstreamResponseModelObserverFromContext(c); observer != nil {
		observer.Observe(gjson.GetBytes(body, "model").String(), true)
		for _, result := range results {
			observer.Observe(result.Model, true)
		}
	}
	clientModel := strings.TrimSpace(parsed.Model)
	if clientModel != "" {
		for i := range gjson.GetBytes(body, "data").Array() {
			body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.model", i), clientModel)
		}
	}
	for i, item := range gjson.GetBytes(body, "data").Array() {
		if actualSize := detectOpenAIImageResultSize(item.Get("b64_json").String()); actualSize != "" {
			body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.size", i), actualSize)
			if i == 0 {
				body, _ = sjson.SetBytes(body, "size", actualSize)
			}
		}
	}
	if parsed.ResponseFormat == "url" {
		for i, item := range gjson.GetBytes(body, "data").Array() {
			format := item.Get("output_format").String()
			if format == "" {
				format = gjson.GetBytes(body, "output_format").String()
			}
			if format == "" {
				format = parsed.OutputFormat
			}
			body = codexDirectImageURL(body, fmt.Sprintf("data.%d.", i), format)
		}
	}
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := "application/json"
	if s.cfg != nil && !s.cfg.Security.ResponseHeaders.Enabled {
		if upstreamType := strings.TrimSpace(resp.Header.Get("Content-Type")); upstreamType != "" {
			contentType = upstreamType
		}
	}
	c.Data(resp.StatusCode, contentType, body)
	return usage, len(results), openAIResponsesImageResultSizes(results), nil
}

// openAIImagesDiagBase64Pattern 匹配诊断片段中的长 base64 串（图片数据），记录日志前替换掉。
var openAIImagesDiagBase64Pattern = regexp.MustCompile(`[A-Za-z0-9+/=]{120,}`)

// summarizeOpenAIImagesDiagSample 生成用于日志的上游响应脱敏片段。
// 参数 body 为原始响应片段；返回替换图片 base64 后最多 600 字符的文本。
func summarizeOpenAIImagesDiagSample(body []byte) string {
	sample := strings.TrimSpace(string(body))
	if sample == "" {
		return ""
	}
	sample = openAIImagesDiagBase64Pattern.ReplaceAllString(sample, "<base64>")
	sample = sanitizeUpstreamErrorMessage(sample)
	const maxSample = 600
	if len(sample) > maxSample {
		sample = sample[:maxSample] + "...(truncated)"
	}
	return sample
}

// emitCodexDirectImagesJSONAsStream 在直连上游返回完整 Images JSON（未按 stream 返回 SSE）时，
// 把每张图片转换为 *.completed 流式事件下发给客户端，并同步用量、计数与错误状态。
// 参数：
//   - c / flusher: 下游请求上下文与刷新器
//   - parsed: 已解析的图片请求（决定事件前缀、对外模型名与 url 格式）
//   - body: 上游完整 JSON 响应体
//   - usage / counter: 用量与图片计数累加目标
//   - streamErr: 上游错误输出位置
//   - seenData / clientDisconnected / lastWriteAt: 流式处理状态，转换成功后同步更新
func (s *OpenAIGatewayService) emitCodexDirectImagesJSONAsStream(
	c *gin.Context,
	flusher http.Flusher,
	parsed *OpenAIImagesRequest,
	body []byte,
	usage *OpenAIUsage,
	counter *openAIImageOutputCounter,
	streamErr *error,
	seenData *bool,
	clientDisconnected *bool,
	lastWriteAt *time.Time,
) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return
	}
	*seenData = true
	// 步骤 1：解析 JSON；上游错误（如 moderation_blocked）不可重试时下发 error 事件，可重试时留给 failover。
	results, err := parseCodexDirectImagesResponse(body)
	if err != nil {
		var upstreamErr *OpenAIImagesUpstreamError
		if errors.As(err, &upstreamErr) {
			*streamErr = upstreamErr
			if !IsOpenAIImagesRetryableUpstreamError(upstreamErr) && !*clientDisconnected {
				_ = s.writeOpenAIImagesStreamEvent(c, flusher, "error", buildOpenAIImagesStreamErrorBodyFromUpstream(upstreamErr))
			}
			return
		}
		*streamErr = newOpenAIUpstreamStreamReadError(err)
		return
	}
	// 步骤 2：同步用量与上游实际模型观测。
	if directUsage, ok := codexDirectImagesUsage(body); ok {
		mergeOpenAIUsageNonZero(usage, directUsage)
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer != nil {
		observer.Observe(gjson.GetBytes(body, "model").String(), true)
	}
	usageRaw := []byte(gjson.GetBytes(body, "usage").Raw)
	createdAt := gjson.GetBytes(body, "created").Int()
	eventType := openAIImagesStreamPrefix(parsed) + ".completed"
	clientModel := strings.TrimSpace(parsed.Model)
	// 步骤 3：逐张图片构造完成事件，尺寸以实际图片为准，对外模型名与客户端请求一致。
	for _, img := range results {
		if observer != nil {
			observer.Observe(img.Model, true)
		}
		if size := detectOpenAIImageResultSize(img.Result); size != "" {
			img.Size = size
		}
		if clientModel != "" {
			img.Model = clientModel
		}
		payload := buildOpenAIImagesStreamCompletedPayload(eventType, img, parsed.ResponseFormat, createdAt, usageRaw)
		counter.AddSSEData(payload)
		if *clientDisconnected {
			continue
		}
		if writeErr := s.writeOpenAIImagesStreamEvent(c, flusher, eventType, payload); writeErr != nil {
			*clientDisconnected = true
			continue
		}
		*lastWriteAt = time.Now()
	}
}

// handleMirasimDirectImagesStreamAsJSON 处理客户端非流式、但向 Mirasim 中继以流式直连的图片响应。
// Mirasim 中继对非流式请求约 120 秒即返回 504，生图较慢时必然失败；上游改用 SSE 保持连接，
// 这里读取完整事件流后拼成标准 Images JSON，再复用非流式处理完成模型改写、尺寸识别与 url 格式转换。
// 参数 resp 为上游响应、c 为请求上下文、parsed 为已解析的图片请求；返回用量、图片数、输出尺寸与错误。
func (s *OpenAIGatewayService) handleMirasimDirectImagesStreamAsJSON(resp *http.Response, c *gin.Context, parsed *OpenAIImagesRequest) (OpenAIUsage, int, []string, error) {
	// 步骤 1：读取完整上游响应，读取上限与非流式 JSON 响应一致。
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		if shouldClassifyOpenAIUpstreamStreamReadError(err) {
			err = newOpenAIUpstreamStreamReadError(err)
		}
		return OpenAIUsage{}, 0, nil, err
	}
	// 步骤 2：把 SSE 完成事件聚合为 Images API JSON；上游错误事件原样返回给错误处理链。
	jsonBody, err := buildCodexDirectImagesJSONFromSSE(body)
	if err != nil {
		var upstreamErr *OpenAIImagesUpstreamError
		if !errors.As(err, &upstreamErr) {
			// 既无图片也无错误帧：记录上游实际返回形态，便于定位未识别的响应格式。
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Mirasim direct images returned no image content_type=%q bytes=%d sample=%s",
				resp.Header.Get("Content-Type"), len(body), summarizeOpenAIImagesDiagSample(body))
		}
		return OpenAIUsage{}, 0, nil, err
	}
	// 步骤 3：以 JSON 响应的形式交给既有非流式处理，响应类型改为 application/json 避免透传 SSE 类型。
	jsonResp := *resp
	jsonResp.Header = resp.Header.Clone()
	if jsonResp.Header == nil {
		jsonResp.Header = http.Header{}
	}
	jsonResp.Header.Set("Content-Type", "application/json")
	jsonResp.Header.Del("Content-Length")
	jsonResp.Body = io.NopCloser(bytes.NewReader(jsonBody))
	return s.handleCodexDirectImagesNonStreamingResponse(&jsonResp, c, parsed)
}

// buildCodexDirectImagesJSONFromSSE 将直连 Images 端点的 SSE 事件流聚合为非流式 Images API JSON。
// 参数 body 为完整上游响应体；上游已返回 JSON 时原样返回。
// 返回聚合后的 JSON：每个 *.completed 事件对应 data 中一张图片，usage 取最后一个非空用量；
// 事件流含错误帧时返回对应上游错误，既无图片也无错误时返回可重试的流截断错误。
func buildCodexDirectImagesJSONFromSSE(body []byte) ([]byte, error) {
	if gjson.ValidBytes(body) {
		return body, nil
	}
	var (
		items       []map[string]any
		usageRaw    string
		created     int64
		responseMod string
		upstreamErr *OpenAIImagesUpstreamError
	)
	forEachOpenAISSEDataPayload(string(body), func(payload []byte) {
		if !gjson.ValidBytes(payload) {
			return
		}
		// 记录第一个上游错误帧（如 moderation_blocked），没有图片时据此返回错误。
		if upstreamErr == nil {
			upstreamErr = openAIImagesUpstreamErrorFromSSEPayload(payload)
		}
		root := gjson.ParseBytes(payload)
		if usage := root.Get("usage"); usage.IsObject() {
			usageRaw = usage.Raw
		}
		// 只收集最终图片；partial_image 等中间事件不计入结果。
		if !strings.HasSuffix(root.Get("type").String(), ".completed") {
			return
		}
		b64 := strings.TrimSpace(root.Get("b64_json").String())
		if b64 == "" {
			return
		}
		item := map[string]any{"b64_json": b64}
		for _, key := range []string{"revised_prompt", "output_format", "size", "quality", "background", "model"} {
			if value := strings.TrimSpace(root.Get(key).String()); value != "" {
				item[key] = value
			}
		}
		if model := strings.TrimSpace(root.Get("model").String()); model != "" {
			responseMod = model
		}
		if createdAt := root.Get("created_at").Int(); createdAt > 0 && created == 0 {
			created = createdAt
		}
		items = append(items, item)
	})
	if len(items) == 0 {
		if upstreamErr != nil {
			return nil, upstreamErr
		}
		return nil, newOpenAIUpstreamStreamReadError(ErrOpenAIUpstreamStreamTruncated)
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	out := map[string]any{"created": created, "data": items}
	if responseMod != "" {
		out["model"] = responseMod
	}
	if usageRaw != "" {
		out["usage"] = json.RawMessage(usageRaw)
	}
	jsonBody, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal aggregated Images response: %w", err)
	}
	return jsonBody, nil
}
