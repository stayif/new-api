package relay

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

func TextHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)

	textReq, ok := info.Request.(*dto.GeneralOpenAIRequest)
	if !ok {
		return types.NewErrorWithStatusCode(fmt.Errorf("invalid request type, expected dto.GeneralOpenAIRequest, got %T", info.Request), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	request, err := common.DeepCopy(textReq)
	if err != nil {
		return types.NewError(fmt.Errorf("failed to copy request to GeneralOpenAIRequest: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	if request.WebSearchOptions != nil {
		c.Set("chat_completion_web_search_context_size", request.WebSearchOptions.SearchContextSize)
	}

	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}

	includeUsage := true
	// 判断用户是否需要返回使用情况
	if request.StreamOptions != nil {
		includeUsage = request.StreamOptions.IncludeUsage
	}

	// 如果不支持StreamOptions，将StreamOptions设置为nil
	if !info.SupportStreamOptions || !lo.FromPtrOr(request.Stream, false) {
		request.StreamOptions = nil
	} else {
		// 如果支持StreamOptions，且请求中没有设置StreamOptions，根据配置文件设置StreamOptions
		if constant.ForceStreamOption {
			request.StreamOptions = &dto.StreamOptions{
				IncludeUsage: true,
			}
		}
	}

	info.ShouldIncludeUsage = includeUsage

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)

	passThroughGlobal := model_setting.GetGlobalSettings().PassThroughRequestEnabled
	if relaycommon.IsHoneyAttestationRequested(c) &&
		(passThroughGlobal || info.ChannelSetting.PassThroughBodyEnabled) {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("attested relay forbids pass-through request bodies"),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if info.RelayMode == relayconstant.RelayModeChatCompletions &&
		!passThroughGlobal &&
		!info.ChannelSetting.PassThroughBodyEnabled &&
		shouldUseResponsesGlobal(c, info) {
		applySystemPromptIfNeeded(c, info, request)
		usage, newApiErr := chatCompletionsViaResponses(c, info, adaptor, request)
		if newApiErr != nil {
			return newApiErr
		}

		var containAudioTokens = usage.CompletionTokenDetails.AudioTokens > 0 || usage.PromptTokensDetails.AudioTokens > 0
		var containsAudioRatios = ratio_setting.ContainsAudioRatio(info.OriginModelName) || ratio_setting.ContainsAudioCompletionRatio(info.OriginModelName)

		if containAudioTokens && containsAudioRatios {
			service.PostAudioConsumeQuota(c, info, usage, "")
		} else {
			service.PostTextConsumeQuota(c, info, usage, nil)
		}
		return nil
	}

	var requestBody io.Reader

	if passThroughGlobal || info.ChannelSetting.PassThroughBodyEnabled {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if common.DebugEnabled {
			if debugBytes, bErr := storage.Bytes(); bErr == nil {
				logger.LogDebug(c, "requestBody: %s", debugBytes)
			}
		}
		requestBody = common.ReaderOnly(storage)
	} else {
		convertedRequest, err := adaptor.ConvertOpenAIRequest(c, info, request)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)

		if info.ChannelSetting.SystemPrompt != "" {
			// 如果有系统提示，则将其添加到请求中
			request, ok := convertedRequest.(*dto.GeneralOpenAIRequest)
			if ok {
				containSystemPrompt := false
				for _, message := range request.Messages {
					if message.Role == request.GetSystemRoleName() {
						containSystemPrompt = true
						break
					}
				}
				if !containSystemPrompt {
					// 如果没有系统提示，则添加系统提示
					systemMessage := dto.Message{
						Role:    request.GetSystemRoleName(),
						Content: info.ChannelSetting.SystemPrompt,
					}
					request.Messages = append([]dto.Message{systemMessage}, request.Messages...)
				} else if info.ChannelSetting.SystemPromptOverride {
					common.SetContextKey(c, constant.ContextKeySystemPromptOverride, true)
					// 如果有系统提示，且允许覆盖，则拼接到前面
					for i, message := range request.Messages {
						if message.Role == request.GetSystemRoleName() {
							if message.IsStringContent() {
								request.Messages[i].SetStringContent(info.ChannelSetting.SystemPrompt + "\n" + message.StringContent())
							} else {
								contents := message.ParseContent()
								contents = append([]dto.MediaContent{
									{
										Type: dto.ContentTypeText,
										Text: info.ChannelSetting.SystemPrompt,
									},
								}, contents...)
								request.Messages[i].Content = contents
							}
							break
						}
					}
				}
			}
		}

		jsonData, err := common.Marshal(convertedRequest)
		if err != nil {
			return types.NewError(err, types.ErrorCodeJsonMarshalFailed, types.ErrOptionWithSkipRetry())
		}

		// remove disabled fields for OpenAI API
		jsonData, err = relaycommon.RemoveDisabledFields(jsonData, info.ChannelOtherSettings, info.ChannelSetting.PassThroughBodyEnabled)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}

		// apply param override
		if len(info.ParamOverride) > 0 {
			jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
			if err != nil {
				return newAPIErrorFromParamOverride(err)
			}
		}

		if relaycommon.IsHoneyAttestationRequested(c) {
			if _, err = relaycommon.StartHoneyAttestedRelay(c, info, jsonData); err != nil {
				return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
		}

		logger.LogDebug(c, "text request body: %s", jsonData)

		body, size, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		defer closer.Close()
		jsonData = nil
		info.UpstreamRequestBodySize = size
		requestBody = body
	}

	var httpResp *http.Response
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")

	if resp != nil {
		httpResp = resp.(*http.Response)
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		if httpResp.StatusCode != http.StatusOK {
			newApiErr := service.RelayErrorHandler(c.Request.Context(), httpResp, false)
			// reset status code 重置状态码
			service.ResetStatusCode(newApiErr, statusCodeMappingStr)
			return newApiErr
		}
	}

	usage, newApiErr := adaptor.DoResponse(c, httpResp, info)
	if newApiErr != nil {
		if info.HoneyAttestedRelay != nil {
			if writeErr := finishHoneyAttestedFailure(c, info, "provider_stream_invalid"); writeErr != nil {
				logger.LogError(c, "error writing attested relay provider failure: "+writeErr.Error())
			}
			// Once an attested terminal has been emitted, retrying this request could
			// append a second execution and terminal to the same client stream.
			newApiErr = markHoneyAttestedFailureNoRetry(newApiErr)
		}
		// reset status code 重置状态码
		service.ResetStatusCode(newApiErr, statusCodeMappingStr)
		return newApiErr
	}

	var containAudioTokens = usage.(*dto.Usage).CompletionTokenDetails.AudioTokens > 0 || usage.(*dto.Usage).PromptTokensDetails.AudioTokens > 0
	var containsAudioRatios = ratio_setting.ContainsAudioRatio(info.OriginModelName) || ratio_setting.ContainsAudioCompletionRatio(info.OriginModelName)

	if containAudioTokens && containsAudioRatios {
		if info.HoneyAttestedRelay != nil {
			return types.NewError(fmt.Errorf("attested relay does not support audio settlement"), types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
		}
		service.PostAudioConsumeQuota(c, info, usage.(*dto.Usage), "")
	} else {
		if info.HoneyAttestedRelay != nil {
			newAPIRequestID := c.GetString(common.RequestIdKey)
			executionRequestID := c.GetString(common.UpstreamRequestIdKey)
			if receiptErr := info.HoneyAttestedRelay.ValidateSuccessPrerequisites(info, newAPIRequestID, executionRequestID); receiptErr != nil {
				if writeErr := finishHoneyAttestedStream(c, relaycommon.NewHoneyAttestedErrorEnvelope(info, "attestation_failed")); writeErr != nil {
					logger.LogError(c, "error writing attested relay terminal failure: "+writeErr.Error())
				}
				return types.NewError(receiptErr, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
			}
			settlement, settlementErr := service.PostTextConsumeQuota(c, info, usage.(*dto.Usage), nil)
			if settlementErr != nil {
				if writeErr := finishHoneyAttestedStream(c, relaycommon.NewHoneyAttestedErrorEnvelope(info, "settlement_failed")); writeErr != nil {
					logger.LogError(c, "error writing attested relay settlement failure: "+writeErr.Error())
				}
				return types.NewError(settlementErr, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
			}
			envelope, receiptErr := info.HoneyAttestedRelay.BuildSuccessEnvelope(
				info,
				settlement,
				newAPIRequestID,
				executionRequestID,
			)
			if receiptErr != nil {
				if writeErr := finishHoneyAttestedStream(c, relaycommon.NewHoneyAttestedErrorEnvelope(info, "attestation_failed")); writeErr != nil {
					logger.LogError(c, "error writing attested relay signing failure: "+writeErr.Error())
				}
				return types.NewError(receiptErr, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
			}
			if writeErr := finishHoneyAttestedStream(c, envelope); writeErr != nil {
				return types.NewError(writeErr, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
			}
		} else {
			service.PostTextConsumeQuota(c, info, usage.(*dto.Usage), nil)
		}
	}
	return nil
}

func finishHoneyAttestedStream(c *gin.Context, envelope interface{}) error {
	if writeErr := helper.ObjectData(c, envelope); writeErr != nil {
		return writeErr
	}
	relaycommon.MarkHoneyAttestedTerminalWritten(c)
	if writeErr := helper.StringData(c, "[DONE]"); writeErr != nil {
		return writeErr
	}
	return nil
}

func finishHoneyAttestedFailure(c *gin.Context, info *relaycommon.RelayInfo, code string) error {
	if relaycommon.HoneyAttestedTerminalWritten(c) {
		return nil
	}
	return finishHoneyAttestedStream(c, relaycommon.NewHoneyAttestedErrorEnvelope(info, code))
}

func markHoneyAttestedFailureNoRetry(newAPIError *types.NewAPIError) *types.NewAPIError {
	if newAPIError == nil {
		return nil
	}
	return types.NewError(newAPIError, newAPIError.GetErrorCode(), types.ErrOptionWithSkipRetry())
}

func shouldUseResponsesGlobal(c *gin.Context, info *relaycommon.RelayInfo) bool {
	if relaycommon.IsHoneyAttestationRequested(c) {
		return false
	}
	return service.ShouldChatCompletionsUseResponsesGlobal(info.ChannelId, info.ChannelType, info.OriginModelName)
}
