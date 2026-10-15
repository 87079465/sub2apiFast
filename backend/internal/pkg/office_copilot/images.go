package office_copilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	_ "golang.org/x/image/webp"
)

const (
	MaxImageBytes      = 5 * 1024 * 1024
	MaxImageTotalBytes = 20 * 1024 * 1024
	MaxImages          = 10
)

// ImageInput is kept outside the text transcript. FileName is a stable reference
// shared by the original message position and the uploaded attachment.
type ImageInput struct {
	FileName  string
	MediaType string
	Data      []byte
}

// ExtractImages validates and replaces image blocks, including tool results and
// earlier turns, without changing their role, order, or tool-call association.
func ExtractImages(request InboundRequest) (InboundRequest, error) {
	if len(request.Images) != 0 {
		return request, nil
	}
	request.Messages = append([]InboundMessage(nil), request.Messages...)
	seen := make(map[[32]byte]int)
	total := 0
	var walk func(json.RawMessage, int) (json.RawMessage, error)
	walk = func(raw json.RawMessage, depth int) (json.RawMessage, error) {
		if depth > 32 {
			return nil, fmt.Errorf("Office Copilot content nesting exceeds limit")
		}
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			return raw, nil
		}
		var blocks []json.RawMessage
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, fmt.Errorf("invalid Office Copilot content blocks")
		}
		for i, b := range blocks {
			var block map[string]json.RawMessage
			if err := json.Unmarshal(b, &block); err != nil {
				return nil, fmt.Errorf("invalid Office Copilot content block")
			}
			var kind string
			_ = json.Unmarshal(block["type"], &kind)
			switch kind {
			case "tool_result":
				if len(block["content"]) == 0 {
					continue
				}
				content, err := walk(block["content"], depth+1)
				if err != nil {
					return nil, err
				}
				block["content"] = content
				blocks[i], _ = json.Marshal(block)
			case "image", "image_url":
				img, err := parseImage(block, kind)
				if err != nil {
					return nil, err
				}
				hash := sha256.Sum256(img.Data)
				idx, exists := seen[hash]
				if !exists {
					total += len(img.Data)
					if len(request.Images) >= MaxImages || total > MaxImageTotalBytes {
						return nil, fmt.Errorf("Office Copilot supports at most %d unique images and 20 MiB of image data per request", MaxImages)
					}
					idx = len(request.Images)
					img.FileName = fmt.Sprintf("image-%d.%s", idx+1, strings.TrimPrefix(img.MediaType, "image/"))
					seen[hash] = idx
					request.Images = append(request.Images, img)
				}
				blocks[i], _ = json.Marshal(map[string]string{"type": "text", "text": "[Attached image: " + request.Images[idx].FileName + "]"})
			}
		}
		return json.Marshal(blocks)
	}
	for i, message := range request.Messages {
		raw, err := json.Marshal(message.Content)
		if err != nil {
			return InboundRequest{}, fmt.Errorf("invalid Office Copilot content")
		}
		content, err := walk(raw, 0)
		if err != nil {
			return InboundRequest{}, err
		}
		// Keep plain text as a string for the existing simple-text fast path.
		if _, ok := message.Content.(string); !ok && message.Content != nil {
			request.Messages[i].Content = content
		}
	}
	return request, nil
}

func parseImage(block map[string]json.RawMessage, kind string) (ImageInput, error) {
	var mime, encoded string
	if kind == "image" {
		var source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		}
		if json.Unmarshal(block["source"], &source) != nil || source.Type != "base64" {
			return ImageInput{}, fmt.Errorf("Office Copilot image source must use base64; remote image URLs are not supported")
		}
		mime, encoded = source.MediaType, source.Data
	} else {
		var value string
		if json.Unmarshal(block["image_url"], &value) != nil {
			var source struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(block["image_url"], &source)
			value = source.URL
		}
		header, data, ok := strings.Cut(value, ",")
		if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
			return ImageInput{}, fmt.Errorf("Office Copilot image_url must be a base64 data URL; remote image URLs are not supported")
		}
		mime, encoded = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64"), data
	}
	if mime != "image/png" && mime != "image/jpeg" && mime != "image/gif" && mime != "image/webp" {
		return ImageInput{}, fmt.Errorf("Office Copilot image must be PNG, JPEG, GIF or WebP")
	}
	if len(encoded) == 0 || len(encoded) > base64.StdEncoding.EncodedLen(MaxImageBytes) {
		return ImageInput{}, fmt.Errorf("Office Copilot image is empty or exceeds 5 MiB")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > MaxImageBytes {
		return ImageInput{}, fmt.Errorf("Office Copilot image contains invalid or oversized base64 data")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != mime {
		return ImageInput{}, fmt.Errorf("Office Copilot image data does not match its media type or is invalid")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > 8192 || config.Height > 8192 || int64(config.Width)*int64(config.Height) > 32*1024*1024 {
		return ImageInput{}, fmt.Errorf("Office Copilot image dimensions exceed 8192 pixels per side or 32 megapixels")
	}
	return ImageInput{MediaType: mime, Data: data}, nil
}

// AttachImages follows Word's UploadFile + ImageFile annotation protocol.
// Upload failure aborts the request; an image is never silently dropped.
func (c *Client) AttachImages(ctx context.Context, payload map[string]any, images []ImageInput) error {
	if len(images) == 0 {
		return nil
	}
	if payload["source"] != "word" {
		return fmt.Errorf("Office Copilot images require a Word model: claude-opus-5-5, claude-sonnet-5 or gpt-6-sol")
	}
	message, ok := payload["message"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid Office Copilot image request")
	}
	conversation := GenerateUUID()
	annotations := make([]map[string]any, 0, len(images))
	for _, img := range images {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		fields := [][2]string{{"scenario", "UploadImage"}, {"conversationId", conversation},
			{"FileBase64", "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)},
			{"optionsSets", "cwcgptvsan"}, {"optionsSets", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch"}, {"optionsSets", "gptvnorm2048"}}
		for _, field := range fields {
			if err := writer.WriteField(field[0], field[1]); err != nil {
				return fmt.Errorf("could not encode Office Copilot image")
			}
		}
		if err := writer.Close(); err != nil {
			return fmt.Errorf("could not encode Office Copilot image")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/m365Copilot/UploadFile", &body)
		if err != nil {
			return fmt.Errorf("invalid Office Copilot upload endpoint")
		}
		c.applyStandardHeaders(req)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.Header.Set("Origin", "https://fa000000125.resources.office.net")
		req.Header.Set("Referer", "https://fa000000125.resources.office.net/")
		req.Header.Set("X-Variants", "feature.EnableImageSupportInUploadFile")
		req.Header.Set("X-GptId", "WordDraftingAgent@BuiltInAgents")
		client := *c.HTTPClient
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("Office Copilot image upload connection failed")
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("Office Copilot image upload returned HTTP %d", resp.StatusCode)
		}
		var uploaded struct {
			DocID          string `json:"docId"`
			ConversationID string `json:"conversationId"`
			Result         struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if readErr != nil || len(raw) > 64*1024 || json.Unmarshal(raw, &uploaded) != nil || uploaded.DocID == "" || uploaded.Result.Value != "Success" {
			return fmt.Errorf("Office Copilot image upload did not return a successful attachment")
		}
		if uploaded.ConversationID != "" {
			conversation = uploaded.ConversationID
		}
		annotations = append(annotations, map[string]any{"id": uploaded.DocID, "messageAnnotationType": "ImageFile",
			"messageAnnotationMetadata": map[string]string{"@type": "File", "annotationType": "File", "fileType": strings.TrimPrefix(img.MediaType, "image/"), "fileName": img.FileName}})
	}
	message["messageAnnotations"] = annotations
	payload["isStartOfSession"] = false
	delete(payload, "conversationId")
	payload["optionsSets"] = []string{"cwc_flux_image", "cwcfluxgptv", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch", "gptvnorm2048"}
	c.imageConversationID = conversation
	return nil
}
