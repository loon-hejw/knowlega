package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/config"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/promptbudget"
)

type OpenAICompatibleEmbeddingProvider struct {
	BaseURL       string
	APIKey        string
	Model         string
	Client        *http.Client
	MaxInputChars int
}

const defaultEmbeddingMaxInputChars = 6000

func (p OpenAICompatibleEmbeddingProvider) EmbeddingModel() string {
	return p.Model
}

func NewEmbeddingProvider(cfg config.EmbeddingConfig) (EmbeddingProvider, bool, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, false, nil
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, false, fmt.Errorf("embedding.api_key is required when embedding.model is configured")
	}
	return OpenAICompatibleEmbeddingProvider{
		BaseURL:       strings.TrimRight(cfg.BaseURL, "/"),
		APIKey:        cfg.APIKey,
		Model:         cfg.Model,
		Client:        &http.Client{Timeout: cfg.Timeout.Duration},
		MaxInputChars: cfg.MaxInputChars,
	}, true, nil
}

func (p OpenAICompatibleEmbeddingProvider) EmbedText(ctx context.Context, text string) ([]float32, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	maxInputChars := p.MaxInputChars
	if maxInputChars <= 0 {
		maxInputChars = defaultEmbeddingMaxInputChars
	}
	text = promptbudget.TrimMiddle(text, maxInputChars)
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	body, err := json.Marshal(embeddingRequest{
		Model: p.Model,
		Input: text,
	})
	if err != nil {
		return nil, err
	}
	embedding, status, err := p.doEmbeddingRequest(ctx, client, p.BaseURL+"/embeddings", body)
	if status == http.StatusNotFound && shouldRetryEmbeddingV1(p.BaseURL) {
		embedding, _, err = p.doEmbeddingRequest(ctx, client, p.BaseURL+"/v1/embeddings", body)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(embedding) == 0 {
		return nil, fmt.Errorf("embedding response returned no data")
	}
	return embedding, nil
}

func (p OpenAICompatibleEmbeddingProvider) doEmbeddingRequest(ctx context.Context, client *http.Client, url string, body []byte) ([]float32, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	var parsed embeddingResponse
	if len(respBody) > 0 {
		_ = json.Unmarshal(respBody, &parsed)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("embedding request failed: url=%s status=%d body=%s", url, resp.StatusCode, embeddingErrorMessage(parsed, respBody))
	}
	if len(parsed.Data) == 0 {
		return nil, resp.StatusCode, nil
	}
	return parsed.Data[0].Embedding, resp.StatusCode, nil
}

func shouldRetryEmbeddingV1(baseURL string) bool {
	return !strings.HasSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

func embeddingErrorMessage(parsed embeddingResponse, body []byte) string {
	if strings.TrimSpace(parsed.Error.Message) != "" {
		return parsed.Error.Message
	}
	return strings.TrimSpace(string(body))
}

type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}
