package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.openai.com/v1"

type Config struct {
	ModelID string
	APIKey  string
	BaseURL string
}

func ConfigFromEnv() (Config, error) {
	modelID := strings.TrimSpace(os.Getenv("MODEL_ID"))
	if modelID == "" {
		return Config{}, errors.New("MODEL_ID is required")
	}

	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	}
	if apiKey == "" {
		return Config{}, errors.New("OPENAI_API_KEY is required (or DEEPSEEK_API_KEY for compatibility)")
	}

	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("DEEPSEEK_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	return Config{
		ModelID: modelID,
		APIKey:  apiKey,
		BaseURL: baseURL,
	}, nil
}

type ToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

type Client struct {
	httpClient *http.Client
	apiKey     string
	baseURL    string
}

func NewClient(apiKey, baseURL string, timeout time.Duration) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: timeout},
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
	}
}

func (c *Client) ChatCompletions(
	ctx context.Context,
	model string,
	messages []Message,
	tools []ToolDef,
	maxTokens int,
) (*ChatResponse, error) {
	type requestTool struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  any    `json:"parameters"`
		} `json:"function"`
	}
	type requestBody struct {
		Model      string        `json:"model"`
		Messages   []Message     `json:"messages"`
		Tools      []requestTool `json:"tools,omitempty"`
		ToolChoice string        `json:"tool_choice,omitempty"`
		MaxTokens  int           `json:"max_tokens,omitempty"`
	}

	reqTools := make([]requestTool, 0, len(tools))
	for _, t := range tools {
		rt := requestTool{Type: "function"}
		rt.Function.Name = t.Name
		rt.Function.Description = t.Description
		rt.Function.Parameters = t.InputSchema
		reqTools = append(reqTools, rt)
	}

	body := requestBody{
		Model:      model,
		Messages:   messages,
		Tools:      reqTools,
		ToolChoice: "auto",
		MaxTokens:  maxTokens,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.apiKey)

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("openai-compatible API %d: %s", res.StatusCode, string(raw))
	}

	var decoded ChatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return &decoded, nil
}
