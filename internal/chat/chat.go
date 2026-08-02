package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Messages    []Message
	System      string
	Model       string
	Temperature *float32
	TopP        *float32
	MaxTokens   int32
	Stop        []string
}

type Usage struct {
	PromptTokens     int32 `json:"prompt_tokens"`
	CompletionTokens int32 `json:"completion_tokens"`
	TotalTokens      int32 `json:"total_tokens"`
}

type Result struct {
	Usage Usage
}

type Generator interface {
	Stream(ctx context.Context, request Request, onText func(string) error) (Result, error)
}

type MockGenerator struct{}

func (MockGenerator) Stream(ctx context.Context, request Request, onText func(string) error) (Result, error) {
	last := "your message"
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == "user" {
			last = request.Messages[i].Content
			break
		}
	}
	words := strings.Fields(last)
	if len(words) > 14 {
		words = words[:14]
	}
	response := fmt.Sprintf("Local preview is working. I received: **%s**\n\nConnect Application Default Credentials to stream a live response from `%s`.", strings.Join(words, " "), request.Model)

	for _, token := range strings.SplitAfter(response, " ") {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(12 * time.Millisecond):
		}
		if err := onText(token); err != nil {
			return Result{}, err
		}
	}

	return Result{Usage: Usage{
		PromptTokens:     int32(len(strings.Fields(last))),
		CompletionTokens: int32(len(strings.Fields(response))),
		TotalTokens:      int32(len(strings.Fields(last)) + len(strings.Fields(response))),
	}}, nil
}

var ErrUnsupportedRole = errors.New("unsupported message role")

// DocumentAnalyzer is implemented separately from Generator (rather than
// folded into it) so MockGenerator stays untouched by document-analysis
// concerns; vertex.Generator implements both.
type DocumentAnalyzer interface {
	AnalyzeDocument(ctx context.Context, data []byte, mimeType, filename string) (string, error)
}

type MockDocumentAnalyzer struct{}

func (MockDocumentAnalyzer) AnalyzeDocument(ctx context.Context, data []byte, mimeType, filename string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(12 * time.Millisecond):
	}
	return fmt.Sprintf("[MOCK ANALYSIS] %s (%s, %d bytes): local preview extraction — connect Application Default Credentials for a real document analysis.", filename, mimeType, len(data)), nil
}
