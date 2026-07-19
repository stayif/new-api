package claude

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCountProviderTokensUsesCompiledAnthropicRequest(t *testing.T) {
	service.InitHttpClient()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages/count_tokens", r.URL.Path)
		require.Equal(t, "provider-key", r.Header.Get("x-api-key"))
		var body map[string]any
		bodyBytes, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, common.Unmarshal(bodyBytes, &body))
		require.Contains(t, body, "thinking")
		require.NotContains(t, body, "stream")
		require.NotContains(t, body, "max_tokens")
		w.Header().Set("request-id", "count_req_123")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":37}`))
	}))
	defer provider.Close()

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	maxTokens := uint(1536)
	stream := true
	compiled, err := common.Marshal(dto.ClaudeRequest{
		Model:     "claude-sonnet-4-6",
		Messages:  []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
		MaxTokens: &maxTokens,
		Stream:    &stream,
		Thinking: &dto.Thinking{
			Type:         "enabled",
			BudgetTokens: common.GetPointer(1280),
		},
	})
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:            "provider-key",
			ChannelBaseUrl:    provider.URL,
			UpstreamModelName: "claude-sonnet-4-6",
		},
	}

	finalBody, receipt, err := (&Adaptor{}).CompileAndCountProviderTokens(c, info, compiled)
	require.NoError(t, err)
	var finalRequest dto.ClaudeRequest
	require.NoError(t, common.Unmarshal(finalBody, &finalRequest))
	require.Equal(t, "summarized", finalRequest.Thinking.Display)
	require.Equal(t, 37, receipt.InputTokens)
	require.Equal(t, "count_req_123", receipt.RequestID)
	require.Equal(t, "anthropic.messages.count_tokens", receipt.Source)
	require.Contains(t, receipt.RequestBodySHA256, "sha256:")
}

func TestCountProviderTokensRejectsAdaptiveThinking(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	maxTokens := uint(1536)
	stream := true
	compiled, err := common.Marshal(dto.ClaudeRequest{
		Model:     "claude-sonnet-4-6",
		Messages:  []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
		MaxTokens: &maxTokens,
		Stream:    &stream,
		Thinking:  &dto.Thinking{Type: "adaptive", Display: "summarized"},
	})
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-sonnet-4-6"}}
	_, _, err = (&Adaptor{}).CompileAndCountProviderTokens(c, info, compiled)
	require.ErrorContains(t, err, "explicitly enabled thinking")
}
