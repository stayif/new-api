package claude

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCompileAttestedProviderRequestFinalizesAnthropicBodyWithoutProviderSideEffect(t *testing.T) {
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
			ChannelBaseUrl:    "https://provider.example",
			UpstreamModelName: "claude-sonnet-4-6",
		},
	}

	finalBody, err := (&Adaptor{}).CompileAttestedProviderRequest(c, info, compiled)
	require.NoError(t, err)
	var finalRequest dto.ClaudeRequest
	require.NoError(t, common.Unmarshal(finalBody, &finalRequest))
	require.Equal(t, "summarized", finalRequest.Thinking.Display)
	require.Equal(t, "claude-sonnet-4-6", finalRequest.Model)
	require.True(t, *finalRequest.Stream)
}

func TestCompileAttestedProviderRequestRejectsAdaptiveThinking(t *testing.T) {
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
	_, err = (&Adaptor{}).CompileAttestedProviderRequest(c, info, compiled)
	require.ErrorContains(t, err, "explicitly enabled thinking")
}
