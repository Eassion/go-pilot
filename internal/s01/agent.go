package s01

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"go-pilot/internal/shared/envutil"
	"go-pilot/internal/shared/openai"
)

const (
	maxOutputChars   = 50000
	maxTokensPerCall = 8000
	commandTimeout   = 120 * time.Second
)

type toolUseInput struct {
	Command string `json:"command"`
}

type Agent struct {
	client  *openai.Client
	modelID string
	workDir string
	system  string
	tools   []openai.ToolDef
	history []openai.Message
}

func NewAgent() (*Agent, error) {
	_ = envutil.LoadDotEnv(".env")

	cfg, err := openai.ConfigFromEnv()
	if err != nil {
		return nil, err
	}

	workDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	tools := []openai.ToolDef{
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
		client:  openai.NewClient(cfg.APIKey, cfg.BaseURL, 180*time.Second),
		modelID: cfg.ModelID,
		workDir: workDir,
		system:  fmt.Sprintf("You are a coding agent at %s. Use bash to solve tasks. Act, don't explain.", workDir),
		tools:   tools,
	}, nil
}

func (a *Agent) RunTurn(query string) error {
	if len(a.history) == 0 {
		a.history = append(a.history, openai.Message{
			Role:    "system",
			Content: a.system,
		})
	}
	a.history = append(a.history, openai.Message{
		Role:    "user",
		Content: query,
	})

	for {
		resp, err := a.client.ChatCompletions(context.Background(), a.modelID, a.history, a.tools, maxTokensPerCall)
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

			a.history = append(a.history, openai.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
			})
		}
	}
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
