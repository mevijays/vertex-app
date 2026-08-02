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
	model  string
}

var (
	_ chat.Generator        = (*Generator)(nil)
	_ chat.DocumentAnalyzer = (*Generator)(nil)
)

func New(ctx context.Context, projectID, location, modelID string) (*Generator, error) {
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
	return &Generator{client: client, model: modelID}, nil
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

// AnalyzeDocument sends inline document bytes (PDF or text) to Gemini and
// returns an extracted-text/summary suitable for storing as reusable chat
// context. Uses g.model rather than a per-request model, since document
// analysis always runs against the app's configured model.
func (g *Generator) AnalyzeDocument(ctx context.Context, data []byte, mimeType, filename string) (string, error) {
	prompt := fmt.Sprintf(
		"Extract and summarize the key information from this document (%s) in detail, "+
			"preserving facts, figures, and structure so the summary can answer follow-up questions about it.",
		filename,
	)
	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
		genai.NewPartFromBytes(data, mimeType),
	}
	contents := []*genai.Content{genai.NewContentFromParts(parts, genai.RoleUser)}

	response, err := g.client.Models.GenerateContent(ctx, g.model, contents, &genai.GenerateContentConfig{})
	if err != nil {
		return "", fmt.Errorf("analyze document: %w", err)
	}
	text := response.Text()
	if text == "" {
		return "", fmt.Errorf("analyze document: empty response from model")
	}
	return text, nil
}
