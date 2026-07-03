package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/config"
)

type OpenAICompatibleEmbeddingProvider struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func (p OpenAICompatibleEmbeddingProvider) EmbeddingModel() string {
	return p.Model
}

func NewEnvEmbeddingProvider() (EmbeddingProvider, bool, error) {
	apiKey := config.Value("KB_CORE_EMBEDDING_API_KEY", "OPENAI_API_KEY", "KB_CORE_LLM_API_KEY")
	model := config.Value("KB_CORE_EMBEDDING_MODEL", "OPENAI_EMBEDDING_MODEL")
	if apiKey == "" || model == "" {
		return nil, false, nil
	}
	baseURL := config.Value("KB_CORE_EMBEDDING_BASE_URL", "OPENAI_BASE_URL", "KB_CORE_LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return OpenAICompatibleEmbeddingProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Client:  &http.Client{Timeout: 60 * time.Second},
	}, true, nil
}

func (p OpenAICompatibleEmbeddingProvider) EmbedText(ctx context.Context, text string) ([]float32, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var parsed embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding request failed: status=%d body=%s", resp.StatusCode, parsed.Error.Message)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("embedding response returned no data")
	}
	return parsed.Data[0].Embedding, nil
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
