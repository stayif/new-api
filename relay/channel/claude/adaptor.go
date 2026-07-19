package claude

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type Adaptor struct {
}

func (a *Adaptor) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	return request, nil
}

func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	requestURL := fmt.Sprintf("%s/v1/messages", info.ChannelBaseUrl)
	if !shouldAppendClaudeBetaQuery(info) {
		return requestURL, nil
	}

	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return "", err
	}
	query := parsedURL.Query()
	query.Set("beta", "true")
	parsedURL.RawQuery = query.Encode()
	return parsedURL.String(), nil
}

func shouldAppendClaudeBetaQuery(info *relaycommon.RelayInfo) bool {
	if info == nil {
		return false
	}
	if info.IsClaudeBetaQuery {
		return true
	}
	if info.ChannelOtherSettings.ClaudeBetaQuery {
		return true
	}
	return false
}

func CommonClaudeHeadersOperation(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) {
	// common headers operation
	anthropicBeta := c.Request.Header.Get("anthropic-beta")
	if anthropicBeta != "" {
		req.Set("anthropic-beta", anthropicBeta)
	}
	model_setting.GetClaudeSettings().WriteHeaders(info.OriginModelName, req)
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	channel.SetupApiRequestHeader(info, c, req)
	req.Set("x-api-key", info.ApiKey)
	anthropicVersion := c.Request.Header.Get("anthropic-version")
	if anthropicVersion == "" {
		anthropicVersion = "2023-06-01"
	}
	req.Set("anthropic-version", anthropicVersion)
	CommonClaudeHeadersOperation(c, req, info)
	return nil
}

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	result, err := relayconvert.ConvertRequest(c, info, types.RelayFormatClaude, request)
	if err != nil {
		return nil, err
	}
	return result.Value, nil
}

func (a *Adaptor) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, nil
}

func (a *Adaptor) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	// TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Adaptor) CompileAndCountProviderTokens(c *gin.Context, info *relaycommon.RelayInfo, compiledBody []byte) ([]byte, *relaycommon.ProviderTokenCountReceipt, error) {
	if c == nil || info == nil {
		return nil, nil, errors.New("provider token count requires request context and relay info")
	}
	var compiled dto.ClaudeRequest
	if err := common.Unmarshal(compiledBody, &compiled); err != nil {
		return nil, nil, fmt.Errorf("decode compiled Anthropic request: %w", err)
	}
	if compiled.Model == "" || compiled.Model != info.UpstreamModelName {
		return nil, nil, errors.New("compiled Anthropic model does not match selected upstream model")
	}
	if compiled.Stream == nil || !*compiled.Stream {
		return nil, nil, errors.New("attested Anthropic execution must be streaming")
	}
	if compiled.Thinking == nil || compiled.Thinking.Type != "enabled" || compiled.Thinking.GetBudgetTokens() < 1024 {
		return nil, nil, errors.New("attested Anthropic execution requires explicitly enabled thinking")
	}
	if compiled.MaxTokens == nil || int(*compiled.MaxTokens) <= compiled.Thinking.GetBudgetTokens() {
		return nil, nil, errors.New("compiled Anthropic max_tokens must leave room for ordinary text")
	}
	if compiled.Thinking.Display != "" && compiled.Thinking.Display != "summarized" {
		return nil, nil, errors.New("attested Anthropic execution requires summarized thinking")
	}
	compiled.Thinking.Display = "summarized"
	compiledBody, err := common.Marshal(compiled)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal final Anthropic request: %w", err)
	}

	var compiledMap map[string]any
	if err := common.Unmarshal(compiledBody, &compiledMap); err != nil {
		return nil, nil, fmt.Errorf("decode compiled Anthropic body: %w", err)
	}
	countBody := make(map[string]any)
	for _, name := range []string{"model", "messages", "system", "tools", "tool_choice", "thinking"} {
		if value, ok := compiledMap[name]; ok {
			countBody[name] = value
		}
	}
	countJSON, err := common.Marshal(countBody)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Anthropic count request: %w", err)
	}

	countURL := strings.TrimRight(info.ChannelBaseUrl, "/") + "/v1/messages/count_tokens"
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, countURL, bytes.NewReader(countJSON))
	if err != nil {
		return nil, nil, fmt.Errorf("create Anthropic count request: %w", err)
	}
	if err := channel.ApplyAdaptorRequestHeaders(a, c, info, req); err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(countJSON))

	httpClient := service.GetHttpClient()
	if info.ChannelSetting.Proxy != "" {
		httpClient, err = service.NewProxyHttpClient(info.ChannelSetting.Proxy)
		if err != nil {
			return nil, nil, fmt.Errorf("create provider count proxy client: %w", err)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("provider token count request failed: %w", err)
	}
	defer service.CloseResponseBodyGracefully(resp)
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read provider token count response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("provider token count returned status %d", resp.StatusCode)
	}
	var countResponse struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := common.Unmarshal(responseBody, &countResponse); err != nil {
		return nil, nil, fmt.Errorf("decode provider token count response: %w", err)
	}
	requestID := channel.CaptureProviderRequestID(nil, resp.Header)
	if countResponse.InputTokens <= 0 || requestID == "" {
		return nil, nil, errors.New("provider token count response is missing tokens or request id")
	}
	digest := sha256.Sum256(countJSON)
	return compiledBody, &relaycommon.ProviderTokenCountReceipt{
		InputTokens:       countResponse.InputTokens,
		RequestID:         requestID,
		RequestBodySHA256: fmt.Sprintf("sha256:%x", digest),
		Source:            "anthropic.messages.count_tokens",
	}, nil
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	info.FinalRequestRelayFormat = types.RelayFormatClaude
	if info.IsStream {
		return ClaudeStreamHandler(c, resp, info)
	} else {
		return ClaudeHandler(c, resp, info)
	}
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}
