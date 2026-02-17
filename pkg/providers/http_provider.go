// DotAgent - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 DotAgent contributors

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

	"github.com/dotsetgreg/dotagent/pkg/config"
)

const (
	defaultOpenRouterAPIBase = "https://openrouter.ai/api/v1"
	defaultOpenRouterModel   = "openai/gpt-5.2"
)

type HTTPProvider struct {
	apiKey     string
	apiBase    string
	httpClient *http.Client
}

func NewHTTPProvider(apiKey, apiBase, proxy string) *HTTPProvider {
	client := &http.Client{Timeout: 120 * time.Second}

	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err == nil {
			client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		}
	}

	return &HTTPProvider{
		apiKey:     apiKey,
		apiBase:    strings.TrimRight(apiBase, "/"),
		httpClient: client,
	}
}

func (p *HTTPProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string, options map[string]interface{}) (*LLMResponse, error) {
	if p.apiBase == "" {
		return nil, fmt.Errorf("OpenRouter API base not configured")
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

	if maxTokens, ok := options["max_tokens"].(int); ok {
		requestBody["max_tokens"] = maxTokens
	}

	if temperature, ok := options["temperature"].(float64); ok {
		requestBody["temperature"] = temperature
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/chat/completions", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenRouter API request failed:\n  Status: %d\n  Body:   %s", resp.StatusCode, string(body))
	}

	return p.parseResponse(body)
}

func (p *HTTPProvider) parseResponse(body []byte) (*LLMResponse, error) {
	var apiResponse struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
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
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
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

		arguments := make(map[string]interface{})
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &arguments); err != nil {
				arguments["raw"] = tc.Function.Arguments
			}
		}

		toolCalls = append(toolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: arguments,
		})
	}

	return &LLMResponse{
		Content:      choice.Message.Content,
		ToolCalls:    toolCalls,
		FinishReason: choice.FinishReason,
		Usage:        apiResponse.Usage,
	}, nil
}

func (p *HTTPProvider) GetDefaultModel() string {
	return defaultOpenRouterModel
}

func CreateProvider(cfg *config.Config) (LLMProvider, error) {
	providerName := strings.ToLower(strings.TrimSpace(cfg.Agents.Defaults.Provider))
	if providerName != "" && providerName != "openrouter" {
		return nil, fmt.Errorf("unsupported provider %q: DotAgent only supports openrouter", providerName)
	}

	apiKey := strings.TrimSpace(cfg.Providers.OpenRouter.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("OpenRouter API key is required (set providers.openrouter.api_key or DOTAGENT_PROVIDERS_OPENROUTER_API_KEY)")
	}

	apiBase := strings.TrimSpace(cfg.Providers.OpenRouter.APIBase)
	if apiBase == "" {
		apiBase = defaultOpenRouterAPIBase
	}

	return NewHTTPProvider(apiKey, apiBase, strings.TrimSpace(cfg.Providers.OpenRouter.Proxy)), nil
}
