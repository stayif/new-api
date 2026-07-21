package common

import (
	"net/http"
	"net/http/httptest"
	"testing"

	basecommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const honeyTestSecret = "0123456789abcdef0123456789abcdef"

func honeyTestInfo() *RelayInfo {
	return &RelayInfo{
		IsStream:           true,
		ShouldIncludeUsage: true,
		OriginModelName:    "honey-claude-sonnet-4-6",
		ChannelMeta: &ChannelMeta{
			ChannelId: 15, ChannelType: 14, ChannelBaseUrl: "https://provider.example",
			UpstreamModelName: "claude-sonnet-4-6",
		},
	}
}

func honeyTestContext(t *testing.T, info *RelayInfo) *gin.Context {
	t.Helper()
	t.Setenv(honeyAttestationSecret, honeyTestSecret)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	route := honeyRouteFingerprint(honeyTestSecret, info)
	payload := honeyAttestationVersion + "\npolicy-v1\nattempt-123\nhoney.chat.sonnet-4.6.reasoning\n" + info.OriginModelName + "\n" + route
	c.Request.Header.Set("X-NewAPI-Attestation-Version", honeyAttestationVersion)
	c.Request.Header.Set("X-Honey-Policy-Version", "policy-v1")
	c.Request.Header.Set("X-Honey-Attempt-Id", "attempt-123")
	c.Request.Header.Set("X-Honey-Logical-Model", "honey.chat.sonnet-4.6.reasoning")
	c.Request.Header.Set("X-Honey-Expected-Route", route)
	c.Request.Header.Set("X-Honey-Attestation", "v2="+honeySign(honeyTestSecret, []byte(payload)))
	return c
}

func completeHoneyState(t *testing.T, state *HoneyAttestedRelay, withUsage bool) {
	t.Helper()
	message := &dto.ClaudeMediaMessage{Model: "claude-sonnet-4-6"}
	if withUsage {
		message.Usage = &dto.ClaudeUsage{InputTokens: 17}
	}
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_start", Message: message}))
	reasoning, text := "visible reasoning", "ordinary answer"
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &reasoning}}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text}}))
	if withUsage {
		require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_delta", Usage: &dto.ClaudeUsage{OutputTokens: 23}}))
	}
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_stop"}))
}

func TestHoneyAttestedRelaySeparatesSettlementFromProviderReportedUsage(t *testing.T) {
	info := honeyTestInfo()
	state, err := StartHoneyAttestedRelay(honeyTestContext(t, info), info, []byte(`{"model":"claude-sonnet-4-6"}`))
	require.NoError(t, err)
	completeHoneyState(t, state, true)
	envelope, err := state.BuildSuccessEnvelope(info, HoneyNewAPISettlement{Amount: 341, Unit: "quota", Kind: "text_quota", Source: "newapi.final_settlement", BillingVersion: "newapi.text_quota.v1", MultiplierVersion: "newapi.runtime_price_data.v1"}, "request-123", "upstream-123")
	require.NoError(t, err)
	require.Equal(t, HoneyAttestedRelaySchema, envelope.Object)
	require.Equal(t, int64(341), envelope.Receipt.NewAPISettlement.Amount)
	require.Equal(t, "reported", envelope.Receipt.ProviderReported.Status)
	require.Equal(t, 40, envelope.Receipt.ProviderReported.TotalTokens)
	require.Equal(t, "not_comparable", envelope.Receipt.UsageComparability.Status)
	require.Equal(t, "v3="+honeySign(honeyTestSecret, mustHoneyMarshal(t, envelope.Receipt)), envelope.Signature)
}

func TestHoneyAttestedRelayAllowsUnknownProviderUsageButNotTextBeforeReasoning(t *testing.T) {
	info := honeyTestInfo()
	state, err := StartHoneyAttestedRelay(honeyTestContext(t, info), info, []byte(`{}`))
	require.NoError(t, err)
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_start", Message: &dto.ClaudeMediaMessage{Model: "claude-sonnet-4-6"}}))
	text := "must not escape"
	require.ErrorContains(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text}}), "before visible reasoning")

	info = honeyTestInfo()
	state, err = StartHoneyAttestedRelay(honeyTestContext(t, info), info, []byte(`{}`))
	require.NoError(t, err)
	completeHoneyState(t, state, false)
	envelope, err := state.BuildSuccessEnvelope(info, HoneyNewAPISettlement{Amount: 1, Unit: "quota", Kind: "text_quota", Source: "newapi.final_settlement", BillingVersion: "newapi.text_quota.v1", MultiplierVersion: "newapi.runtime_price_data.v1"}, "request-123", "upstream-123")
	require.NoError(t, err)
	require.Equal(t, "unknown", envelope.Receipt.ProviderReported.Status)
}

func TestHoneyAttestedRelayRejectsReasoningAfterTextAndRequiresMessageStop(t *testing.T) {
	info := honeyTestInfo()
	state, err := StartHoneyAttestedRelay(honeyTestContext(t, info), info, []byte(`{}`))
	require.NoError(t, err)
	reasoning, text, late := "reasoning", "answer", "late"
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_start", Message: &dto.ClaudeMediaMessage{Model: "claude-sonnet-4-6"}}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &reasoning}}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text}}))
	require.ErrorContains(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &late}}), "reasoning after text")
	_, err = state.BuildSuccessEnvelope(info, HoneyNewAPISettlement{Amount: 1, Unit: "quota", Kind: "text_quota", Source: "newapi.final_settlement", BillingVersion: "v1", MultiplierVersion: "v1"}, "request-123", "upstream-123")
	require.ErrorContains(t, err, "terminal contract")
}

func TestHoneyAttestedRelayRejectsUnknownAndPostTerminalEvents(t *testing.T) {
	info := honeyTestInfo()
	state, err := StartHoneyAttestedRelay(honeyTestContext(t, info), info, []byte(`{}`))
	require.NoError(t, err)
	require.ErrorContains(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_stop"}), "before message_start")
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "ping"}))
	pingText := "hidden content"
	require.ErrorContains(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "ping", Delta: &dto.ClaudeMediaMessage{Text: &pingText}}), "semantic fields")
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_start", Message: &dto.ClaudeMediaMessage{Model: "claude-sonnet-4-6"}}))
	require.ErrorContains(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_start", ContentBlock: &dto.ClaudeMediaMessage{Type: "tool_use"}}), "unsupported provider content block")
	require.ErrorContains(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "unexpected"}), "unsupported provider event")

	reasoning, text := "reasoning", "answer"
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &reasoning}}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text}}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_stop"}))
	require.ErrorContains(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_stop"}), "after message_stop")
	require.ErrorContains(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "ping"}), "after message_stop")
}

func TestHoneyAttestedRelayMarksComparableUsageAtTheFiftyPercentBoundary(t *testing.T) {
	info := honeyTestInfo()
	state, err := StartHoneyAttestedRelay(honeyTestContext(t, info), info, []byte(`{}`))
	require.NoError(t, err)
	completeHoneyState(t, state, true)
	within, err := state.BuildSuccessEnvelope(info, HoneyNewAPISettlement{Amount: 60, Unit: "tokens", Kind: "unconverted_tokens", Source: "newapi.final_settlement", BillingVersion: "v1", MultiplierVersion: "v1"}, "request-123", "upstream-123")
	require.NoError(t, err)
	require.Equal(t, "within_tolerance", within.Receipt.UsageComparability.Status)
	require.Equal(t, 0.5, *within.Receipt.UsageComparability.Ratio)

	outside, err := state.BuildSuccessEnvelope(info, HoneyNewAPISettlement{Amount: 61, Unit: "tokens", Kind: "unconverted_tokens", Source: "newapi.final_settlement", BillingVersion: "v1", MultiplierVersion: "v1"}, "request-123", "upstream-123")
	require.NoError(t, err)
	require.Equal(t, "outside_tolerance", outside.Receipt.UsageComparability.Status)
	require.Greater(t, *outside.Receipt.UsageComparability.Ratio, 0.5)
}

func TestHoneyAttestedRelayCountsLeadingReasoningWhitespaceWithoutTreatingItAsVisible(t *testing.T) {
	info := honeyTestInfo()
	state, err := StartHoneyAttestedRelay(honeyTestContext(t, info), info, []byte(`{}`))
	require.NoError(t, err)
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_start", Message: &dto.ClaudeMediaMessage{Model: "claude-sonnet-4-6"}}))
	whitespace, visible, text := "  ", "reasoning", "answer"
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &whitespace}}))
	require.ErrorContains(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text}}), "before visible reasoning")
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &visible}}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text}}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_stop"}))
	envelope, err := state.BuildSuccessEnvelope(info, HoneyNewAPISettlement{Amount: 1, Unit: "quota", Kind: "text_quota", Source: "newapi.final_settlement", BillingVersion: "v1", MultiplierVersion: "v1"}, "request-123", "upstream-123")
	require.NoError(t, err)
	require.Equal(t, len([]rune(whitespace+visible)), envelope.Receipt.Reasoning.Chars)
}

func mustHoneyMarshal(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := basecommon.Marshal(value)
	require.NoError(t, err)
	return payload
}
