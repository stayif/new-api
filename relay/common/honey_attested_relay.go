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
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const (
	HoneyAttestedRelaySchema   = "newapi.attested_relay.v3"
	maxHoneyBufferedTextBytes  = 256 << 10
	maxHoneyBufferedTextDeltas = 4096
	honeyAttestationVersion    = "2"
	honeyAttestationSecret     = "HONEY_ATTESTATION_SECRET"
	honeyTerminalWrittenKey    = "honey_attested_terminal_written"
)

func MarkHoneyAttestedTerminalWritten(c *gin.Context) {
	if c != nil {
		c.Set(honeyTerminalWrittenKey, true)
	}
}

func HoneyAttestedTerminalWritten(c *gin.Context) bool {
	return c != nil && c.GetBool(honeyTerminalWrittenKey)
}

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
	PolicyVersion       string
	AttemptID           string
	LogicalModel        string
	SelectedModel       string
	UpstreamModel       string
	RouteFingerprint    string
	CompiledBodySHA256  string
	ResponseModel       string
	ReasoningChars      int
	ReasoningVisible    bool
	TextChars           int
	PromptTokens        int
	CompletionTokens    int
	ReasoningTokens     int
	MessageStarted      bool
	MessageDeltaSeen    bool
	MessageStop         bool
	TextStarted         bool
	bufferedText        []string
	bufferedTextBytes   int
	releasedText        []string
	currentTextBuffered bool
	protocolInvalid     bool
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
	if !info.IsStream || !info.ShouldIncludeUsage || info.ChannelType != 14 || info.RelayFormat != types.RelayFormatOpenAI {
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

func (s *HoneyAttestedRelay) ObserveClaudeResponse(response *dto.ClaudeResponse) (err error) {
	if s == nil {
		return errors.New("attested relay received an empty provider event")
	}
	defer func() {
		if err != nil {
			s.protocolInvalid = true
		}
	}()
	if response == nil {
		return errors.New("attested relay received an empty provider event")
	}
	if s.MessageStop {
		return errors.New("provider emitted an event after message_stop")
	}
	if s.MessageDeltaSeen && response.Type != "message_stop" && response.Type != "ping" {
		return errors.New("provider emitted an event after message_delta")
	}
	s.currentTextBuffered = false
	if response.Type != "message_start" && response.Type != "ping" && !s.MessageStarted {
		return errors.New("provider emitted content before message_start")
	}
	switch response.Type {
	case "message_start":
		if s.MessageStarted {
			return errors.New("provider emitted duplicate message_start")
		}
		if response.Message == nil || response.Message.Model == "" || response.Message.Model != s.UpstreamModel {
			return errors.New("provider message_start has an unexpected model")
		}
		s.MessageStarted = true
		s.ResponseModel = response.Message.Model
		if response.Message.Usage != nil && response.Message.Usage.InputTokens > 0 {
			s.PromptTokens = response.Message.Usage.InputTokens
		}
	case "content_block_start":
		if response.ContentBlock == nil {
			return errors.New("provider content block is missing")
		}
		switch response.ContentBlock.Type {
		case "thinking":
			if response.ContentBlock.Thinking != nil {
				if err := s.observeReasoning(*response.ContentBlock.Thinking); err != nil {
					return err
				}
			}
		case "text":
			if response.ContentBlock.Text != nil {
				if err := s.observeText(*response.ContentBlock.Text); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported provider content block %q", response.ContentBlock.Type)
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
			if response.Delta.Signature == "" || response.Delta.Text != nil ||
				response.Delta.Thinking != nil || response.Delta.PartialJson != nil ||
				response.Delta.Content != nil || response.Delta.Input != nil {
				return errors.New("provider signature delta is malformed")
			}
		default:
			return fmt.Errorf("unsupported provider delta %q", response.Delta.Type)
		}
	case "message_delta":
		s.MessageDeltaSeen = true
		if response.Usage != nil && response.Usage.OutputTokens > 0 {
			s.CompletionTokens = response.Usage.OutputTokens
		}
		if err := s.releaseBufferedText(); err != nil {
			return err
		}
	case "content_block_stop":
	case "ping":
		if response.Id != "" || response.Role != "" || len(response.Content) != 0 ||
			response.Completion != "" || response.StopReason != "" || response.Model != "" ||
			response.Error != nil || response.Usage != nil || response.Index != nil ||
			response.ContentBlock != nil || response.Delta != nil || response.Message != nil {
			return errors.New("provider ping contained semantic fields")
		}
	case "message_stop":
		if err := s.releaseBufferedText(); err != nil {
			return err
		}
		s.MessageStop = true
	default:
		return fmt.Errorf("unsupported provider event %q", response.Type)
	}
	return nil
}

func (s *HoneyAttestedRelay) observeText(value string) error {
	text := strings.TrimSpace(value)
	if !s.ReasoningVisible || len(s.bufferedText) > 0 {
		s.currentTextBuffered = true
		if value == "" {
			return nil
		}
		if len(s.bufferedText) >= maxHoneyBufferedTextDeltas ||
			s.bufferedTextBytes+len(value) > maxHoneyBufferedTextBytes {
			return errors.New("provider text before reasoning exceeded the relay buffer")
		}
		s.bufferedText = append(s.bufferedText, value)
		s.bufferedTextBytes += len(value)
		return nil
	}
	if text == "" {
		return nil
	}
	s.TextStarted = true
	s.TextChars += len([]rune(text))
	return nil
}

// CurrentTextBuffered reports whether the current provider text event was held
// back from the client. Once an upstream emits text before visible reasoning,
// every later text delta is buffered as well so the original text order can be
// preserved when the lane is released.
func (s *HoneyAttestedRelay) CurrentTextBuffered() bool {
	return s != nil && s.currentTextBuffered
}

// TakeReleasedText returns provider text made eligible for emission by a
// message_delta or message_stop after visible reasoning. The relay emits these
// deltas before forwarding that terminal provider event. If reasoning never
// becomes visible, releaseBufferedText fails closed and no buffered text is
// exposed.
func (s *HoneyAttestedRelay) TakeReleasedText() []string {
	if s == nil || len(s.releasedText) == 0 {
		return nil
	}
	released := append([]string(nil), s.releasedText...)
	s.releasedText = nil
	return released
}

func (s *HoneyAttestedRelay) releaseBufferedText() error {
	if len(s.bufferedText) == 0 {
		return nil
	}
	if !s.ReasoningVisible {
		return errors.New("provider completed without visible reasoning")
	}
	for _, value := range s.bufferedText {
		text := strings.TrimSpace(value)
		if text != "" {
			s.TextChars += len([]rune(text))
		}
		s.releasedText = append(s.releasedText, value)
	}
	s.bufferedText = nil
	s.bufferedTextBytes = 0
	if len(s.releasedText) > 0 {
		s.TextStarted = true
	}
	return nil
}

func (s *HoneyAttestedRelay) observeReasoning(value string) error {
	if s.TextStarted {
		return errors.New("provider emitted reasoning after text")
	}
	s.ReasoningChars += len([]rune(value))
	if strings.TrimSpace(value) != "" {
		s.ReasoningVisible = true
	}
	return nil
}

func (s *HoneyAttestedRelay) ValidateSuccessPrerequisites(info *RelayInfo, newAPIRequestID, executionRequestID string) error {
	if s == nil || info == nil || !s.MessageStarted || !s.MessageStop || s.ResponseModel == "" ||
		!s.ReasoningVisible || s.ReasoningChars == 0 || s.TextChars == 0 ||
		s.protocolInvalid || newAPIRequestID == "" || executionRequestID == "" {
		return errors.New("attested relay terminal contract is incomplete")
	}
	if len(strings.TrimSpace(os.Getenv(honeyAttestationSecret))) < 32 {
		return errors.New("attestation secret is not configured")
	}
	return nil
}

func (s *HoneyAttestedRelay) BuildSuccessEnvelope(info *RelayInfo, settlement HoneyNewAPISettlement, newAPIRequestID, executionRequestID string) (*HoneyAttestedEnvelope, error) {
	if err := s.ValidateSuccessPrerequisites(info, newAPIRequestID, executionRequestID); err != nil {
		return nil, err
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
