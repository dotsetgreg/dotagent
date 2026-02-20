package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultHTTPTimeout = 300 * time.Second

type chatCompletionsProvider struct {
	providerName string
	apiBase      string
	defaultModel string
	auth         AuthStrategy
	httpClient   *http.Client
	extraHeaders map[string]string
}

func newChatCompletionsProvider(providerName, apiBase, defaultModel, proxy string, auth AuthStrategy, extraHeaders map[string]string) (*chatCompletionsProvider, error) {
	providerName = strings.TrimSpace(strings.ToLower(providerName))
	if providerName == "" {
		return nil, fmt.Errorf("provider name is required")
	}
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if apiBase == "" {
		return nil, fmt.Errorf("%s API base not configured", providerName)
	}
	if auth == nil {
		return nil, fmt.Errorf("%s auth is not configured", providerName)
	}

	client := &http.Client{Timeout: defaultHTTPTimeout}
	proxy = strings.TrimSpace(proxy)
	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("parse %s proxy: %w", providerName, err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}

	cleanHeaders := map[string]string{}
	for k, v := range extraHeaders {
		name := strings.TrimSpace(k)
		value := strings.TrimSpace(v)
		if name == "" || value == "" {
			continue
		}
		cleanHeaders[name] = value
	}

	return &chatCompletionsProvider{
		providerName: providerName,
		apiBase:      apiBase,
		defaultModel: strings.TrimSpace(defaultModel),
		auth:         auth,
		httpClient:   client,
		extraHeaders: cleanHeaders,
	}, nil
}

func (p *chatCompletionsProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string, options map[string]interface{}) (*LLMResponse, error) {
	if p == nil {
		return nil, fmt.Errorf("provider not initialized")
	}

	model = strings.TrimSpace(model)
	if model == "" {
		model = p.GetDefaultModel()
	}

	requestBody := map[string]interface{}{
		"model":    model,
		"messages": messages,
	}

	if len(tools) > 0 {
		requestBody["tools"] = tools
		requestBody["tool_choice"] = "auto"
	}

	if maxTokens, ok := optionAsInt(options, "max_tokens"); ok {
		requestBody["max_tokens"] = maxTokens
	}
	if temperature, ok := optionAsFloat(options, "temperature"); ok {
		requestBody["temperature"] = temperature
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", p.providerName, err)
	}

	endpoint := p.apiBase + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", p.providerName, err)
	}

	req.Header.Set("Content-Type", "application/json")
	if err := p.auth.Apply(ctx, req); err != nil {
		return nil, fmt.Errorf("apply %s auth: %w", p.providerName, err)
	}
	for name, value := range p.extraHeaders {
		req.Header.Set(name, value)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send %s request: %w", p.providerName, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", p.providerName, err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		msg := augmentProviderError(p.providerName, extractAPIError(body))
		return nil, fmt.Errorf("%s API request failed: status=%d error=%s", p.providerName, resp.StatusCode, msg)
	}

	result, err := parseChatCompletionsResponse(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s response: %w", p.providerName, err)
	}
	return result, nil
}

func (p *chatCompletionsProvider) GetDefaultModel() string {
	if p == nil {
		return ""
	}
	return p.defaultModel
}

func optionAsInt(opts map[string]interface{}, key string) (int, bool) {
	if len(opts) == 0 {
		return 0, false
	}
	v, ok := opts[key]
	if !ok || v == nil {
		return 0, false
	}
	switch vv := v.(type) {
	case int:
		return vv, true
	case int32:
		return int(vv), true
	case int64:
		return int(vv), true
	case float32:
		return int(vv), true
	case float64:
		return int(vv), true
	default:
		return 0, false
	}
}

func optionAsFloat(opts map[string]interface{}, key string) (float64, bool) {
	if len(opts) == 0 {
		return 0, false
	}
	v, ok := opts[key]
	if !ok || v == nil {
		return 0, false
	}
	switch vv := v.(type) {
	case float64:
		return vv, true
	case float32:
		return float64(vv), true
	case int:
		return float64(vv), true
	case int64:
		return float64(vv), true
	default:
		return 0, false
	}
}

func parseChatCompletionsResponse(body []byte) (*LLMResponse, error) {
	var apiResponse struct {
		Choices []struct {
			Message struct {
				Content   interface{} `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function *struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *UsageInfo `json:"usage"`
	}

	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return nil, err
	}

	if len(apiResponse.Choices) == 0 {
		return &LLMResponse{Content: "", FinishReason: "stop"}, nil
	}

	choice := apiResponse.Choices[0]
	toolCalls := make([]ToolCall, 0, len(choice.Message.ToolCalls))
	for _, tc := range choice.Message.ToolCalls {
		if tc.Function == nil {
			continue
		}

		arguments := map[string]interface{}{}
		if strings.TrimSpace(tc.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &arguments); err != nil {
				arguments["raw"] = tc.Function.Arguments
			}
		}

		toolCalls = append(toolCalls, ToolCall{
			ID:        tc.ID,
			Type:      tc.Type,
			Name:      tc.Function.Name,
			Arguments: arguments,
		})
	}

	return &LLMResponse{
		Content:      flattenMessageContent(choice.Message.Content),
		ToolCalls:    toolCalls,
		FinishReason: choice.FinishReason,
		Usage:        apiResponse.Usage,
	}, nil
}

func flattenMessageContent(raw interface{}) string {
	switch v := raw.(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if text, ok := m["text"].(string); ok {
				parts = append(parts, text)
				continue
			}
			if content, ok := m["content"].(string); ok {
				parts = append(parts, content)
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

func extractAPIError(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "empty response body"
	}

	var payload struct {
		Error struct {
			Message string      `json:"message"`
			Type    string      `json:"type"`
			Code    interface{} `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if msg := strings.TrimSpace(payload.Error.Message); msg != "" {
			return msg
		}
		if msg := strings.TrimSpace(payload.Message); msg != "" {
			return msg
		}
	}

	if len(trimmed) > 2000 {
		return trimmed[:2000] + "..."
	}
	return trimmed
}
