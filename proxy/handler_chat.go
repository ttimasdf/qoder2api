package proxy

import (
	"bufio"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ttimasdf/qoder2api/api"
	"github.com/ttimasdf/qoder2api/auth"
	"github.com/ttimasdf/qoder2api/database"
	"github.com/ttimasdf/qoder2api/security"
	"github.com/tidwall/gjson"
)

// ChatCompletions 是 qoder2api 唯一的下游入口：OpenAI 兼容的 /v1/chat/completions。
//
// 上游（Qoder / Qoder CN）原生说 chat/completions，因此这里不做任何协议翻译，
// 仅做：鉴权 → 账号池调度 → Cosy 签名转发 → 流式/非流式透传 → 计费埋点。
// 所有 Codex Responses / Anthropic / 图片转换逻辑已移除。
func (h *Handler) ChatCompletions(c *gin.Context) {
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	supportedModels := h.supportedModelIDs(c.Request.Context())
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredModelMappingToBody(rawBody, supportedModels)

	validator := api.NewValidator(rawBody)
	rules := api.ChatCompletionValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(supportedModels))
	if result := validator.ValidateRequest(rules); !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if mappedModel != "" {
		model = mappedModel
	}
	logModel := requestModel
	if logModel == "" {
		logModel = model
	}
	if model == "" {
		model = "qwen3-coder"
		logModel = model
	}
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/chat/completions", model) {
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	reasoningEffort := extractReasoningEffort(rawBody)

	logEffectiveModel := usageEffectiveModelForMapping(logModel, model, mappingApplied)
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := h.withModelCooldownFilter(model, nil)

	sessionID := ResolveSessionID(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionID, apiKeyID)

	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()

	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	for attempt := 0; ; attempt++ {
		account, stickyProxyURL := h.nextRetryAccountForSession(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter)
		if account == nil {
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(model))
			return
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)

		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		lastUpstreamCancel = upstreamCancel

		resp, reqErr := ExecuteQoderRequest(upstreamCtx, account, rawBody, proxyURL, c.Request.Header)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			kind := classifyTransportFailure(reqErr)
			retryable := IsRetryableError(reqErr) || kind != ""
			shouldRetry := retryable && shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
			if kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHard(account.ID())
			if !retryable {
				ErrorToGinResponse(c, reqErr)
				return
			}
			log.Printf("Qoder 上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			if shouldRetry {
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHard(account.ID())

			log.Printf("Qoder 上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/chat/completions", resp.StatusCode, logModel, account.ID(), errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, model)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          "/v1/chat/completions",
				Model:             logModel,
				EffectiveModel:    logEffectiveModel,
				StatusCode:        resp.StatusCode,
				DurationMs:        durationMs,
				ReasoningEffort:   reasoningEffort,
				InboundEndpoint:   "/v1/chat/completions",
				UpstreamEndpoint:  "/chat/completions",
				Stream:            isStream,
				IsRetryAttempt:    shouldRetry,
				AttemptIndex:      attempt + 1,
				UpstreamErrorKind: upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:      usageLogErrorMessage(resp.StatusCode, errBody),
			})
			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功：透传上游 chat/completions 响应。
		c.Set("x-account-email", account.Email)
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", logModel)
		c.Set("x-reasoning-effort", reasoningEffort)

		firstTokenMs, bytesOut, usage, streamErr := h.streamQoderResponse(c, resp, isStream, start)
		totalDuration := int(time.Since(start).Milliseconds())
		resp.Body.Close()

		if streamErr != nil && bytesOut == 0 && shouldRetryRequestError(streamErr, &generalRetries, maxRetries) {
			log.Printf("Qoder 流首包前断开，重试 (attempt %d/%d, account %d): %v", attempt+1, maxRetries+1, account.ID(), streamErr)
			recyclePooledClient(account, proxyURL)
			h.store.ReportRequestFailure(account, "stream_break", time.Duration(totalDuration)*time.Millisecond)
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			continue
		}

		h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		h.store.Release(account)

		input := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/chat/completions",
			Model:            logModel,
			EffectiveModel:   logEffectiveModel,
			StatusCode:       http.StatusOK,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/chat/completions",
			UpstreamEndpoint: "/chat/completions",
			Stream:           isStream,
			AttemptIndex:     attempt + 1,
		}
		if usage != nil {
			input.PromptTokens = usage.PromptTokens
			input.InputTokens = usage.PromptTokens
			input.CompletionTokens = usage.CompletionTokens
			input.OutputTokens = usage.CompletionTokens
			input.TotalTokens = usage.TotalTokens
			input.CachedTokens = usage.CachedTokens
			input.ReasoningTokens = usage.ReasoningTokens
		}
		h.logUsageForRequest(c, input)
		return
	}
}

// streamQoderResponse 把上游 chat/completions 响应透传给客户端。
// 流式：逐行转发 SSE 并解析 usage；非流式：整体转发并解析 usage。
// 返回首字延迟、已写出字节数、usage、错误。
func (h *Handler) streamQoderResponse(c *gin.Context, resp *http.Response, isStream bool, start time.Time) (firstTokenMs int, bytesOut int, usage *UsageInfo, err error) {
	if !isStream {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return 0, 0, nil, readErr
		}
		usage = extractUsageFromChatCompletion(body)
		c.Data(http.StatusOK, "application/json", body)
		return int(time.Since(start).Milliseconds()), len(body), usage, nil
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	flusher, _ := c.Writer.(http.Flusher)

	reader := bufio.NewReader(resp.Body)
	ttftRecorded := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !ttftRecorded {
				firstTokenMs = int(time.Since(start).Milliseconds())
				ttftRecorded = true
			}
			bytesOut += len(line)
			if _, werr := c.Writer.Write(line); werr != nil {
				return firstTokenMs, bytesOut, usage, werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			// 解析 usage（OpenAI 流末 data: {... "usage": {...}}）。
			if trimmed := strings.TrimSpace(string(line)); strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload != "" && payload != "[DONE]" {
					if u := extractUsageFromChatCompletion([]byte(payload)); u != nil {
						usage = u
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return firstTokenMs, bytesOut, usage, nil
			}
			return firstTokenMs, bytesOut, usage, readErr
		}
	}
}

// extractUsageFromChatCompletion 从 OpenAI chat/completion (流式或非流式) 提取 usage。
func extractUsageFromChatCompletion(body []byte) *UsageInfo {
	u := gjson.GetBytes(body, "usage")
	if !u.Exists() {
		return nil
	}
	prompt := int(u.Get("prompt_tokens").Int())
	completion := int(u.Get("completion_tokens").Int())
	total := int(u.Get("total_tokens").Int())
	if total == 0 {
		total = prompt + completion
	}
	return &UsageInfo{
		PromptTokens:     prompt,
		InputTokens:      prompt,
		CompletionTokens: completion,
		OutputTokens:     completion,
		TotalTokens:      total,
		CachedTokens:     int(u.Get("prompt_tokens_details.cached_tokens").Int()),
		ReasoningTokens:  int(u.Get("completion_tokens_details.reasoning_tokens").Int()),
	}
}

var _ = auth.UpstreamQoder
