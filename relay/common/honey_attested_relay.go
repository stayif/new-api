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
	HoneyAttestedRelaySchema = "newapi.attested_relay.v3"
	honeyAttestationVersion  = "2"
	honeyAttestationSecret   = "HONEY_ATTESTATION_SECRET"
)

var honeyIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type HoneyNewAPISettlement struct {
	Amount            int64   `json:"amount"`
	Unit              string  `json:"unit"`
	Kind              string  `json:"kind"`
	Source            string  `json:"source"`
	BillingVersion    string  `json:"billing_version"`
	MultiplierVersion string  `json:"multiplier_version"`
	ModelRatio        float64 `json:"model_ratio"`
	CompletionRatio   float64 `json:"completion_ratio"`
	GroupRatio        float64 `json:"group_ratio"`
	ModelPrice        float64 `json:"model_price"`
}

type HoneyProviderReportedUsage struct {
	Status           string `json:"status"`
	Source           string `json:"source,omitempty"`
	Unit             string `json:"unit,omitempty"`
	Scope            string `json:"scope,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	TotalTokens      int    `json:"total_tokens,omitempty"`
	ReasoningTokens  int    `json:"reasoning_tokens,omitempty"`
}

type HoneyUsageComparability struct {
	Status string   `json:"status"`
	Ratio  *float64 `json:"ratio,omitempty"`
}

type HoneyAttestedReceipt struct {
	Schema             string                     `json:"schema"`
	PolicyVersion      string                     `json:"policy_version"`
	AttemptID          string                     `json:"attempt_id"`
	LogicalModel       string                     `json:"logical_model"`
	NewAPIRequestID    string                     `json:"newapi_request_id"`
	SelectedModel      string                     `json:"selected_model"`
	UpstreamModel      string                     `json:"upstream_model"`
	ResponseModel      string                     `json:"response_model"`
	RouteFingerprint   string                     `json:"route_fingerprint"`
	Compiler           string                     `json:"compiler"`
	NewAPIVersion      string                     `json:"newapi_version"`
	CompiledBodySHA256 string                     `json:"compiled_body_sha256"`
	ExecutionRequestID string                     `json:"execution_request_id"`
	NewAPISettlement   HoneyNewAPISettlement      `json:"newapi_settlement"`
	ProviderReported   HoneyProviderReportedUsage `json:"provider_reported"`
	UsageComparability HoneyUsageComparability    `json:"usage_comparability"`
	Reasoning          HoneyReasoningReceipt      `json:"reasoning"`
	Terminal           string                     `json:"terminal"`
}

type HoneyReasoningReceipt struct {
	Source string `json:"source"`
	Chars  int    `json:"chars"`
}

type HoneyAttestedEnvelope struct {
	Object    string               `json:"object"`
	Receipt   HoneyAttestedReceipt `json:"receipt"`
	Signature string               `json:"signature"`
}

type HoneyAttestedErrorEnvelope struct {
	Object string `json:"object"`
	Error  struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		AttemptID string `json:"attempt_id"`
	} `json:"error"`
}

type HoneyAttestedRelay struct {
	PolicyVersion      string
	AttemptID          string
	LogicalModel       string
	SelectedModel      string
	UpstreamModel      string
	RouteFingerprint   string
	CompiledBodySHA256 string
	ResponseModel      string
	ReasoningChars     int
	TextChars          int
	PromptTokens       int
	CompletionTokens   int
	ReasoningTokens    int
	MessageStop        bool
	TextStarted        bool
}

func IsHoneyAttestationRequested(c *gin.Context) bool {
	return c != nil && strings.TrimSpace(c.GetHeader("X-NewAPI-Attestation-Version")) != ""
}

func StartHoneyAttestedRelay(c *gin.Context, info *RelayInfo, compiledBody []byte) (*HoneyAttestedRelay, error) {
	if c == nil || info == nil || info.ChannelMeta == nil {
		return nil, errors.New("attested relay requires relay metadata")
	}
	if c.GetHeader("X-NewAPI-Attestation-Version") != honeyAttestationVersion {
		return nil, errors.New("unsupported attestation version")
	}
	if !info.IsStream || !info.ShouldIncludeUsage || info.ChannelType != 14 {
		return nil, errors.New("attested relay requires a streaming Claude route with usage")
	}
	if info.ChannelSetting.PassThroughBodyEnabled || info.ChannelSetting.ThinkingToContent {
		return nil, errors.New("attested relay rejects reasoning-altering channel settings")
	}
	policy := strings.TrimSpace(c.GetHeader("X-Honey-Policy-Version"))
	attempt := strings.TrimSpace(c.GetHeader("X-Honey-Attempt-Id"))
	logical := strings.TrimSpace(c.GetHeader("X-Honey-Logical-Model"))
	expectedRoute := strings.TrimSpace(c.GetHeader("X-Honey-Expected-Route"))
	for name, value := range map[string]string{"policy version": policy, "attempt id": attempt, "logical model": logical, "expected route": expectedRoute} {
		if !honeyIdentityPattern.MatchString(value) {
			return nil, fmt.Errorf("invalid %s", name)
		}
	}
	secret := strings.TrimSpace(os.Getenv(honeyAttestationSecret))
	if len(secret) < 32 {
		return nil, errors.New("attestation secret is not configured")
	}
	route := honeyRouteFingerprint(secret, info)
	if !hmac.Equal([]byte(expectedRoute), []byte(route)) {
		return nil, errors.New("selected route does not match pinned route")
	}
	authPayload := strings.Join([]string{honeyAttestationVersion, policy, attempt, logical, info.OriginModelName, expectedRoute}, "\n")
	if !hmac.Equal([]byte(c.GetHeader("X-Honey-Attestation")), []byte("v2="+honeySign(secret, []byte(authPayload)))) {
		return nil, errors.New("invalid attestation authorization")
	}
	digest := sha256.Sum256(compiledBody)
	state := &HoneyAttestedRelay{PolicyVersion: policy, AttemptID: attempt, LogicalModel: logical, SelectedModel: info.OriginModelName, UpstreamModel: info.UpstreamModelName, RouteFingerprint: route, CompiledBodySHA256: fmt.Sprintf("sha256:%x", digest)}
	info.HoneyAttestedRelay = state
	return state, nil
}

func (s *HoneyAttestedRelay) ObserveClaudeResponse(response *dto.ClaudeResponse) error {
	if s == nil || response == nil {
		return errors.New("attested relay received an empty provider event")
	}
	switch response.Type {
	case "message_start":
		if response.Message == nil || response.Message.Model == "" || response.Message.Model != s.UpstreamModel {
			return errors.New("provider message_start has an unexpected model")
		}
		s.ResponseModel = response.Message.Model
		if response.Message.Usage != nil && response.Message.Usage.InputTokens > 0 {
			s.PromptTokens = response.Message.Usage.InputTokens
		}
	case "content_block_start":
		if response.ContentBlock == nil {
			return errors.New("provider content block is missing")
		}
		if response.ContentBlock.Type == "thinking" && response.ContentBlock.Thinking != nil {
			if err := s.observeReasoning(*response.ContentBlock.Thinking); err != nil {
				return err
			}
		}
		if response.ContentBlock.Type == "text" && response.ContentBlock.Text != nil {
			if err := s.observeText(*response.ContentBlock.Text); err != nil {
				return err
			}
		}
	case "content_block_delta":
		if response.Delta == nil {
			return errors.New("provider delta is missing")
		}
		switch response.Delta.Type {
		case "thinking_delta":
			if response.Delta.Thinking != nil {
				if err := s.observeReasoning(*response.Delta.Thinking); err != nil {
					return err
				}
			}
		case "text_delta":
			if response.Delta.Text != nil {
				if err := s.observeText(*response.Delta.Text); err != nil {
					return err
				}
			}
		case "signature_delta":
		default:
			return fmt.Errorf("unsupported provider delta %q", response.Delta.Type)
		}
	case "message_delta":
		if response.Usage != nil && response.Usage.OutputTokens > 0 {
			s.CompletionTokens = response.Usage.OutputTokens
		}
	case "message_stop":
		s.MessageStop = true
	}
	return nil
}

func (s *HoneyAttestedRelay) observeText(value string) error {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil
	}
	if s.ReasoningChars == 0 {
		return errors.New("provider emitted text before visible reasoning")
	}
	s.TextStarted = true
	s.TextChars += len([]rune(text))
	return nil
}

func (s *HoneyAttestedRelay) observeReasoning(value string) error {
	if s.TextStarted {
		return errors.New("provider emitted reasoning after text")
	}
	if strings.TrimSpace(value) != "" || s.ReasoningChars > 0 {
		s.ReasoningChars += len([]rune(value))
	}
	return nil
}

func (s *HoneyAttestedRelay) BuildSuccessEnvelope(info *RelayInfo, settlement HoneyNewAPISettlement, newAPIRequestID, executionRequestID string) (*HoneyAttestedEnvelope, error) {
	if s == nil || info == nil || !s.MessageStop || s.ResponseModel == "" || s.ReasoningChars == 0 || s.TextChars == 0 || newAPIRequestID == "" || executionRequestID == "" {
		return nil, errors.New("attested relay terminal contract is incomplete")
	}
	provider := HoneyProviderReportedUsage{Status: "unknown"}
	comparability := HoneyUsageComparability{Status: "not_comparable"}
	if s.PromptTokens > 0 && s.CompletionTokens > 0 {
		provider = HoneyProviderReportedUsage{Status: "reported", Source: "claude_messages", Unit: "tokens", Scope: "input_output_total", PromptTokens: s.PromptTokens, CompletionTokens: s.CompletionTokens, TotalTokens: s.PromptTokens + s.CompletionTokens, ReasoningTokens: s.ReasoningTokens}
		if settlement.Unit == "tokens" && settlement.Kind == "unconverted_tokens" {
			ratio := float64(absInt64(settlement.Amount-int64(provider.TotalTokens))) / float64(provider.TotalTokens)
			status := "within_tolerance"
			if ratio > 0.5 {
				status = "outside_tolerance"
			}
			comparability = HoneyUsageComparability{Status: status, Ratio: &ratio}
		}
	}
	receipt := HoneyAttestedReceipt{Schema: HoneyAttestedRelaySchema, PolicyVersion: s.PolicyVersion, AttemptID: s.AttemptID, LogicalModel: s.LogicalModel, NewAPIRequestID: newAPIRequestID, SelectedModel: s.SelectedModel, UpstreamModel: s.UpstreamModel, ResponseModel: s.ResponseModel, RouteFingerprint: s.RouteFingerprint, Compiler: "openai_chat_completions_to_anthropic_messages", NewAPIVersion: basecommon.Version, CompiledBodySHA256: s.CompiledBodySHA256, ExecutionRequestID: executionRequestID, NewAPISettlement: settlement, ProviderReported: provider, UsageComparability: comparability, Reasoning: HoneyReasoningReceipt{Source: "anthropic.thinking_delta", Chars: s.ReasoningChars}, Terminal: "succeeded"}
	secret := strings.TrimSpace(os.Getenv(honeyAttestationSecret))
	if len(secret) < 32 {
		return nil, errors.New("attestation secret is not configured")
	}
	payload, err := basecommon.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	return &HoneyAttestedEnvelope{Object: HoneyAttestedRelaySchema, Receipt: receipt, Signature: "v3=" + honeySign(secret, payload)}, nil
}

func honeyRouteFingerprint(secret string, info *RelayInfo) string {
	payload := strings.Join([]string{"route-v1", fmt.Sprintf("%d", info.ChannelId), fmt.Sprintf("%d", info.ChannelType), strings.TrimRight(strings.TrimSpace(info.ChannelBaseUrl), "/"), info.UpstreamModelName}, "\n")
	return "rt1_" + honeySign(secret, []byte(payload))
}

func NewHoneyAttestedErrorEnvelope(info *RelayInfo, code string) HoneyAttestedErrorEnvelope {
	envelope := HoneyAttestedErrorEnvelope{Object: "newapi.attested_relay.error"}
	envelope.Error.Code = code
	envelope.Error.Message = "attested relay rejected the execution"
	if info != nil && info.HoneyAttestedRelay != nil {
		envelope.Error.AttemptID = info.HoneyAttestedRelay.AttemptID
	}
	return envelope
}

func honeySign(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
