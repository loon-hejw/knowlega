package service

import "context"

type fakeEmbeddingProvider struct {
	embedding []float32
	called    int
	text      string
	texts     []string
}

func (p *fakeEmbeddingProvider) EmbedText(_ context.Context, text string) ([]float32, error) {
	p.called++
	p.text = text
	p.texts = append(p.texts, text)
	return p.embedding, nil
}
