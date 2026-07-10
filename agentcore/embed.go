package agentcore

import (
	"context"
	"math"
)

// Embedder turns text into dense vectors for semantic memory recall (§14.7).
// It is the embedding analogue of LLMProvider: a narrow, product-agnostic seam
// the consumer backs with a real vendor (OpenAIEmbedder) or a test fake. A nil
// Embedder means the consumer falls back to keyword recall — embeddings are an
// additive relevance upgrade, never a hard dependency.
type Embedder interface {
	// Embed returns one vector per input string, index-aligned. All returned
	// vectors share the same dimension.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Cosine is the cosine similarity of two equal-length vectors, in [-1, 1].
// Mismatched lengths or a zero-magnitude vector yield 0 (no signal) rather than
// NaN, so ranking degrades gracefully instead of panicking.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// FauxEmbedder is a deterministic, keyless Embedder for tests: each input maps
// to a fixed vector supplied by Vectors (by exact text), else the Default
// vector. It lets the recall/ranking path be tested with no network or keys,
// mirroring FauxProvider.
type FauxEmbedder struct {
	Vectors map[string][]float32
	Default []float32
}

// Embed returns the scripted vector for each text.
func (f *FauxEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := f.Vectors[t]; ok {
			out[i] = v
			continue
		}
		out[i] = f.Default
	}
	return out, nil
}
