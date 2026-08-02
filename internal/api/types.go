package api

import "vertex-chat/internal/chat"

type ChatCompletionRequest struct {
	Model       string         `json:"model"`
	Messages    []chat.Message `json:"messages"`
	Stream      bool           `json:"stream,omitempty"`
	Temperature *float32       `json:"temperature,omitempty"`
	TopP        *float32       `json:"top_p,omitempty"`
	MaxTokens   int32          `json:"max_tokens,omitempty"`
	Stop        StringList     `json:"stop,omitempty"`
	N           int            `json:"n,omitempty"`
}

type StringList []string

func (s *StringList) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var value string
		if err := unmarshalJSON(data, &value); err != nil {
			return err
		}
		*s = []string{value}
		return nil
	}
	var values []string
	if err := unmarshalJSON(data, &values); err != nil {
		return err
	}
	*s = values
	return nil
}

type chatCompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *chat.Usage        `json:"usage,omitempty"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      *assistantMessage `json:"message,omitempty"`
	Delta        *streamDelta      `json:"delta,omitempty"`
	FinishReason *string           `json:"finish_reason"`
}

type assistantMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}
