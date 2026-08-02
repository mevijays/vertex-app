package vertex

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"vertex-chat/internal/chat"
)

type Generator struct {
	client *genai.Client
}

func New(ctx context.Context, projectID, location string) (*Generator, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  projectID,
		Location: location,
		Backend:  genai.BackendVertexAI,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: "v1",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create Vertex AI client: %w", err)
	}
	return &Generator{client: client}, nil
}

func (g *Generator) Stream(ctx context.Context, request chat.Request, onText func(string) error) (chat.Result, error) {
	contents := make([]*genai.Content, 0, len(request.Messages))
	for _, message := range request.Messages {
		var role genai.Role
		switch message.Role {
		case "user":
			role = genai.RoleUser
		case "assistant":
			role = genai.RoleModel
		default:
			return chat.Result{}, fmt.Errorf("%w: %s", chat.ErrUnsupportedRole, message.Role)
		}
		contents = append(contents, genai.NewContentFromText(message.Content, role))
	}

	config := &genai.GenerateContentConfig{}
	if strings.TrimSpace(request.System) != "" {
		config.SystemInstruction = genai.NewContentFromText(request.System, genai.RoleUser)
	}
	config.Temperature = request.Temperature
	config.TopP = request.TopP
	if request.MaxTokens > 0 {
		config.MaxOutputTokens = request.MaxTokens
	}
	if len(request.Stop) > 0 {
		config.StopSequences = request.Stop
	}

	var usage chat.Usage
	for response, err := range g.client.Models.GenerateContentStream(ctx, request.Model, contents, config) {
		if err != nil {
			return chat.Result{}, fmt.Errorf("stream Gemini response: %w", err)
		}
		if response.UsageMetadata != nil {
			usage = chat.Usage{
				PromptTokens:     response.UsageMetadata.PromptTokenCount,
				CompletionTokens: response.UsageMetadata.CandidatesTokenCount,
				TotalTokens:      response.UsageMetadata.TotalTokenCount,
			}
		}
		if text := response.Text(); text != "" {
			if err := onText(text); err != nil {
				return chat.Result{}, err
			}
		}
	}

	return chat.Result{Usage: usage}, nil
}
