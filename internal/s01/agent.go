package s01

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	defaultOpenAIURL = "https://api.openai.com/v1"
	maxOutputChars   = 50000
	maxTokensPerCall = 8000
	commandTimeout   = 120 * time.Second
)

type ToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIChatRequest struct {
	Model      string          `json:"model"`
	Messages   []openAIMessage `json:"messages"`
	Tools      []openAITool    `json:"tools,omitempty"`
	ToolChoice string          `json:"tool_choice,omitempty"`
	MaxTokens  int             `json:"max_tokens,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
}

type toolUseInput struct {
	Command string `json:"command"`
}

type Agent struct {
	client  *http.Client
	apiKey  string
	baseURL string
	modelID string
	workDir string
	system  string
	tools   []ToolDef

	history []openAIMessage
}

func NewAgent() (*Agent, error) {
	_ = LoadDotEnv(".env")

	modelID := strings.TrimSpace(os.Getenv("MODEL_ID"))
	if modelID == "" {
		return nil, errors.New("MODEL_ID is required")
	}

	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	}
	if apiKey == "" {
		return nil, errors.New("OPENAI_API_KEY is required (or DEEPSEEK_API_KEY for compatibility)")
	}

	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("DEEPSEEK_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = defaultOpenAIURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	workDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	system := fmt.Sprintf("You are a coding agent at %s. Use bash to solve tasks. Act, don't explain.", workDir)
	tools := []ToolDef{
		{
			Name:        "bash",
			Description: "Run a shell command.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
				},
				"required": []string{"command"},
			},
		},
	}

	return &Agent{
		client:  &http.Client{Timeout: 180 * time.Second},
		apiKey:  apiKey,
		baseURL: baseURL,
		modelID: modelID,
		workDir: workDir,
		system:  system,
		tools:   tools,
	}, nil
}

func (a *Agent) RunTurn(query string) error {
	if len(a.history) == 0 {
		a.history = append(a.history, openAIMessage{
			Role:    "system",
			Content: a.system,
		})
	}
	a.history = append(a.history, openAIMessage{
		Role:    "user",
		Content: query,
	})

	for {
		resp, err := a.createOpenAIChat(a.history)
		if err != nil {
			return err
		}
		if len(resp.Choices) == 0 {
			return errors.New("model response has no choices")
		}

		msg := resp.Choices[0].Message
		a.history = append(a.history, msg)

		if len(msg.ToolCalls) == 0 {
			text, _ := msg.Content.(string)
			if strings.TrimSpace(text) == "" {
				fmt.Println("(no text response)")
			} else {
				fmt.Println(text)
			}
			return nil
		}

		for _, tc := range msg.ToolCalls {
			output := "Error: unknown tool"

			if tc.Function.Name == "bash" {
				var input toolUseInput
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					output = "Error: invalid tool input"
				} else {
					fmt.Printf("$ %s\n", input.Command)
					output = runBash(a.workDir, input.Command)
					if output != "" {
						fmt.Println(outputPreview(output, 200))
					}
				}
			}

			a.history = append(a.history, openAIMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
			})
		}
	}
}

func (a *Agent) createOpenAIChat(messages []openAIMessage) (*openAIChatResponse, error) {
	tools := make([]openAITool, 0, len(a.tools))
	for _, t := range a.tools {
		tools = append(tools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	body := openAIChatRequest{
		Model:      a.modelID,
		Messages:   messages,
		Tools:      tools,
		ToolChoice: "auto",
		MaxTokens:  maxTokensPerCall,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := a.baseURL + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+a.apiKey)

	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("openai-compatible API %d: %s", res.StatusCode, string(raw))
	}

	var decoded openAIChatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return &decoded, nil
}

func runBash(workDir, command string) string {
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-Command", command)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-lc", command)
	}
	cmd.Dir = workDir

	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "Error: Timeout (120s)"
	}
	text := strings.TrimSpace(string(out))
	if err != nil && text == "" {
		text = err.Error()
	}
	if text == "" {
		text = "(no output)"
	}
	return truncate(text, maxOutputChars)
}

func outputPreview(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
