package common

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	basecommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"github.com/gin-gonic/gin"
)

const (
	AttestedRelaySchema = "newapi.attested_relay.v1"

	AttestationVersionHeader       = "X-NewAPI-Attestation-Version"
	AttestationAuthorizationHeader = "X-Honey-Attestation"
	AttestationPolicyHeader        = "X-Honey-Policy-Version"
	AttestationAttemptHeader       = "X-Honey-Attempt-Id"
	AttestationLogicalModelHeader  = "X-Honey-Logical-Model"
	AttestationExpectedRouteHeader = "X-Honey-Expected-Route"

	attestationSecretEnv = "HONEY_ATTESTATION_SECRET"
	attestationVersion   = "1"
	attestationCompiler  = "openai_chat_completions_to_anthropic_messages"
)

var attestationIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type ProviderTokenCountReceipt struct {
	InputTokens       int
	RequestID         string
	RequestBodySHA256 string
	Source            string
}

type AttestedRelayState struct {
	PolicyVersion            string
	AttemptID                string
	LogicalModel             string
	SelectedModel            string
	UpstreamModel            string
	RouteFingerprint         string
	CompiledBodySHA256       string
	Count                    *ProviderTokenCountReceipt
	ResponseModel            string
	ReasoningChars           int
	TextChars                int
	ProviderPromptTokens     int
	ProviderCompletionTokens int
	ProviderDone             bool
}

type AttestedRelayCount struct {
	Source            string `json:"source"`
	InputTokens       int    `json:"input_tokens"`
	RequestID         string `json:"request_id"`
	RequestBodySHA256 string `json:"request_body_sha256"`
}

type AttestedRelayUsage struct {
	Source           string `json:"source"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	ReasoningTokens  int    `json:"reasoning_tokens"`
}

type AttestedRelayReasoning struct {
	Source string `json:"source"`
	Chars  int    `json:"chars"`
}

type AttestedRelayReceipt struct {
	Schema             string                 `json:"schema"`
	PolicyVersion      string                 `json:"policy_version"`
	AttemptID          string                 `json:"attempt_id"`
	LogicalModel       string                 `json:"logical_model"`
	NewAPIRequestID    string                 `json:"newapi_request_id"`
	SelectedModel      string                 `json:"selected_model"`
	UpstreamModel      string                 `json:"upstream_model"`
	ResponseModel      string                 `json:"response_model"`
	RouteFingerprint   string                 `json:"route_fingerprint"`
	Compiler           string                 `json:"compiler"`
	NewAPIVersion      string                 `json:"newapi_version"`
	CompiledBodySHA256 string                 `json:"compiled_body_sha256"`
	Count              AttestedRelayCount     `json:"count"`
	ExecutionRequestID string                 `json:"execution_request_id"`
	Usage              AttestedRelayUsage     `json:"usage"`
	Reasoning          AttestedRelayReasoning `json:"reasoning"`
	Terminal           string                 `json:"terminal"`
}

type AttestedRelayEnvelope struct {
	Object    string               `json:"object"`
	Receipt   AttestedRelayReceipt `json:"receipt"`
	Signature string               `json:"signature"`
}

type AttestedRelayErrorEnvelope struct {
	Object string `json:"object"`
	Error  struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		AttemptID string `json:"attempt_id"`
	} `json:"error"`
}

func IsAttestationRequested(c *gin.Context) bool {
	return c != nil && strings.TrimSpace(c.GetHeader(AttestationVersionHeader)) != ""
}

func StartAttestedRelay(c *gin.Context, info *RelayInfo, compiledBody []byte) (*AttestedRelayState, error) {
	if c == nil || info == nil {
		return nil, errors.New("attested relay requires request context and relay info")
	}
	if c.GetHeader(AttestationVersionHeader) != attestationVersion {
		return nil, errors.New("unsupported attestation version")
	}
	if !info.IsStream || !info.ShouldIncludeUsage {
		return nil, errors.New("attested relay requires streaming with include_usage")
	}
	if info.ChannelSetting.PassThroughBodyEnabled {
		return nil, errors.New("attested relay forbids pass-through request bodies")
	}
	if info.ChannelSetting.ThinkingToContent {
		return nil, errors.New("attested relay forbids mixing reasoning into content")
	}

	policy := strings.TrimSpace(c.GetHeader(AttestationPolicyHeader))
	attempt := strings.TrimSpace(c.GetHeader(AttestationAttemptHeader))
	logicalModel := strings.TrimSpace(c.GetHeader(AttestationLogicalModelHeader))
	expectedRoute := strings.TrimSpace(c.GetHeader(AttestationExpectedRouteHeader))
	for name, value := range map[string]string{
		"policy version": policy,
		"attempt id":     attempt,
		"logical model":  logicalModel,
		"expected route": expectedRoute,
	} {
		if !attestationIdentityPattern.MatchString(value) {
			return nil, fmt.Errorf("invalid %s", name)
		}
	}

	secret := strings.TrimSpace(os.Getenv(attestationSecretEnv))
	if len(secret) < 32 {
		return nil, errors.New("attestation secret is not configured")
	}
	routeFingerprint := attestedRouteFingerprint(secret, info)
	if !hmac.Equal([]byte(expectedRoute), []byte(routeFingerprint)) {
		return nil, errors.New("selected route does not match pinned route")
	}

	authPayload := strings.Join([]string{
		attestationVersion,
		policy,
		attempt,
		logicalModel,
		info.OriginModelName,
		expectedRoute,
	}, "\n")
	expectedAuthorization := "v1=" + signAttestation(secret, []byte(authPayload))
	providedAuthorization := strings.TrimSpace(c.GetHeader(AttestationAuthorizationHeader))
	if !hmac.Equal([]byte(providedAuthorization), []byte(expectedAuthorization)) {
		return nil, errors.New("invalid attestation authorization")
	}

	digest := sha256.Sum256(compiledBody)
	state := &AttestedRelayState{
		PolicyVersion:      policy,
		AttemptID:          attempt,
		LogicalModel:       logicalModel,
		SelectedModel:      info.OriginModelName,
		UpstreamModel:      info.UpstreamModelName,
		RouteFingerprint:   routeFingerprint,
		CompiledBodySHA256: fmt.Sprintf("sha256:%x", digest),
	}
	info.AttestedRelay = state
	return state, nil
}

func (s *AttestedRelayState) SetCompiledRequest(compiledBody []byte, count *ProviderTokenCountReceipt) error {
	if s == nil || count == nil {
		return errors.New("provider token count receipt is missing")
	}
	if len(compiledBody) == 0 {
		return errors.New("compiled provider request is missing")
	}
	if count.InputTokens <= 0 || count.RequestID == "" || count.RequestBodySHA256 == "" || count.Source == "" {
		return errors.New("provider token count receipt is incomplete")
	}
	digest := sha256.Sum256(compiledBody)
	s.CompiledBodySHA256 = fmt.Sprintf("sha256:%x", digest)
	s.Count = count
	return nil
}

func (s *AttestedRelayState) ObserveClaudeResponse(response *dto.ClaudeResponse) error {
	if s == nil || response == nil {
		return errors.New("attested relay received an empty provider event")
	}
	switch response.Type {
	case "message_start":
		if response.Message == nil || strings.TrimSpace(response.Message.Model) == "" {
			return errors.New("provider message_start is missing the actual model")
		}
		s.ResponseModel = response.Message.Model
		if s.ResponseModel != s.UpstreamModel {
			return errors.New("provider response model does not match the compiled upstream model")
		}
		if response.Message.Usage == nil || response.Message.Usage.InputTokens <= 0 {
			return errors.New("provider message_start is missing native input usage")
		}
		s.ProviderPromptTokens = response.Message.Usage.InputTokens
	case "content_block_start":
		if response.ContentBlock == nil {
			return errors.New("provider content_block_start is missing its block")
		}
		switch response.ContentBlock.Type {
		case "thinking":
			if response.ContentBlock.Thinking != nil {
				s.ReasoningChars += len([]rune(strings.TrimSpace(*response.ContentBlock.Thinking)))
			}
		case "text":
			if response.ContentBlock.Text != nil {
				text := strings.TrimSpace(*response.ContentBlock.Text)
				if text != "" && s.ReasoningChars == 0 {
					return errors.New("provider emitted text before visible reasoning")
				}
				s.TextChars += len([]rune(text))
			}
		default:
			return fmt.Errorf("unsupported provider content block %q", response.ContentBlock.Type)
		}
	case "content_block_delta":
		if response.Delta == nil {
			return errors.New("provider content_block_delta is missing its delta")
		}
		switch response.Delta.Type {
		case "thinking_delta":
			if response.Delta.Thinking != nil {
				s.ReasoningChars += len([]rune(strings.TrimSpace(*response.Delta.Thinking)))
			}
		case "signature_delta":
			// A provider signature is opaque continuity metadata, not visible reasoning.
		case "text_delta":
			if response.Delta.Text != nil {
				text := strings.TrimSpace(*response.Delta.Text)
				if text != "" && s.ReasoningChars == 0 {
					return errors.New("provider emitted text before visible reasoning")
				}
				s.TextChars += len([]rune(text))
			}
		default:
			return fmt.Errorf("unsupported provider delta %q", response.Delta.Type)
		}
	case "content_block_stop":
	case "message_delta":
		if response.Usage == nil || response.Usage.OutputTokens <= 0 {
			return errors.New("provider message_delta is missing native output usage")
		}
		s.ProviderCompletionTokens = response.Usage.OutputTokens
	case "message_stop":
		s.ProviderDone = true
	default:
		return fmt.Errorf("unsupported provider event %q", response.Type)
	}
	return nil
}

func (s *AttestedRelayState) BuildSuccessEnvelope(info *RelayInfo, usage *dto.Usage, newAPIRequestID, executionRequestID string) (*AttestedRelayEnvelope, error) {
	if s == nil || info == nil || usage == nil {
		return nil, errors.New("attested relay terminal receipt is missing state")
	}
	if s.Count == nil {
		return nil, errors.New("provider token count receipt is missing")
	}
	if !s.ProviderDone || s.ResponseModel == "" {
		return nil, errors.New("provider stream did not reach message_stop")
	}
	if s.ReasoningChars <= 0 {
		return nil, errors.New("provider returned no visible reasoning")
	}
	if s.TextChars <= 0 {
		return nil, errors.New("provider returned no ordinary text")
	}
	if s.ProviderPromptTokens <= 0 || s.ProviderCompletionTokens <= 0 {
		return nil, errors.New("provider-native usage is incomplete")
	}
	if executionRequestID == "" {
		return nil, errors.New("provider execution request id is missing")
	}
	if usage.PromptTokens <= 0 || usage.CompletionTokens <= 0 || usage.TotalTokens <= 0 {
		return nil, errors.New("provider usage is incomplete")
	}
	if usage.PromptTokens != s.ProviderPromptTokens ||
		usage.CompletionTokens != s.ProviderCompletionTokens ||
		usage.TotalTokens != s.ProviderPromptTokens+s.ProviderCompletionTokens {
		return nil, errors.New("settlement usage does not match provider-native usage")
	}
	if usage.BillingUsage == nil || usage.BillingUsage.Estimated || usage.BillingUsage.Source != dto.BillingUsageSourceClaudeMessages {
		return nil, errors.New("provider billing usage receipt is missing")
	}

	receipt := AttestedRelayReceipt{
		Schema:             AttestedRelaySchema,
		PolicyVersion:      s.PolicyVersion,
		AttemptID:          s.AttemptID,
		LogicalModel:       s.LogicalModel,
		NewAPIRequestID:    newAPIRequestID,
		SelectedModel:      s.SelectedModel,
		UpstreamModel:      s.UpstreamModel,
		ResponseModel:      s.ResponseModel,
		RouteFingerprint:   s.RouteFingerprint,
		Compiler:           attestationCompiler,
		NewAPIVersion:      basecommon.Version,
		CompiledBodySHA256: s.CompiledBodySHA256,
		Count: AttestedRelayCount{
			Source:            s.Count.Source,
			InputTokens:       s.Count.InputTokens,
			RequestID:         s.Count.RequestID,
			RequestBodySHA256: s.Count.RequestBodySHA256,
		},
		ExecutionRequestID: executionRequestID,
		Usage: AttestedRelayUsage{
			Source:           dto.BillingUsageSourceClaudeMessages,
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			ReasoningTokens:  usage.CompletionTokenDetails.ReasoningTokens,
		},
		Reasoning: AttestedRelayReasoning{
			Source: "anthropic.thinking_delta",
			Chars:  s.ReasoningChars,
		},
		Terminal: "succeeded",
	}

	secret := strings.TrimSpace(os.Getenv(attestationSecretEnv))
	if len(secret) < 32 {
		return nil, errors.New("attestation secret is not configured")
	}
	payload, err := basecommon.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("marshal attested relay receipt: %w", err)
	}
	return &AttestedRelayEnvelope{
		Object:    AttestedRelaySchema,
		Receipt:   receipt,
		Signature: "v1=" + signAttestation(secret, payload),
	}, nil
}

func NewAttestedRelayErrorEnvelope(info *RelayInfo, code string) AttestedRelayErrorEnvelope {
	envelope := AttestedRelayErrorEnvelope{Object: "newapi.attested_relay.error"}
	envelope.Error.Code = code
	envelope.Error.Message = "attested relay rejected the execution"
	if info != nil && info.AttestedRelay != nil {
		envelope.Error.AttemptID = info.AttestedRelay.AttemptID
	}
	return envelope
}

func attestedRouteFingerprint(secret string, info *RelayInfo) string {
	payload := strings.Join([]string{
		"route-v1",
		fmt.Sprintf("%d", info.ChannelId),
		fmt.Sprintf("%d", info.ChannelType),
		strings.TrimRight(strings.TrimSpace(info.ChannelBaseUrl), "/"),
		info.UpstreamModelName,
	}, "\n")
	return "rt1_" + signAttestation(secret, []byte(payload))
}

func signAttestation(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
