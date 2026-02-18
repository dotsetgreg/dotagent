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
}

func newResponsesProvider(providerName, apiBase, defaultModel, proxy string, auth AuthStrategy, extraHeaders map[string]string) (*responsesProvider, error) {
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

	return &responsesProvider{
		providerName: providerName,
		apiBase:      apiBase,
		defaultModel: strings.TrimSpace(defaultModel),
		auth:         auth,
		httpClient:   client,
		extraHeaders: cleanHeaders,
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

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, "", fmt.Errorf("marshal %s request: %w", p.providerName, err)
	}

	endpoint := p.apiBase + "/responses"
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

	parsed, err := parseResponsesResponse(body)
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
