package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
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
	contextCache sync.Map // model -> context window tokens
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
	streamCallback := optionAsStreamCallback(options)
	streaming := streamCallback != nil || optionAsBool(options, "stream")
	if streaming {
		requestBody["stream"] = true
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
		return nil, WrapTransportError(p.providerName, fmt.Errorf("send %s request: %w", p.providerName, err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(resp.Body)
		msg := augmentProviderError(p.providerName, extractAPIError(body))
		retryAfter := ParseRetryAfterHeader(resp.Header.Get("Retry-After"))
		return nil, NewHTTPError(p.providerName, resp.StatusCode, msg, retryAfter)
	}

	if streaming {
		result, err := parseChatCompletionsStreamResponse(resp.Body, streamCallback)
		if err != nil {
			return nil, fmt.Errorf("parse %s stream response: %w", p.providerName, err)
		}
		return result, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", p.providerName, err)
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

func (p *chatCompletionsProvider) ResolveContextWindow(ctx context.Context, model string) (int, error) {
	if p == nil {
		return 0, fmt.Errorf("provider not initialized")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = p.defaultModel
	}
	if model == "" {
		return 0, fmt.Errorf("model is required")
	}
	if v := knownModelContextWindow(model); v > 0 {
		return v, nil
	}
	if cached, ok := p.contextCache.Load(strings.ToLower(model)); ok {
		if n, ok := cached.(int); ok && n > 0 {
			return n, nil
		}
	}
	// OpenRouter exposes context lengths via /models; OpenAI chat-completions
	// generally does not expose this in model metadata.
	if p.providerName != ProviderOpenRouter {
		return 0, fmt.Errorf("provider context metadata unavailable")
	}

	timeout := 4 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if rem := time.Until(deadline); rem < timeout {
			timeout = rem
		}
	}
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.apiBase+"/models", nil)
	if err != nil {
		return 0, err
	}
	if err := p.auth.Apply(reqCtx, req); err != nil {
		return 0, err
	}
	for name, value := range p.extraHeaders {
		req.Header.Set(name, value)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return 0, NewHTTPError(p.providerName, resp.StatusCode, extractAPIError(body), ParseRetryAfterHeader(resp.Header.Get("Retry-After")))
	}

	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, err
	}
	best := 0
	target := strings.ToLower(strings.TrimSpace(model))
	for _, entry := range payload.Data {
		entryID := strings.ToLower(strings.TrimSpace(entry.ID))
		if entryID == "" || entry.ContextLength <= 0 {
			continue
		}
		if entryID == target || strings.HasSuffix(target, "/"+entryID) || strings.HasSuffix(entryID, "/"+target) {
			best = entry.ContextLength
			break
		}
	}
	if best <= 0 {
		return 0, fmt.Errorf("context window metadata not found for model %s", model)
	}
	p.contextCache.Store(target, best)
	return best, nil
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

func optionAsBool(opts map[string]interface{}, key string) bool {
	if len(opts) == 0 {
		return false
	}
	v, ok := opts[key]
	if !ok || v == nil {
		return false
	}
	switch vv := v.(type) {
	case bool:
		return vv
	case string:
		return strings.EqualFold(strings.TrimSpace(vv), "true")
	default:
		return false
	}
}

func optionAsStreamCallback(opts map[string]interface{}) func(string) {
	if len(opts) == 0 {
		return nil
	}
	v, ok := opts["stream_callback"]
	if !ok || v == nil {
		return nil
	}
	cb, _ := v.(func(string))
	return cb
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

func parseChatCompletionsStreamResponse(r io.Reader, onDelta func(string)) (*LLMResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	type toolAccumulator struct {
		ID   string
		Type string
		Name string
		Args strings.Builder
	}

	accumulators := map[int]*toolAccumulator{}
	var (
		content      strings.Builder
		finishReason string
		usage        *UsageInfo
	)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function *struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *UsageInfo `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if delta := choice.Delta.Content; delta != "" {
				content.WriteString(delta)
				if onDelta != nil {
					onDelta(delta)
				}
			}
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			for _, tc := range choice.Delta.ToolCalls {
				acc := accumulators[tc.Index]
				if acc == nil {
					acc = &toolAccumulator{}
					accumulators[tc.Index] = acc
				}
				if id := strings.TrimSpace(tc.ID); id != "" {
					acc.ID = id
				}
				if typ := strings.TrimSpace(tc.Type); typ != "" {
					acc.Type = typ
				}
				if tc.Function != nil {
					if name := strings.TrimSpace(tc.Function.Name); name != "" {
						acc.Name = name
					}
					if args := tc.Function.Arguments; args != "" {
						acc.Args.WriteString(args)
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	indexes := make([]int, 0, len(accumulators))
	for idx := range accumulators {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)

	toolCalls := make([]ToolCall, 0, len(indexes))
	for _, idx := range indexes {
		acc := accumulators[idx]
		if acc == nil {
			continue
		}
		argsRaw := strings.TrimSpace(acc.Args.String())
		args := map[string]interface{}{}
		if argsRaw != "" {
			if err := json.Unmarshal([]byte(argsRaw), &args); err != nil {
				args["raw"] = argsRaw
			}
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        acc.ID,
			Type:      valueOr(acc.Type, "function"),
			Name:      acc.Name,
			Arguments: args,
		})
	}

	if finishReason == "" {
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}
	return &LLMResponse{
		Content:      strings.TrimSpace(content.String()),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
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

func valueOr(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
