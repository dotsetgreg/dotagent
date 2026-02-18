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
)

type responsesProvider struct {
	providerName string
	apiBase      string
	defaultModel string
	auth         AuthStrategy
	httpClient   *http.Client
	extraHeaders map[string]string
	options      responsesProviderOptions
}

func newResponsesProvider(providerName, apiBase, defaultModel, proxy string, auth AuthStrategy, extraHeaders map[string]string) (*responsesProvider, error) {
	return newResponsesProviderWithOptions(providerName, apiBase, defaultModel, proxy, auth, extraHeaders, nil)
}

type responsesProviderOptions struct {
	// buildEndpoint resolves the final request URL from configured apiBase.
	buildEndpoint func(apiBase string) string
	// beforeMarshal can inject provider-specific request-body fields.
	beforeMarshal func(body map[string]interface{})
	// beforeSend can inject provider-specific headers after auth is applied.
	beforeSend func(req *http.Request) error
}

func newResponsesProviderWithOptions(
	providerName,
	apiBase,
	defaultModel,
	proxy string,
	auth AuthStrategy,
	extraHeaders map[string]string,
	options *responsesProviderOptions,
) (*responsesProvider, error) {
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

	effectiveOptions := responsesProviderOptions{
		buildEndpoint: defaultResponsesEndpoint,
	}
	if options != nil {
		if options.buildEndpoint != nil {
			effectiveOptions.buildEndpoint = options.buildEndpoint
		}
		effectiveOptions.beforeMarshal = options.beforeMarshal
		effectiveOptions.beforeSend = options.beforeSend
	}

	return &responsesProvider{
		providerName: providerName,
		apiBase:      apiBase,
		defaultModel: strings.TrimSpace(defaultModel),
		auth:         auth,
		httpClient:   client,
		extraHeaders: cleanHeaders,
		options:      effectiveOptions,
	}, nil
}

func (p *responsesProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string, options map[string]interface{}) (*LLMResponse, error) {
	resp, _, err := p.ChatWithState(ctx, "", messages, tools, model, options)
	return resp, err
}

func (p *responsesProvider) ChatWithState(ctx context.Context, stateID string, messages []Message, tools []ToolDefinition, model string, options map[string]interface{}) (*LLMResponse, string, error) {
	if p == nil {
		return nil, "", fmt.Errorf("provider not initialized")
	}

	model = strings.TrimSpace(model)
	if model == "" {
		model = p.GetDefaultModel()
	}

	requestBody := map[string]interface{}{
		"model": model,
		"input": buildResponsesInput(messages),
	}
	if toolDefs := toResponsesTools(tools); len(toolDefs) > 0 {
		requestBody["tools"] = toolDefs
	}
	if prev := strings.TrimSpace(stateID); prev != "" {
		requestBody["previous_response_id"] = prev
	}
	if maxTokens, ok := optionAsInt(options, "max_tokens"); ok {
		requestBody["max_output_tokens"] = maxTokens
	}
	if temperature, ok := optionAsFloat(options, "temperature"); ok {
		requestBody["temperature"] = temperature
	}
	if p.options.beforeMarshal != nil {
		p.options.beforeMarshal(requestBody)
	}
	streaming := responsesBoolField(requestBody["stream"])

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, "", fmt.Errorf("marshal %s request: %w", p.providerName, err)
	}

	endpoint := strings.TrimSpace(p.options.buildEndpoint(p.apiBase))
	if endpoint == "" {
		return nil, "", fmt.Errorf("%s endpoint is empty", p.providerName)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonData))
	if err != nil {
		return nil, "", fmt.Errorf("create %s request: %w", p.providerName, err)
	}

	req.Header.Set("Content-Type", "application/json")
	if err := p.auth.Apply(ctx, req); err != nil {
		return nil, "", fmt.Errorf("apply %s auth: %w", p.providerName, err)
	}
	for name, value := range p.extraHeaders {
		req.Header.Set(name, value)
	}
	if p.options.beforeSend != nil {
		if err := p.options.beforeSend(req); err != nil {
			return nil, "", fmt.Errorf("prepare %s request: %w", p.providerName, err)
		}
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("send %s request: %w", p.providerName, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read %s response: %w", p.providerName, err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		msg := augmentProviderError(p.providerName, extractAPIError(body))
		return nil, "", fmt.Errorf("%s API request failed: status=%d error=%s", p.providerName, resp.StatusCode, msg)
	}

	var parsed *parsedResponsesResult
	if streaming {
		parsed, err = parseResponsesStreamBody(body)
	} else {
		parsed, err = parseResponsesResponse(body)
	}
	if err != nil {
		return nil, "", fmt.Errorf("parse %s response: %w", p.providerName, err)
	}
	return parsed.Response, parsed.ResponseID, nil
}

func (p *responsesProvider) GetDefaultModel() string {
	if p == nil {
		return ""
	}
	return p.defaultModel
}

func defaultResponsesEndpoint(apiBase string) string {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		return ""
	}
	return base + "/responses"
}

func responsesBoolField(value interface{}) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

type parsedResponsesResult struct {
	Response   *LLMResponse
	ResponseID string
}

func parseResponsesResponse(body []byte) (*parsedResponsesResult, error) {
	var apiResponse struct {
		ID         string      `json:"id"`
		Status     string      `json:"status"`
		OutputText interface{} `json:"output_text"`
		Output     []struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			Role      string `json:"role"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Text string `json:"text"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return nil, err
	}

	toolCalls := make([]ToolCall, 0)
	contentParts := make([]string, 0)
	if top := flattenResponsesOutputText(apiResponse.OutputText); top != "" {
		contentParts = append(contentParts, top)
	}

	for _, item := range apiResponse.Output {
		switch strings.TrimSpace(strings.ToLower(item.Type)) {
		case "function_call":
			args := map[string]interface{}{}
			rawArgs := strings.TrimSpace(item.Arguments)
			if rawArgs != "" {
				if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
					args["raw"] = rawArgs
				}
			}
			callID := strings.TrimSpace(item.CallID)
			if callID == "" {
				callID = strings.TrimSpace(item.ID)
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        callID,
				Type:      "function",
				Name:      strings.TrimSpace(item.Name),
				Arguments: args,
			})
		case "message":
			for _, part := range item.Content {
				if txt := strings.TrimSpace(part.Text); txt != "" {
					contentParts = append(contentParts, txt)
				}
			}
			if txt := strings.TrimSpace(item.Text); txt != "" {
				contentParts = append(contentParts, txt)
			}
		case "output_text", "text":
			if txt := strings.TrimSpace(item.Text); txt != "" {
				contentParts = append(contentParts, txt)
			}
		}
	}

	usage := (*UsageInfo)(nil)
	if apiResponse.Usage != nil {
		usage = &UsageInfo{
			PromptTokens:     apiResponse.Usage.InputTokens,
			CompletionTokens: apiResponse.Usage.OutputTokens,
			TotalTokens:      apiResponse.Usage.TotalTokens,
		}
	}

	finishReason := strings.TrimSpace(apiResponse.Status)
	if finishReason == "" {
		finishReason = "completed"
	}
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	return &parsedResponsesResult{
		ResponseID: strings.TrimSpace(apiResponse.ID),
		Response: &LLMResponse{
			Content:      strings.TrimSpace(strings.Join(contentParts, "\n")),
			ToolCalls:    toolCalls,
			FinishReason: finishReason,
			Usage:        usage,
		},
	}, nil
}

func parseResponsesStreamBody(body []byte) (*parsedResponsesResult, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, fmt.Errorf("empty streaming response body")
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return parseResponsesResponse([]byte(trimmed))
	}

	normalized := strings.ReplaceAll(trimmed, "\r\n", "\n")
	chunks := strings.Split(normalized, "\n\n")
	var content strings.Builder

	for _, chunk := range chunks {
		data := extractSSEDataChunk(chunk)
		if data == "" || data == "[DONE]" {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType := strings.TrimSpace(responsesAsString(event["type"]))
		switch eventType {
		case "error":
			return nil, fmt.Errorf("%s", extractSSEErrorMessage(event))
		case "response.failed":
			if msg := extractResponsesErrorMessage(event); msg != "" {
				return nil, fmt.Errorf("%s", msg)
			}
			return nil, fmt.Errorf("response.failed")
		case "response.done", "response.completed":
			raw, ok := event["response"].(map[string]interface{})
			if !ok {
				continue
			}
			payload, err := json.Marshal(raw)
			if err != nil {
				return nil, err
			}
			return parseResponsesResponse(payload)
		case "response.output_text.delta", "response.refusal.delta":
			if delta := responsesAsString(event["delta"]); delta != "" {
				content.WriteString(delta)
			}
		case "response.output_text.done":
			text := strings.TrimSpace(responsesAsString(event["text"]))
			if text != "" {
				if content.Len() > 0 {
					content.WriteString("\n")
				}
				content.WriteString(text)
			}
		}
	}

	if fallback := strings.TrimSpace(content.String()); fallback != "" {
		return &parsedResponsesResult{
			Response: &LLMResponse{
				Content:      fallback,
				FinishReason: "completed",
			},
		}, nil
	}

	return nil, fmt.Errorf("streaming responses payload missing completion event")
}

func extractSSEDataChunk(chunk string) string {
	lines := strings.Split(chunk, "\n")
	dataLines := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
	}
	return strings.TrimSpace(strings.Join(dataLines, "\n"))
}

func extractSSEErrorMessage(event map[string]interface{}) string {
	if msg := strings.TrimSpace(responsesAsString(event["message"])); msg != "" {
		return msg
	}
	if rawErr, ok := event["error"].(map[string]interface{}); ok {
		if msg := strings.TrimSpace(responsesAsString(rawErr["message"])); msg != "" {
			return msg
		}
	}
	return "stream error"
}

func extractResponsesErrorMessage(event map[string]interface{}) string {
	rawResp, ok := event["response"].(map[string]interface{})
	if !ok {
		return ""
	}
	rawErr, ok := rawResp["error"].(map[string]interface{})
	if !ok {
		return ""
	}
	return strings.TrimSpace(responsesAsString(rawErr["message"]))
}

func responsesAsString(value interface{}) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func buildResponsesInput(messages []Message) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(messages))
	for _, msg := range messages {
		role := strings.TrimSpace(strings.ToLower(msg.Role))
		switch role {
		case "system", "user", "assistant", "developer", "tool":
		default:
			role = "user"
		}
		switch role {
		case "tool":
			callID := strings.TrimSpace(msg.ToolCallID)
			if callID == "" {
				continue
			}
			out = append(out, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  msg.Content,
			})
			continue
		default:
			content := strings.TrimSpace(msg.Content)
			if content != "" {
				out = append(out, map[string]interface{}{
					"role": role,
					"content": []map[string]interface{}{
						{
							"type": "input_text",
							"text": content,
						},
					},
				})
			}
		}

		if len(msg.ToolCalls) == 0 {
			continue
		}
		for _, tc := range msg.ToolCalls {
			name := strings.TrimSpace(tc.Name)
			callID := strings.TrimSpace(tc.ID)
			if tc.Function != nil {
				if name == "" {
					name = strings.TrimSpace(tc.Function.Name)
				}
			}
			if name == "" || callID == "" {
				continue
			}
			rawArgs := ""
			if tc.Function != nil {
				rawArgs = strings.TrimSpace(tc.Function.Arguments)
			}
			if rawArgs == "" && tc.Arguments != nil {
				enc, _ := json.Marshal(tc.Arguments)
				rawArgs = string(enc)
			}
			if rawArgs == "" {
				rawArgs = "{}"
			}
			out = append(out, map[string]interface{}{
				"type":      "function_call",
				"call_id":   callID,
				"name":      name,
				"arguments": rawArgs,
			})
		}
	}
	return out
}

func toResponsesTools(defs []ToolDefinition) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(defs))
	for _, def := range defs {
		if strings.TrimSpace(strings.ToLower(def.Type)) != "function" {
			continue
		}
		name := strings.TrimSpace(def.Function.Name)
		if name == "" {
			continue
		}
		item := map[string]interface{}{
			"type": "function",
			"name": name,
		}
		if desc := strings.TrimSpace(def.Function.Description); desc != "" {
			item["description"] = desc
		}
		if def.Function.Parameters != nil {
			item["parameters"] = def.Function.Parameters
		}
		out = append(out, item)
	}
	return out
}

func flattenResponsesOutputText(raw interface{}) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch tv := item.(type) {
			case string:
				if s := strings.TrimSpace(tv); s != "" {
					parts = append(parts, s)
				}
			case map[string]interface{}:
				if text, ok := tv["text"].(string); ok {
					if s := strings.TrimSpace(text); s != "" {
						parts = append(parts, s)
					}
				}
				if text, ok := tv["content"].(string); ok {
					if s := strings.TrimSpace(text); s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}
