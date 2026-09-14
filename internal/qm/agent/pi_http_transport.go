package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

const maxCapturedPiRequestChars = 2_000_000

type PiHTTPTransport struct {
	models config.ModelsConfig
	client *http.Client
}

type piProviderError struct {
	status    int
	body      string
	retryable bool
}

func (e *piProviderError) Error() string {
	return fmt.Sprintf("pi provider request failed: status=%d body=%s", e.status, e.body)
}

func (e *piProviderError) Permanent() bool { return !e.retryable }

func NewPiHTTPTransport(models config.ModelsConfig) (*PiHTTPTransport, error) {
	if _, ok := models.Harness("pi"); !ok {
		return nil, errors.New("pi harness is not configured")
	}
	timeout := time.Duration(models.Request.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	return &PiHTTPTransport{models: models, client: &http.Client{Timeout: timeout}}, nil
}

func (t *PiHTTPTransport) Close(context.Context) error {
	if t != nil && t.client != nil {
		t.client.CloseIdleConnections()
	}
	return nil
}

func (t *PiHTTPTransport) Complete(ctx context.Context, request PiCompletionRequest, onDelta func(string), onTextBlockStart func()) (PiCompletion, error) {
	provider, ok := t.models.ProviderForModel(request.Model)
	if !ok {
		return PiCompletion{}, &piProviderError{body: "model provider is not configured"}
	}
	var payload any
	var endpoint string
	switch provider.Protocol {
	case "openai":
		payload = openAIPiPayload(request)
		endpoint = "/chat/completions"
	case "anthropic":
		payload = anthropicPiPayload(request)
		endpoint = "/messages"
	default:
		return PiCompletion{}, &piProviderError{body: "unsupported protocol " + provider.Protocol}
	}
	modelDefinition := piModelDefinition(provider, request.Model)
	capPiModelOutput(payload, modelDefinition)
	guardPiOutputBudget(payload, modelDefinition, request)
	body, err := marshalPiPayloadWithinBudget(payload, provider.Protocol, 18_000_000)
	if err != nil {
		return PiCompletion{}, &piProviderError{body: err.Error()}
	}
	captured, truncated := capturePiRequest(body)
	started := time.Now()
	completion, err := t.completeWithRetry(ctx, provider, endpoint, body, request.FastMode, onDelta, onTextBlockStart)
	if err != nil {
		return PiCompletion{}, err
	}
	duration := int(time.Since(started).Milliseconds())
	completion.Request = captured
	completion.Truncated = truncated
	completion.DurationMS = &duration
	completion.Transport, _ = json.Marshal(map[string]any{"modelId": firstNonEmpty(completion.Model, request.Model)})
	return completion, nil
}

func marshalPiPayloadWithinBudget(payload any, protocol string, maximum int) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil || maximum <= 0 || len(body) <= maximum {
		return body, err
	}
	document, ok := payload.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("pi request exceeds %d bytes", maximum)
	}
	messages, ok := document["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("pi request exceeds %d bytes", maximum)
	}
	const elided = "[image removed: this conversation's images no longer fit the model's request-size limit; ask for it to be re-shared if needed]"
	for messageIndex := range messages {
		message, ok := messages[messageIndex].(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for contentIndex, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if !piInlineImage(block, protocol) {
				continue
			}
			content[contentIndex] = map[string]any{"type": "text", "text": elided}
			body, err = json.Marshal(document)
			if err != nil {
				return nil, err
			}
			if len(body) <= maximum-3_000_000 {
				return body, nil
			}
		}
	}
	if len(body) <= maximum {
		return body, nil
	}
	return nil, fmt.Errorf("pi request is %d bytes after inline images were elided, exceeding %d bytes", len(body), maximum)
}

func piInlineImage(block map[string]any, protocol string) bool {
	if protocol == "anthropic" && block["type"] == "image" {
		source, _ := block["source"].(map[string]any)
		data, _ := source["data"].(string)
		return len(data) > 0
	}
	if protocol == "openai" && block["type"] == "image_url" {
		var url string
		switch imageURL := block["image_url"].(type) {
		case map[string]any:
			url, _ = imageURL["url"].(string)
		case map[string]string:
			url = imageURL["url"]
		}
		return strings.HasPrefix(url, "data:image/")
	}
	return false
}

func piModelDefinition(provider config.ModelProviderConfig, modelID string) config.ModelDefinition {
	for _, model := range provider.Models {
		if model.ID == modelID {
			return model
		}
	}
	return config.ModelDefinition{ID: modelID}
}

func guardPiOutputBudget(payload any, model config.ModelDefinition, request PiCompletionRequest) {
	document, ok := payload.(map[string]any)
	if !ok || model.ContextWindow <= 0 {
		return
	}
	cap, ok := document["max_tokens"].(int)
	if !ok || cap >= 1024 {
		return
	}
	estimated := estimatePiTokens(request.SystemPrompt, request.Messages)
	for _, message := range request.Messages {
		estimated += len(message.Images) * 1200
	}
	available := model.ContextWindow - estimated - 4096
	if available < 1024 {
		return
	}
	ceiling := available
	if model.MaxTokens > 0 && model.MaxTokens < ceiling {
		ceiling = model.MaxTokens
	}
	if ceiling > cap {
		document["max_tokens"] = ceiling
	}
}

func capPiModelOutput(payload any, model config.ModelDefinition) {
	document, ok := payload.(map[string]any)
	if !ok || model.MaxTokens <= 0 {
		return
	}
	maximum, ok := document["max_tokens"].(int)
	if !ok || maximum <= model.MaxTokens {
		return
	}
	document["max_tokens"] = model.MaxTokens
	thinking, _ := document["thinking"].(map[string]any)
	budget, _ := thinking["budget_tokens"].(int)
	if budget >= model.MaxTokens {
		thinking["budget_tokens"] = max(0, model.MaxTokens-1024)
	}
}

func (t *PiHTTPTransport) completeWithRetry(ctx context.Context, provider config.ModelProviderConfig, endpoint string, body []byte, fast bool, onDelta func(string), onTextBlockStart func()) (PiCompletion, error) {
	attempts := t.models.Request.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		response, retryable, err := t.open(ctx, provider, endpoint, body, fast)
		if err == nil {
			defer response.Body.Close()
			contentType := strings.ToLower(response.Header.Get("Content-Type"))
			if strings.Contains(contentType, "text/event-stream") {
				if provider.Protocol == "openai" {
					return parseOpenAIPiStream(response.Body, onDelta, onTextBlockStart)
				}
				return parseAnthropicPiStream(response.Body, onDelta, onTextBlockStart)
			}
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<20))
			if readErr != nil {
				return PiCompletion{}, readErr
			}
			var completion PiCompletion
			if provider.Protocol == "openai" {
				completion, err = parseOpenAIPiResponse(data)
			} else {
				completion, err = parseAnthropicPiResponse(data)
			}
			if err != nil {
				return PiCompletion{}, &piProviderError{status: http.StatusOK, body: err.Error(), retryable: true}
			}
			emitPiText(completion.Text, onDelta, onTextBlockStart)
			return completion, nil
		}
		last = err
		if !retryable || attempt+1 == attempts {
			return PiCompletion{}, err
		}
		if err := waitPiRetry(ctx, attempt); err != nil {
			return PiCompletion{}, err
		}
	}
	return PiCompletion{}, last
}

func (t *PiHTTPTransport) open(ctx context.Context, provider config.ModelProviderConfig, endpoint string, body []byte, fast bool) (*http.Response, bool, error) {
	request, err := t.newRequest(ctx, provider, endpoint, body, fast)
	if err != nil {
		return nil, false, err
	}
	response, err := t.client.Do(request)
	if err != nil {
		return nil, ctx.Err() == nil, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, false, nil
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return nil, true, readErr
	}
	retryable := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	return nil, retryable, &piProviderError{status: response.StatusCode, body: piErrorMessage(data), retryable: retryable}
}

func (t *PiHTTPTransport) newRequest(ctx context.Context, provider config.ModelProviderConfig, endpoint string, body []byte, fast bool) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, piProtocolEndpoint(provider.BaseURL, endpoint), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+provider.APIKey)
	userAgent := strings.TrimSpace(provider.UserAgent)
	if userAgent == "" {
		userAgent = "knowledge-core/0.1"
	}
	request.Header.Set("User-Agent", userAgent)
	if provider.Protocol == "anthropic" {
		request.Header.Set("x-api-key", provider.APIKey)
		version := strings.TrimSpace(provider.AnthropicVersion)
		if version == "" {
			version = "2023-06-01"
		}
		request.Header.Set("anthropic-version", version)
		if fast {
			request.Header.Set("anthropic-beta", "fast-mode-2026-02-01")
		}
	}
	return request, nil
}

func waitPiRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(200*(1<<attempt)) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func openAIPiPayload(request PiCompletionRequest) map[string]any {
	messages := make([]any, 0, len(request.Messages)+1)
	if request.SystemPrompt != "" {
		messages = append(messages, map[string]any{"role": "system", "content": request.SystemPrompt})
	}
	for _, message := range request.Messages {
		entry := map[string]any{"role": message.Role}
		if message.Role == "tool" {
			entry["tool_call_id"] = message.ToolCallID
			entry["content"] = message.Content
		} else if len(message.Images) > 0 {
			content := []any{map[string]any{"type": "text", "text": message.Content}}
			for _, image := range message.Images {
				content = append(content, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + image.MIMEType + ";base64," + image.DataBase64}})
			}
			entry["content"] = content
		} else {
			entry["content"] = message.Content
		}
		if len(message.ToolCalls) > 0 {
			calls := make([]any, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				calls = append(calls, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": string(call.Arguments)}})
			}
			entry["tool_calls"] = calls
		}
		messages = append(messages, entry)
	}
	payload := map[string]any{"model": request.Model, "messages": messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}, "max_tokens": max(1, request.MaxOutputTokens)}
	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": rawJSONObject(tool.InputSchema)}})
		}
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	if request.DisableThinking {
		payload["enable_thinking"] = false
		payload["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	if effort := piEffort(request.ThinkingLevel); effort != "" {
		payload["reasoning_effort"] = effort
	} else if request.DisableThinking {
		payload["reasoning_effort"] = "low"
	}
	if request.FastMode {
		payload["speed"] = "fast"
	}
	return payload
}

func applyAnthropicPiThinking(payload map[string]any, request PiCompletionRequest) {
	level := strings.ToLower(strings.TrimSpace(request.ThinkingLevel))
	if request.DisableThinking || level == "off" {
		payload["thinking"] = map[string]string{"type": "disabled"}
		return
	}
	if level == "" || level == "auto" {
		return
	}
	if request.AdaptiveThinking {
		payload["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
		payload["output_config"] = map[string]string{"effort": anthropicPiEffort(level)}
		return
	}
	budget := anthropicPiBudget(level)
	maximum, _ := payload["max_tokens"].(int)
	maximum += budget
	if maximum <= budget {
		budget = max(0, maximum-1024)
	}
	payload["max_tokens"] = maximum
	payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget, "display": "summarized"}
}

func anthropicPiEffort(level string) string {
	switch level {
	case "minimal":
		return "low"
	case "ultracode":
		return "max"
	default:
		return level
	}
}

func anthropicPiBudget(level string) int {
	switch level {
	case "minimal":
		return 1024
	case "medium":
		return 8192
	case "high", "xhigh", "max", "ultracode":
		return 16384
	default:
		return 2048
	}
}

func anthropicPiPayload(request PiCompletionRequest) map[string]any {
	messages := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.Role == "tool" {
			messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": message.Content}}})
			continue
		}
		content := make([]any, 0, 1+len(message.Images)+len(message.ToolCalls))
		if message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": message.Content})
		}
		for _, image := range message.Images {
			content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": image.MIMEType, "data": image.DataBase64}})
		}
		for _, call := range message.ToolCalls {
			content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": rawJSONObject(call.Arguments)})
		}
		messages = append(messages, map[string]any{"role": message.Role, "content": content})
	}
	system, cacheSplit := anthropicPiSystem(request.SystemPrompt, request.SystemCacheBoundary)
	payload := map[string]any{"model": request.Model, "system": system, "messages": messages, "max_tokens": max(1, request.MaxOutputTokens), "stream": true}
	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			definition := map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": rawJSONObject(tool.InputSchema)}
			if cacheSplit {
				definition["cache_control"] = map[string]string{"type": "ephemeral", "ttl": "1h"}
			}
			tools = append(tools, definition)
		}
		payload["tools"] = tools
	}
	if request.FastMode {
		payload["speed"] = "fast"
	}
	applyAnthropicPiThinking(payload, request)
	return payload
}

func anthropicPiSystem(prompt string, boundary *int) (any, bool) {
	if boundary == nil || *boundary <= 0 {
		return prompt, false
	}
	stable, rest, ok := splitUTF16(prompt, *boundary)
	if !ok || strings.TrimSpace(stable) == "" || strings.TrimSpace(rest) == "" {
		return prompt, false
	}
	return []any{
		map[string]any{"type": "text", "text": stable, "cache_control": map[string]string{"type": "ephemeral", "ttl": "1h"}},
		map[string]any{"type": "text", "text": rest},
	}, true
}

func splitUTF16(value string, boundary int) (string, string, bool) {
	units := 0
	for index, current := range value {
		if units == boundary {
			return value[:index], value[index:], true
		}
		if current > 0xffff {
			units += 2
		} else {
			units++
		}
		if units > boundary {
			return "", "", false
		}
	}
	return "", "", false
}

func parseOpenAIPiResponse(data []byte) (PiCompletion, error) {
	var response struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name, Arguments string
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return PiCompletion{}, fmt.Errorf("decode OpenAI response: %w", err)
	}
	if len(response.Choices) == 0 {
		return PiCompletion{}, errors.New("OpenAI response contained no choices")
	}
	choice := response.Choices[0].Message
	completion := PiCompletion{StopReason: response.Choices[0].FinishReason, Text: openAIPiText(choice.Content), Thinking: choice.ReasoningContent, Model: response.Model, Usage: PiUsage{Input: response.Usage.PromptTokens, Output: response.Usage.CompletionTokens, CacheRead: response.Usage.PromptTokensDetails.CachedTokens}}
	for _, call := range choice.ToolCalls {
		arguments := json.RawMessage(call.Function.Arguments)
		arguments = piToolArguments(arguments)
		completion.ToolCalls = append(completion.ToolCalls, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
	}
	if completion.Text == "" && len(completion.ToolCalls) == 0 && completion.Thinking == "" {
		return PiCompletion{}, errors.New("OpenAI response contained no text or tool calls")
	}
	return completion, nil
}

func parseAnthropicPiResponse(data []byte) (PiCompletion, error) {
	var response struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type, Text, Thinking, ID, Name string
			Input                          json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return PiCompletion{}, fmt.Errorf("decode Anthropic response: %w", err)
	}
	var texts, thinking []string
	completion := PiCompletion{StopReason: response.StopReason, Model: response.Model, Usage: PiUsage{Input: response.Usage.InputTokens, Output: response.Usage.OutputTokens, CacheRead: response.Usage.CacheReadInputTokens, CacheWrite: response.Usage.CacheCreationInputTokens}}
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			texts = append(texts, block.Text)
		case "thinking":
			thinking = append(thinking, block.Thinking)
		case "tool_use":
			block.Input = piToolArguments(block.Input)
			completion.ToolCalls = append(completion.ToolCalls, ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Input})
		}
	}
	completion.Text = strings.TrimSpace(strings.Join(texts, "\n"))
	completion.Thinking = strings.TrimSpace(strings.Join(thinking, "\n"))
	if completion.Text == "" && len(completion.ToolCalls) == 0 && completion.Thinking == "" {
		return PiCompletion{}, errors.New("Anthropic response contained no text or tool calls")
	}
	return completion, nil
}

func parseOpenAIPiStream(reader io.Reader, onDelta func(string), onTextBlockStart func()) (PiCompletion, error) {
	type accumulatedCall struct {
		id, name, arguments string
	}
	completion := PiCompletion{}
	calls := map[int]*accumulatedCall{}
	order := []int{}
	textStarted := false
	streamStarted := time.Now()
	firstText := false
	err := scanPiSSE(reader, func(data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		var event struct {
			Model   string `json:"model"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content          json.RawMessage `json:"content"`
					ReasoningContent string          `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				PromptDetails    struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		if event.Model != "" {
			completion.Model = event.Model
		}
		if event.Usage.PromptTokens != 0 || event.Usage.CompletionTokens != 0 {
			completion.Usage = PiUsage{Input: event.Usage.PromptTokens, Output: event.Usage.CompletionTokens, CacheRead: event.Usage.PromptDetails.CachedTokens}
		}
		for _, choice := range event.Choices {
			if choice.FinishReason != "" {
				completion.StopReason = choice.FinishReason
			}
			delta := openAIPiDeltaText(choice.Delta.Content)
			if delta != "" {
				if !textStarted {
					textStarted = true
					if onTextBlockStart != nil {
						onTextBlockStart()
					}
				}
				if !firstText {
					firstText = true
					value := int(time.Since(streamStarted).Milliseconds())
					completion.TTFTMS = &value
				}
				completion.Text += delta
				if onDelta != nil {
					onDelta(delta)
				}
			}
			completion.Thinking += choice.Delta.ReasoningContent
			for _, partial := range choice.Delta.ToolCalls {
				call := calls[partial.Index]
				if call == nil {
					call = &accumulatedCall{}
					calls[partial.Index] = call
					order = append(order, partial.Index)
				}
				call.id += partial.ID
				call.name += partial.Function.Name
				call.arguments += partial.Function.Arguments
			}
		}
		return nil
	})
	if err != nil {
		return PiCompletion{}, fmt.Errorf("decode OpenAI stream: %w", err)
	}
	for _, index := range order {
		call := calls[index]
		arguments := json.RawMessage(call.arguments)
		arguments = piToolArguments(arguments)
		completion.ToolCalls = append(completion.ToolCalls, ToolCall{ID: call.id, Name: call.name, Arguments: arguments})
	}
	if completion.Text == "" && len(completion.ToolCalls) == 0 && completion.Thinking == "" {
		return PiCompletion{}, errors.New("OpenAI stream contained no text or tool calls")
	}
	return completion, nil
}

func parseAnthropicPiStream(reader io.Reader, onDelta func(string), onTextBlockStart func()) (PiCompletion, error) {
	type blockState struct {
		typeName, id, name, text, input string
	}
	completion := PiCompletion{}
	blocks := map[int]*blockState{}
	order := []int{}
	streamStarted := time.Now()
	firstText := false
	err := scanPiSSE(reader, func(data []byte) error {
		var event struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type, ID, Name, Text, Thinking string
				Input                          json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		switch event.Type {
		case "message_start":
			completion.Model = event.Message.Model
			completion.Usage.Input = event.Message.Usage.InputTokens
			completion.Usage.Output = event.Message.Usage.OutputTokens
			completion.Usage.CacheRead = event.Message.Usage.CacheReadInputTokens
			completion.Usage.CacheWrite = event.Message.Usage.CacheCreationInputTokens
		case "content_block_start":
			block := &blockState{typeName: event.ContentBlock.Type, id: event.ContentBlock.ID, name: event.ContentBlock.Name, text: firstNonEmpty(event.ContentBlock.Text, event.ContentBlock.Thinking)}
			if len(event.ContentBlock.Input) > 0 && string(event.ContentBlock.Input) != "{}" {
				block.input = string(event.ContentBlock.Input)
			}
			blocks[event.Index] = block
			order = append(order, event.Index)
			if block.typeName == "text" && onTextBlockStart != nil {
				onTextBlockStart()
			}
		case "content_block_delta":
			block := blocks[event.Index]
			if block == nil {
				block = &blockState{}
				blocks[event.Index] = block
				order = append(order, event.Index)
			}
			switch event.Delta.Type {
			case "text_delta":
				block.typeName = "text"
				block.text += event.Delta.Text
				completion.Text += event.Delta.Text
				if !firstText {
					firstText = true
					value := int(time.Since(streamStarted).Milliseconds())
					completion.TTFTMS = &value
				}
				if onDelta != nil {
					onDelta(event.Delta.Text)
				}
			case "thinking_delta":
				block.typeName = "thinking"
				block.text += event.Delta.Thinking
				completion.Thinking += event.Delta.Thinking
			case "input_json_delta":
				block.input += event.Delta.PartialJSON
			}
		case "message_delta":
			completion.StopReason = event.Delta.StopReason
			if event.Usage.OutputTokens != 0 {
				completion.Usage.Output = event.Usage.OutputTokens
			}
		}
		return nil
	})
	if err != nil {
		return PiCompletion{}, fmt.Errorf("decode Anthropic stream: %w", err)
	}
	for _, index := range order {
		block := blocks[index]
		if block.typeName != "tool_use" {
			continue
		}
		arguments := json.RawMessage(block.input)
		arguments = piToolArguments(arguments)
		completion.ToolCalls = append(completion.ToolCalls, ToolCall{ID: block.id, Name: block.name, Arguments: arguments})
	}
	if completion.Text == "" && len(completion.ToolCalls) == 0 && completion.Thinking == "" {
		return PiCompletion{}, errors.New("Anthropic stream contained no text or tool calls")
	}
	return completion, nil
}

func scanPiSSE(reader io.Reader, consume func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := bytes.TrimSpace([]byte(strings.TrimPrefix(line, "data:")))
		if len(data) == 0 {
			continue
		}
		if err := consume(data); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func emitPiText(text string, onDelta func(string), onTextBlockStart func()) {
	if text == "" {
		return
	}
	if onTextBlockStart != nil {
		onTextBlockStart()
	}
	if onDelta != nil {
		onDelta(text)
	}
}

func openAIPiText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var blocks []struct {
		Type, Text string
	}
	if json.Unmarshal(raw, &blocks) == nil {
		parts := []string{}
		for _, block := range blocks {
			if block.Type == "text" || block.Type == "output_text" {
				parts = append(parts, block.Text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

func openAIPiDeltaText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return openAIPiText(raw)
}

func capturePiRequest(body []byte) (json.RawMessage, bool) {
	redacted := redactPiImageData(body)
	if len(redacted) <= maxCapturedPiRequestChars {
		return json.RawMessage(redacted), false
	}
	preview := string(redacted[:maxCapturedPiRequestChars])
	captured, _ := json.Marshal(map[string]any{"truncated": true, "bytes": len(redacted), "preview": preview})
	return captured, true
}

func redactPiImageData(body []byte) []byte {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return []byte(`{"note":"payload not capturable"}`)
	}
	redactPiValue(value)
	redacted, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"note":"payload not serializable"}`)
	}
	return redacted
}

func redactPiValue(value any) {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			redactPiValue(item)
		}
	case map[string]any:
		for key, item := range current {
			if key == "data" {
				if text, ok := item.(string); ok && len(text) > 256 {
					current[key] = "<image " + strconv.Itoa(len(text)) + " chars omitted>"
					continue
				}
			}
			if key == "url" {
				if text, ok := item.(string); ok && strings.HasPrefix(text, "data:image/") {
					current[key] = "<image " + strconv.Itoa(len(text)) + " chars omitted>"
					continue
				}
			}
			redactPiValue(item)
		}
	}
}

func piProtocolEndpoint(baseURL, endpoint string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, endpoint) {
		return baseURL
	}
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + endpoint
	}
	if endpoint == "/messages" {
		return baseURL + "/v1" + endpoint
	}
	return baseURL + endpoint
}

func piErrorMessage(data []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &parsed) == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	text := strings.TrimSpace(string(data))
	if len(text) > 1000 {
		text = text[:1000]
	}
	return text
}

func piEffort(level string) string {
	switch strings.TrimSpace(strings.ToLower(level)) {
	case "", "auto":
		return ""
	case "ultracode":
		return "max"
	default:
		return strings.TrimSpace(strings.ToLower(level))
	}
}

// Preserve malformed JSON as a JSON string so it can be recorded and returned
// to the model without corrupting the transcript. Object validation rejects it.
func piToolArguments(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	if json.Valid(raw) {
		return raw
	}
	encoded, _ := json.Marshal(string(raw))
	return encoded
}
