package core

import (
	"context"
)

type Provider interface {
	Invoke(ctx context.Context, messages []Message, tools []Tool) (Message, error)
}

type StreamProvider interface {
	Provider
	InvokeStream(ctx context.Context, messages []Message, tools []Tool) (<-chan StreamChunk, error)
}
