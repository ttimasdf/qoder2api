package proxy

import "strings"

// UsageInfo 描述一次请求的 token 用量（从上游 chat/completion 的 usage 字段解析）。
// 原先位于已删除的 translator.go；qoder2api 仅保留计费埋点所需字段。
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
}

// canonicalizeCodexModel 把请求模型名规范化为受支持列表中的精确名称。
// qoder2api 仅做大小写无关匹配；未命中时原样返回。
// （函数名保留是为了不改动 model_mapping.go / reasoning_effort_models.go 的调用点。）
func canonicalizeCodexModel(model string, supportedModels []string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}
	for _, supported := range supportedModels {
		if strings.EqualFold(trimmed, supported) {
			return supported
		}
	}
	return trimmed
}
