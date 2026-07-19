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

func newAttestedRelayTestContext(t *testing.T, info *RelayInfo, secret string) *gin.Context {
	t.Helper()
	t.Setenv(attestationSecretEnv, secret)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	route := attestedRouteFingerprint(secret, info)
	policy := "honey-chat-v1"
	attempt := "attempt-123"
	logical := "honey.chat.sonnet-4.6.reasoning"
	authPayload := attestationVersion + "\n" + policy + "\n" + attempt + "\n" + logical + "\n" + info.OriginModelName + "\n" + route
	c.Request.Header.Set(AttestationVersionHeader, attestationVersion)
	c.Request.Header.Set(AttestationPolicyHeader, policy)
	c.Request.Header.Set(AttestationAttemptHeader, attempt)
	c.Request.Header.Set(AttestationLogicalModelHeader, logical)
	c.Request.Header.Set(AttestationExpectedRouteHeader, route)
	c.Request.Header.Set(AttestationAuthorizationHeader, "v1="+signAttestation(secret, []byte(authPayload)))
	return c
}

func newAttestedRelayTestInfo() *RelayInfo {
	return &RelayInfo{
		IsStream:           true,
		ShouldIncludeUsage: true,
		OriginModelName:    "honey-claude-sonnet-4-6",
		ChannelMeta: &ChannelMeta{
			ChannelId:         42,
			ChannelType:       14,
			ChannelBaseUrl:    "https://provider.example",
			UpstreamModelName: "claude-sonnet-4-6",
		},
	}
}

func TestAttestedRelayBuildsSignedProviderReceipt(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	info := newAttestedRelayTestInfo()
	c := newAttestedRelayTestContext(t, info, secret)
	c.Set(basecommon.RequestIdKey, "newapi_req_1")

	state, err := StartAttestedRelay(c, info, []byte(`{"model":"claude-sonnet-4-6"}`))
	require.NoError(t, err)
	require.NoError(t, state.SetCompiledRequest([]byte(`{"model":"claude-sonnet-4-6"}`), &ProviderTokenCountReceipt{
		InputTokens:       17,
		RequestID:         "count_req_1",
		RequestBodySHA256: "sha256:count",
		Source:            "anthropic.messages.count_tokens",
	}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type: "message_start",
		Message: &dto.ClaudeMediaMessage{
			Model: "claude-sonnet-4-6",
			Usage: &dto.ClaudeUsage{InputTokens: 17},
		},
	}))
	reasoning := "provider-visible reasoning"
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type:  "content_block_delta",
		Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &reasoning},
	}))
	text := "final answer"
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type:  "content_block_delta",
		Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text},
	}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type:  "message_delta",
		Usage: &dto.ClaudeUsage{OutputTokens: 23},
	}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_stop"}))

	providerUsage := &dto.ClaudeUsage{InputTokens: 17, OutputTokens: 23}
	usage := &dto.Usage{
		PromptTokens:     17,
		CompletionTokens: 23,
		TotalTokens:      40,
		BillingUsage:     dto.NewClaudeMessagesBillingUsage(providerUsage),
	}
	usage.CompletionTokenDetails.ReasoningTokens = 19
	envelope, err := state.BuildSuccessEnvelope(info, usage, "newapi_req_1", "execute_req_1")
	require.NoError(t, err)
	require.Equal(t, AttestedRelaySchema, envelope.Object)
	require.Equal(t, "claude-sonnet-4-6", envelope.Receipt.ResponseModel)
	require.Equal(t, "execute_req_1", envelope.Receipt.ExecutionRequestID)
	require.Equal(t, 19, envelope.Receipt.Usage.ReasoningTokens)
	require.Positive(t, envelope.Receipt.Reasoning.Chars)

	payload, err := basecommon.Marshal(envelope.Receipt)
	require.NoError(t, err)
	require.Equal(t, "v1="+signAttestation(secret, payload), envelope.Signature)
}

func TestAttestedRelayRejectsTextBeforeReasoning(t *testing.T) {
	info := newAttestedRelayTestInfo()
	c := newAttestedRelayTestContext(t, info, "0123456789abcdef0123456789abcdef")
	state, err := StartAttestedRelay(c, info, []byte(`{}`))
	require.NoError(t, err)
	text := "must not escape"
	err = state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type:  "content_block_delta",
		Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text},
	})
	require.ErrorContains(t, err, "before visible reasoning")
}

func TestAttestedRelayRejectsEstimatedSettlementUsage(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	info := newAttestedRelayTestInfo()
	c := newAttestedRelayTestContext(t, info, secret)
	state, err := StartAttestedRelay(c, info, []byte(`{}`))
	require.NoError(t, err)
	require.NoError(t, state.SetCompiledRequest([]byte(`{}`), &ProviderTokenCountReceipt{
		InputTokens: 17, RequestID: "count_req", RequestBodySHA256: "sha256:count", Source: "anthropic.messages.count_tokens",
	}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type: "message_start", Message: &dto.ClaudeMediaMessage{Model: "claude-sonnet-4-6", Usage: &dto.ClaudeUsage{InputTokens: 17}},
	}))
	reasoning := "reasoning"
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &reasoning},
	}))
	answer := "answer"
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type: "content_block_delta", Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &answer},
	}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_delta", Usage: &dto.ClaudeUsage{OutputTokens: 23}}))
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "message_stop"}))

	providerUsage := &dto.ClaudeUsage{InputTokens: 17, OutputTokens: 22}
	_, err = state.BuildSuccessEnvelope(info, &dto.Usage{
		PromptTokens: 17, CompletionTokens: 22, TotalTokens: 39,
		BillingUsage: dto.NewClaudeMessagesBillingUsage(providerUsage),
	}, "newapi_req", "execute_req")
	require.ErrorContains(t, err, "does not match provider-native usage")
}

func TestAttestedRelayRejectsRouteDriftAndPassThrough(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	info := newAttestedRelayTestInfo()
	c := newAttestedRelayTestContext(t, info, secret)
	c.Request.Header.Set(AttestationExpectedRouteHeader, "rt1_drifted")
	_, err := StartAttestedRelay(c, info, []byte(`{}`))
	require.ErrorContains(t, err, "pinned route")

	info = newAttestedRelayTestInfo()
	info.ChannelSetting.PassThroughBodyEnabled = true
	c = newAttestedRelayTestContext(t, info, secret)
	_, err = StartAttestedRelay(c, info, []byte(`{}`))
	require.ErrorContains(t, err, "pass-through")
}
