package core

const (
	// MaxSourceBytes bounds in-memory extraction and LLM compilation. Raw import
	// itself is streamed, but oversized sources must be split or preprocessed.
	MaxSourceBytes int64 = 64 << 20
	// MaxUploadBytes is the aggregate multipart request limit.
	MaxUploadBytes int64 = 128 << 20
)
