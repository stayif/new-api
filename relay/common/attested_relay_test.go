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
	c.Request.Header.Set(AttestationAuthorizationHeader, "v2="+signAttestation(secret, []byte(authPayload)))
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
	require.NoError(t, state.SetBillingReceipt("claude-sonnet-4-6", "ratio", 0.5, 5, 1, 0))
	require.NoError(t, state.SetCompiledRequest([]byte(`{"model":"claude-sonnet-4-6"}`)))
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
	require.Equal(t, "actual", envelope.Receipt.Usage.Provenance)
	require.Equal(t, 19, envelope.Receipt.Usage.ReasoningTokens)
	require.Positive(t, envelope.Receipt.Reasoning.Chars)
	require.Equal(t, "claude-sonnet-4-6", envelope.Receipt.Billing.Model)
	require.Equal(t, "ratio", envelope.Receipt.Billing.Mode)
	require.Equal(t, 0.5, envelope.Receipt.Billing.ModelRatio)
	require.Equal(t, float64(5), envelope.Receipt.Billing.CompletionRatio)

	payload, err := basecommon.Marshal(envelope.Receipt)
	require.NoError(t, err)
	require.Equal(t, "v2="+signAttestation(secret, payload), envelope.Signature)
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

func TestAttestedRelayRejectsReasoningAfterOrdinaryText(t *testing.T) {
	info := newAttestedRelayTestInfo()
	c := newAttestedRelayTestContext(t, info, "0123456789abcdef0123456789abcdef")
	state, err := StartAttestedRelay(c, info, []byte(`{}`))
	require.NoError(t, err)
	reasoning := " first thought "
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type:  "content_block_delta",
		Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &reasoning},
	}))
	require.Equal(t, len([]rune(reasoning)), state.ReasoningChars)
	text := " answer "
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type:  "content_block_delta",
		Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text},
	}))
	require.Equal(t, len([]rune(text)), state.TextChars)
	lateReasoning := "late thought"
	err = state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type:  "content_block_delta",
		Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &lateReasoning},
	})
	require.ErrorContains(t, err, "reasoning after ordinary text")
}

func TestAttestedRelayRejectsEstimatedSettlementUsage(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	info := newAttestedRelayTestInfo()
	c := newAttestedRelayTestContext(t, info, secret)
	state, err := StartAttestedRelay(c, info, []byte(`{}`))
	require.NoError(t, err)
	require.NoError(t, state.SetBillingReceipt("claude-sonnet-4-6", "ratio", 0.5, 5, 1, 0))
	require.NoError(t, state.SetCompiledRequest([]byte(`{}`)))
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

	providerUsage := &dto.ClaudeUsage{InputTokens: 17, OutputTokens: 23}
	billingUsage := dto.NewClaudeMessagesBillingUsage(providerUsage)
	require.NotNil(t, billingUsage)
	billingUsage.Estimated = true
	_, err = state.BuildSuccessEnvelope(info, &dto.Usage{
		PromptTokens: 17, CompletionTokens: 23, TotalTokens: 40,
		BillingUsage: billingUsage,
	}, "newapi_req", "execute_req")
	require.ErrorContains(t, err, "billing usage receipt is missing")
}

func TestAttestedRelayAcceptsEmptyProviderPingOnly(t *testing.T) {
	state := &AttestedRelayState{}
	require.NoError(t, state.ObserveClaudeResponse(&dto.ClaudeResponse{Type: "ping"}))
	require.Zero(t, state.ReasoningChars)
	require.Zero(t, state.TextChars)
	require.Zero(t, state.ProviderPromptTokens)
	require.Zero(t, state.ProviderCompletionTokens)
	require.False(t, state.ProviderDone)

	_, err := state.BuildSuccessEnvelope(
		newAttestedRelayTestInfo(),
		&dto.Usage{},
		"newapi_req",
		"execute_req",
	)
	require.ErrorContains(t, err, "compiled provider request receipt is missing")

	text := "not a heartbeat"
	err = state.ObserveClaudeResponse(&dto.ClaudeResponse{
		Type:  "ping",
		Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text},
	})
	require.ErrorContains(t, err, "unexpected semantic fields")
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
