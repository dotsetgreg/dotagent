package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	openAPIMethods = map[string]struct{}{
		"get": {}, "post": {}, "put": {}, "patch": {}, "delete": {}, "head": {}, "options": {},
	}
	openAPISlugRegex = regexp.MustCompile(`[^a-z0-9_]+`)
	openAPISpecCache sync.Map
)

type OpenAPIConfig struct {
	SpecPath         string            `json:"spec_path,omitempty"`
	SpecURL          string            `json:"spec_url,omitempty"`
	BaseURL          string            `json:"base_url,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	TimeoutSeconds   int               `json:"timeout_seconds,omitempty"`
	MaxConcurrency   int               `json:"max_concurrency,omitempty"`
	RetryMaxAttempts int               `json:"retry_max_attempts,omitempty"`
	RetryBackoffMS   int               `json:"retry_backoff_ms,omitempty"`
	AuthHeader       string            `json:"auth_header,omitempty"`
	AuthToken        string            `json:"auth_token,omitempty"`
}

type openAPIOperation struct {
	ID               string
	Method           string
	Path             string
	Description      string
	Parameters       []openAPIParameter
	RequestBody      map[string]interface{}
	RequestBodyNeeds bool
}

type openAPIParameter struct {
	Name        string
	In          string
	Description string
	Required    bool
	Schema      map[string]interface{}
}

type openAPICompiledSpec struct {
	raw        map[string]interface{}
	baseURL    string
	operations map[string]openAPIOperation
}

type OpenAPIRuntime struct {
	id        string
	cfg       OpenAPIConfig
	timeout   time.Duration
	retry     RetryPolicy
	semaphore chan struct{}
	client    *http.Client
	headers   map[string]string

	mu       sync.RWMutex
	compiled *openAPICompiledSpec
}

func NewOpenAPIRuntime(id string, cfg OpenAPIConfig) (*OpenAPIRuntime, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("openapi runtime id is required")
	}
	cfg = normalizeOpenAPIConfig(cfg)
	if strings.TrimSpace(cfg.SpecPath) == "" && strings.TrimSpace(cfg.SpecURL) == "" {
		return nil, fmt.Errorf("openapi connector requires spec_path or spec_url")
	}
	timeout := 30 * time.Second
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	maxConcurrency := cfg.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}
	retry := RetryPolicy{
		MaxAttempts: cfg.RetryMaxAttempts,
		Backoff:     time.Duration(cfg.RetryBackoffMS) * time.Millisecond,
	}
	if retry.Backoff <= 0 {
		retry.Backoff = 250 * time.Millisecond
	}
	headers := ResolveStringMap(cfg.Headers)
	if headers == nil {
		headers = map[string]string{}
	}
	authHeader := strings.TrimSpace(cfg.AuthHeader)
	authToken := ResolveSecretRef(cfg.AuthToken)
	if authHeader != "" && authToken != "" {
		headers[authHeader] = authToken
	}
	return &OpenAPIRuntime{
		id:        id,
		cfg:       cfg,
		timeout:   timeout,
		retry:     retry,
		semaphore: make(chan struct{}, maxConcurrency),
		client:    &http.Client{Timeout: timeout},
		headers:   headers,
	}, nil
}

func (r *OpenAPIRuntime) ID() string {
	return r.id
}

func (r *OpenAPIRuntime) Type() string {
	return "openapi"
}

func (r *OpenAPIRuntime) Health(ctx context.Context) error {
	compiled, err := r.ensureCompiled(ctx)
	if err != nil {
		return err
	}
	if len(compiled.operations) == 0 {
		return fmt.Errorf("openapi spec has no operations")
	}
	return nil
}

func (r *OpenAPIRuntime) ToolSchema(ctx context.Context, target string) (string, map[string]interface{}, error) {
	compiled, err := r.ensureCompiled(ctx)
	if err != nil {
		return "", nil, err
	}
	op, ok := compiled.operations[strings.TrimSpace(target)]
	if !ok {
		return "", nil, fmt.Errorf("openapi operation %q not found", target)
	}
	properties := map[string]interface{}{}
	required := make([]string, 0)
	for _, p := range op.Parameters {
		schema := compactJSONSchema(p.Schema)
		if strings.TrimSpace(p.Description) != "" {
			schema = cloneMap(schema)
			schema["description"] = strings.TrimSpace(p.Description)
		}
		properties[p.Name] = schema
		if p.Required {
			required = appendIfMissing(required, p.Name)
		}
	}
	if len(op.RequestBody) > 0 {
		bodySchema := compactJSONSchema(op.RequestBody)
		if strings.TrimSpace(op.Description) != "" {
			bodySchema = cloneMap(bodySchema)
		}
		properties["body"] = bodySchema
		if op.RequestBodyNeeds {
			required = appendIfMissing(required, "body")
		}
	}
	schema := map[string]interface{}{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	desc := strings.TrimSpace(op.Description)
	if desc == "" {
		desc = fmt.Sprintf("%s %s", strings.ToUpper(op.Method), op.Path)
	}
	return desc, schema, nil
}

func (r *OpenAPIRuntime) Invoke(ctx context.Context, target string, args map[string]interface{}) (InvocationResult, error) {
	if err := r.acquire(ctx); err != nil {
		return InvocationResult{}, err
	}
	defer r.release()

	compiled, err := r.ensureCompiled(ctx)
	if err != nil {
		return InvocationResult{}, err
	}
	op, ok := compiled.operations[strings.TrimSpace(target)]
	if !ok {
		return InvocationResult{}, fmt.Errorf("openapi operation %q not found", target)
	}

	var out InvocationResult
	invokeErr := withRetry(ctx, r.retry, func(attempt int) error {
		callCtx, cancel := context.WithTimeout(ctx, r.timeout)
		defer cancel()
		req, reqErr := r.buildRequest(callCtx, compiled.baseURL, op, args)
		if reqErr != nil {
			return reqErr
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		if readErr != nil {
			return readErr
		}
		bodyStr := strings.TrimSpace(string(body))
		if bodyStr == "" {
			bodyStr = "(empty response body)"
		}
		if resp.StatusCode >= 400 {
			out = InvocationResult{
				Content: fmt.Sprintf("OpenAPI request failed (%d): %s", resp.StatusCode, bodyStr),
				IsError: true,
			}
			return nil
		}
		out = InvocationResult{
			Content: fmt.Sprintf("OpenAPI response (%d): %s", resp.StatusCode, bodyStr),
		}
		return nil
	})
	if invokeErr != nil {
		return InvocationResult{}, invokeErr
	}
	return out, nil
}

func (r *OpenAPIRuntime) Close() error {
	// Stateless HTTP runtime; no active resources.
	return nil
}

func (r *OpenAPIRuntime) ensureCompiled(ctx context.Context) (*openAPICompiledSpec, error) {
	r.mu.RLock()
	if r.compiled != nil {
		compiled := r.compiled
		r.mu.RUnlock()
		return compiled, nil
	}
	r.mu.RUnlock()

	raw, err := r.loadSpecBytes(ctx)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	cacheKey := hex.EncodeToString(sum[:]) + "|" + strings.TrimSpace(r.cfg.BaseURL)
	if cached, ok := openAPISpecCache.Load(cacheKey); ok {
		if compiled, ok := cached.(*openAPICompiledSpec); ok {
			r.mu.Lock()
			r.compiled = compiled
			r.mu.Unlock()
			return compiled, nil
		}
	}

	compiled, err := compileOpenAPISpec(raw, strings.TrimSpace(r.cfg.BaseURL))
	if err != nil {
		return nil, err
	}
	openAPISpecCache.Store(cacheKey, compiled)
	r.mu.Lock()
	r.compiled = compiled
	r.mu.Unlock()
	return compiled, nil
}

func (r *OpenAPIRuntime) loadSpecBytes(ctx context.Context) ([]byte, error) {
	if path := strings.TrimSpace(r.cfg.SpecPath); path != "" {
		return os.ReadFile(path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(r.cfg.SpecURL), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range r.headers {
		if strings.TrimSpace(v) == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openapi spec download failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
}

func compileOpenAPISpec(raw []byte, configuredBaseURL string) (*openAPICompiledSpec, error) {
	spec := map[string]interface{}{}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse openapi spec (JSON expected): %w", err)
	}
	pathsRaw, _ := spec["paths"].(map[string]interface{})
	if len(pathsRaw) == 0 {
		return nil, fmt.Errorf("openapi spec contains no paths")
	}
	baseURL := strings.TrimSpace(configuredBaseURL)
	if baseURL == "" {
		if servers, ok := spec["servers"].([]interface{}); ok && len(servers) > 0 {
			if first, ok := servers[0].(map[string]interface{}); ok {
				if serverURL, ok := first["url"].(string); ok {
					baseURL = strings.TrimSpace(serverURL)
				}
			}
		}
	}
	if baseURL == "" {
		return nil, fmt.Errorf("openapi base url is required (set connector.openapi.base_url or spec servers[0].url)")
	}
	if err := validateAbsoluteHTTPURL(baseURL); err != nil {
		return nil, fmt.Errorf("invalid openapi base url %q: %w", baseURL, err)
	}
	operations := map[string]openAPIOperation{}
	for rawPath, pathVal := range pathsRaw {
		pathObj, ok := pathVal.(map[string]interface{})
		if !ok {
			continue
		}
		pathParameters := parseOpenAPIParameters(spec, toInterfaceSlice(pathObj["parameters"]))
		for method, opVal := range pathObj {
			methodLower := strings.ToLower(strings.TrimSpace(method))
			if _, ok := openAPIMethods[methodLower]; !ok {
				continue
			}
			opObj, ok := opVal.(map[string]interface{})
			if !ok {
				continue
			}
			opID, _ := opObj["operationId"].(string)
			opID = strings.TrimSpace(opID)
			if opID == "" {
				opID = fallbackOperationID(methodLower, rawPath)
			}
			summary, _ := opObj["summary"].(string)
			description, _ := opObj["description"].(string)
			desc := strings.TrimSpace(summary)
			if desc == "" {
				desc = strings.TrimSpace(description)
			}
			opParameters := parseOpenAPIParameters(spec, toInterfaceSlice(opObj["parameters"]))
			parameters := mergeOpenAPIParameters(pathParameters, opParameters)
			bodySchema, bodyRequired := parseOpenAPIRequestBody(spec, opObj["requestBody"])
			operations[opID] = openAPIOperation{
				ID:               opID,
				Method:           methodLower,
				Path:             rawPath,
				Description:      desc,
				Parameters:       parameters,
				RequestBody:      bodySchema,
				RequestBodyNeeds: bodyRequired,
			}
		}
	}
	if len(operations) == 0 {
		return nil, fmt.Errorf("openapi spec has no executable operations")
	}
	return &openAPICompiledSpec{
		raw:        spec,
		baseURL:    baseURL,
		operations: operations,
	}, nil
}

func parseOpenAPIParameters(spec map[string]interface{}, rawParams []interface{}) []openAPIParameter {
	out := make([]openAPIParameter, 0, len(rawParams))
	for _, raw := range rawParams {
		resolved := resolveOpenAPIRef(spec, raw, 0)
		param, ok := resolved.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := param["name"].(string)
		in, _ := param["in"].(string)
		if strings.TrimSpace(name) == "" || strings.TrimSpace(in) == "" {
			continue
		}
		description, _ := param["description"].(string)
		required, _ := param["required"].(bool)
		schema, _ := resolveOpenAPIRef(spec, param["schema"], 0).(map[string]interface{})
		out = append(out, openAPIParameter{
			Name:        strings.TrimSpace(name),
			In:          strings.TrimSpace(strings.ToLower(in)),
			Description: strings.TrimSpace(description),
			Required:    required || strings.EqualFold(strings.TrimSpace(in), "path"),
			Schema:      compactJSONSchema(schema),
		})
	}
	return dedupeOpenAPIParameters(out)
}

func parseOpenAPIRequestBody(spec map[string]interface{}, raw interface{}) (map[string]interface{}, bool) {
	resolved := resolveOpenAPIRef(spec, raw, 0)
	bodyObj, ok := resolved.(map[string]interface{})
	if !ok {
		return nil, false
	}
	required, _ := bodyObj["required"].(bool)
	content, _ := bodyObj["content"].(map[string]interface{})
	if len(content) == 0 {
		return nil, required
	}
	var media map[string]interface{}
	if jsonMedia, ok := content["application/json"].(map[string]interface{}); ok {
		media = jsonMedia
	} else {
		for _, anyMedia := range content {
			if m, ok := anyMedia.(map[string]interface{}); ok {
				media = m
				break
			}
		}
	}
	if len(media) == 0 {
		return nil, required
	}
	schema, _ := resolveOpenAPIRef(spec, media["schema"], 0).(map[string]interface{})
	return compactJSONSchema(schema), required
}

func resolveOpenAPIRef(spec map[string]interface{}, raw interface{}, depth int) interface{} {
	if depth > 12 || raw == nil {
		return raw
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return raw
	}
	refRaw, hasRef := obj["$ref"]
	if !hasRef {
		return raw
	}
	ref, _ := refRaw.(string)
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "#/") {
		return raw
	}
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	cur := interface{}(spec)
	for _, part := range parts {
		node, ok := cur.(map[string]interface{})
		if !ok {
			return raw
		}
		cur, ok = node[part]
		if !ok {
			return raw
		}
	}
	return resolveOpenAPIRef(spec, cur, depth+1)
}

func (r *OpenAPIRuntime) buildRequest(ctx context.Context, baseURL string, op openAPIOperation, args map[string]interface{}) (*http.Request, error) {
	parsedBase, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid openapi base_url %q: %w", baseURL, err)
	}
	path := op.Path
	query := url.Values{}
	headers := map[string]string{}
	for k, v := range r.headers {
		headers[k] = v
	}
	for _, p := range op.Parameters {
		rawValue, ok := args[p.Name]
		if !ok {
			if p.Required {
				return nil, fmt.Errorf("missing required parameter: %s", p.Name)
			}
			continue
		}
		value := fmt.Sprintf("%v", rawValue)
		switch p.In {
		case "path":
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(value))
		case "query":
			query.Set(p.Name, value)
		case "header":
			headers[p.Name] = value
		case "cookie":
			// Keep cookie values as header fallback for simplicity.
			headers["Cookie"] = appendCookie(headers["Cookie"], p.Name, value)
		}
	}
	parsedBase.Path = strings.TrimRight(parsedBase.Path, "/") + "/" + strings.TrimLeft(path, "/")
	parsedBase.RawQuery = query.Encode()

	var bodyReader io.Reader
	if op.RequestBodyNeeds || op.RequestBody != nil {
		rawBody, hasBody := args["body"]
		if op.RequestBodyNeeds && !hasBody {
			return nil, fmt.Errorf("missing required request body")
		}
		if hasBody {
			bodyJSON, err := json.Marshal(rawBody)
			if err != nil {
				return nil, fmt.Errorf("marshal request body: %w", err)
			}
			bodyReader = strings.NewReader(string(bodyJSON))
			if _, exists := headers["Content-Type"]; !exists {
				headers["Content-Type"] = "application/json"
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(op.Method), parsedBase.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		if strings.TrimSpace(v) == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

func dedupeOpenAPIParameters(params []openAPIParameter) []openAPIParameter {
	out := make([]openAPIParameter, 0, len(params))
	seen := map[string]int{}
	for _, p := range params {
		key := strings.ToLower(strings.TrimSpace(p.In)) + ":" + strings.ToLower(strings.TrimSpace(p.Name))
		if idx, ok := seen[key]; ok {
			out[idx] = p
			continue
		}
		seen[key] = len(out)
		out = append(out, p)
	}
	return out
}

func mergeOpenAPIParameters(a, b []openAPIParameter) []openAPIParameter {
	out := make([]openAPIParameter, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return dedupeOpenAPIParameters(out)
}

func toInterfaceSlice(raw interface{}) []interface{} {
	if raw == nil {
		return nil
	}
	out, _ := raw.([]interface{})
	return out
}

func fallbackOperationID(method, path string) string {
	slug := strings.ToLower(strings.TrimSpace(method + "_" + path))
	slug = strings.ReplaceAll(slug, "{", "")
	slug = strings.ReplaceAll(slug, "}", "")
	slug = openAPISlugRegex.ReplaceAllString(slug, "_")
	slug = strings.Trim(slug, "_")
	if slug == "" {
		return "operation"
	}
	return slug
}

func appendIfMissing(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func cloneMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func appendCookie(existing, key, value string) string {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" {
		return existing
	}
	entry := key + "=" + value
	if strings.TrimSpace(existing) == "" {
		return entry
	}
	return existing + "; " + entry
}

func validateAbsoluteHTTPURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return fmt.Errorf("host is required")
	}
	return nil
}

func (r *OpenAPIRuntime) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r.semaphore <- struct{}{}:
		return nil
	}
}

func (r *OpenAPIRuntime) release() {
	select {
	case <-r.semaphore:
	default:
	}
}

func normalizeOpenAPIConfig(cfg OpenAPIConfig) OpenAPIConfig {
	cfg.SpecPath = strings.TrimSpace(ResolveSecretRef(cfg.SpecPath))
	cfg.SpecURL = strings.TrimSpace(ResolveSecretRef(cfg.SpecURL))
	cfg.BaseURL = strings.TrimSpace(ResolveSecretRef(cfg.BaseURL))
	cfg.AuthHeader = strings.TrimSpace(cfg.AuthHeader)
	cfg.AuthToken = strings.TrimSpace(cfg.AuthToken)
	return cfg
}
