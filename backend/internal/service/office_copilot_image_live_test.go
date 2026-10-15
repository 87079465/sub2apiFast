package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Opt-in integration test: the expected random code exists only in pixels,
// never in the prompt, file name, or preceding messages. No Office app needed.
func TestOfficeCopilotLiveImages(t *testing.T) {
	path := os.Getenv("OFFICE_COPILOT_LIVE_CREDENTIALS")
	if path == "" {
		t.Skip("live credentials not configured")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var credentials map[string]any
	require.NoError(t, json.Unmarshal(raw, &credentials))
	account := &Account{Platform: PlatformOfficeCopilot, Type: AccountTypeOAuth, Credentials: credentials}
	if proxyURL := os.Getenv("OFFICE_COPILOT_LIVE_PROXY_URL"); proxyURL != "" {
		u, err := url.Parse(proxyURL)
		require.NoError(t, err)
		port, err := strconv.Atoi(u.Port())
		require.NoError(t, err)
		id := int64(1)
		account.ProxyID = &id
		account.Proxy = &Proxy{Protocol: u.Scheme, Host: u.Hostname(), Port: port}
	}
	for _, mode := range []string{"anthropic", "openai", "tool_result"} {
		t.Run(mode, func(t *testing.T) {
			var random [4]byte
			_, err := rand.Read(random[:])
			require.NoError(t, err)
			expected := strings.ToUpper(hex.EncodeToString(random[:]))
			small := image.NewRGBA(image.Rect(0, 0, 100, 30))
			draw.Draw(small, small.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
			d := font.Drawer{Dst: small, Src: image.NewUniform(color.Black), Face: basicfont.Face7x13, Dot: fixed.P(8, 20)}
			d.DrawString(expected)
			large := image.NewRGBA(image.Rect(0, 0, 600, 180))
			draw.NearestNeighbor.Scale(large, large.Bounds(), small, small.Bounds(), draw.Src, nil)
			var encoded bytes.Buffer
			require.NoError(t, png.Encode(&encoded, large))
			data := base64.StdEncoding.EncodeToString(encoded.Bytes())
			img := map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": data}}
			if mode == "openai" {
				img = map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + data}}
			}
			prompt := "Read the 8-character code in the attached image. Reply only with the code. This is an API image test; no Office document is connected. Do not read or edit an Office document."
			messages := []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": prompt}, img}}}
			if mode == "tool_result" {
				messages = []any{
					map[string]any{"role": "user", "content": prompt},
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "capture_1", "name": "screenshot", "input": map[string]any{}}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "capture_1", "content": []any{img}}}},
				}
			}
			body, err := json.Marshal(map[string]any{"model": "claude-opus-5-5", "max_tokens": 128, "messages": messages})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
			_, err = (&OpenAIGatewayService{}).forwardOfficeCopilot(ctx, c, account, body, "", mode != "openai")
			require.NoError(t, err)
			require.Equal(t, 200, rec.Code)
			answerPath := "content.0.text"
			if mode == "openai" {
				answerPath = "choices.0.message.content"
			}
			answer := gjson.Get(rec.Body.String(), answerPath).String()
			require.Contains(t, strings.ToUpper(answer), expected, "the random image code must be recognized")
		})
	}
}
