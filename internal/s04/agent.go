package s04

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go-pilot/internal/shared/envutil"
	"go-pilot/internal/shared/openai"
)

const (
	maxOutputChars    = 50000
	maxTokensPerCall  = 8000
	commandTimeout    = 120 * time.Second
	maxTodoItems      = 20
	nagAfterRounds    = 3
	maxSubagentRounds = 30
)

type bashInput struct {
	Command string `json:"command"`
}

type readInput struct {
	Path  string `json:"path"`
	Limit int    `json:"limit,omitempty"`
}

type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type editInput struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

type todoInput struct {
	Items []todoItemInput `json:"items"`
}

type taskInput struct {
	Prompt      string `json:"prompt"`
	Description string `json:"description,omitempty"`
}

type todoItemInput struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

type todoItem struct {
	ID     string
	Text   string
	Status string
}

type TodoManager struct {
	items []todoItem
}

func (t *TodoManager) Update(items []todoItemInput) (string, error) {
	if len(items) > maxTodoItems {
		return "", fmt.Errorf("max %d todos allowed", maxTodoItems)
	}

	validated := make([]todoItem, 0, len(items))
	inProgressCount := 0

	for i, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = fmt.Sprintf("%d", i+1)
		}

		text := strings.TrimSpace(item.Text)
		if text == "" {
			return "", fmt.Errorf("item %s: text required", id)
		}

		status := strings.ToLower(strings.TrimSpace(item.Status))
		if status == "" {
			status = "pending"
		}

		switch status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("item %s: invalid status %q", id, status)
		}

		if status == "in_progress" {
			inProgressCount++
		}

		validated = append(validated, todoItem{
			ID:     id,
			Text:   text,
			Status: status,
		})
	}

	if inProgressCount > 1 {
		return "", errors.New("only one task can be in_progress at a time")
	}

	t.items = validated
	return t.Render(), nil
}

func (t *TodoManager) Render() string {
	if len(t.items) == 0 {
		return "No todos."
	}

	lines := make([]string, 0, len(t.items)+1)
	completed := 0
	for _, item := range t.items {
		marker := "[ ]"
		switch item.Status {
		case "in_progress":
			marker = "[>]"
		case "completed":
			marker = "[x]"
			completed++
		}
		lines = append(lines, fmt.Sprintf("%s #%s: %s", marker, item.ID, item.Text))
	}
	lines = append(lines, fmt.Sprintf("\n(%d/%d completed)", completed, len(t.items)))
	return strings.Join(lines, "\n")
}

type toolHandler func(arguments string) string

type Agent struct {
	client         *openai.Client
	modelID        string
	workDir        string
	system         string
	subagentSystem string
	tools          []openai.ToolDef
	subTools       []openai.ToolDef
	todos          TodoManager

	handlers map[string]toolHandler
	history  []openai.Message
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

	a := &Agent{
		client:  openai.NewClient(cfg.APIKey, cfg.BaseURL, 180*time.Second),
		modelID: cfg.ModelID,
		workDir: workDir,
		system: fmt.Sprintf(
			"You are a coding agent at %s. Use todo for multi-step tasks. Use task to delegate exploration or subtasks. Act, don't explain.",
			workDir,
		),
		subagentSystem: fmt.Sprintf(
			"You are a coding subagent at %s. Complete the given task, then summarize your findings.",
			workDir,
		),
		tools:    buildParentTools(),
		subTools: buildSubTools(),
	}
	a.handlers = map[string]toolHandler{
		"bash":       a.handleBash,
		"read_file":  a.handleReadFile,
		"write_file": a.handleWriteFile,
		"edit_file":  a.handleEditFile,
		"todo":       a.handleTodo,
		"task":       a.handleTask,
	}

	return a, nil
}

func buildSubTools() []openai.ToolDef {
	return []openai.ToolDef{
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
		{
			Name:        "read_file",
			Description: "Read file contents.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":  map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "write_file",
			Description: "Write content to file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "edit_file",
			Description: "Replace exact text in file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":     map[string]any{"type": "string"},
					"old_text": map[string]any{"type": "string"},
					"new_text": map[string]any{"type": "string"},
				},
				"required": []string{"path", "old_text", "new_text"},
			},
		},
	}
}

func buildParentTools() []openai.ToolDef {
	tools := buildSubTools()
	tools = append(tools,
		openai.ToolDef{
			Name:        "todo",
			Description: "Update task list and progress for multi-step tasks.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"items": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":     map[string]any{"type": "string"},
								"text":   map[string]any{"type": "string"},
								"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
							},
							"required": []string{"id", "text", "status"},
						},
					},
				},
				"required": []string{"items"},
			},
		},
		openai.ToolDef{
			Name:        "task",
			Description: "Spawn a subagent with fresh context. It shares the filesystem but not conversation history.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt":      map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
				},
				"required": []string{"prompt"},
			},
		},
	)
	return tools
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

	roundsSinceTodo := 0

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

		usedTodo := false
		toolMsgs := make([]openai.Message, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			handler := a.handlers[tc.Function.Name]
			output := fmt.Sprintf("Unknown tool: %s", tc.Function.Name)
			if handler != nil {
				output = handler(tc.Function.Arguments)
			}

			if tc.Function.Name == "task" {
				fmt.Printf("  %s\n", preview(output, 200))
			} else {
				fmt.Printf("> %s:\n", tc.Function.Name)
				fmt.Println(preview(output, 200))
			}

			toolMsgs = append(toolMsgs, openai.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
			})

			if tc.Function.Name == "todo" {
				usedTodo = true
			}
		}

		if usedTodo {
			roundsSinceTodo = 0
		} else {
			roundsSinceTodo++
		}

		a.history = append(a.history, toolMsgs...)

		if roundsSinceTodo >= nagAfterRounds {
			a.history = append(a.history, openai.Message{
				Role:    "user",
				Content: "<reminder>Update your todos.</reminder>",
			})
		}
	}
}

func (a *Agent) runSubagent(prompt string) string {
	subHistory := []openai.Message{
		{Role: "system", Content: a.subagentSystem},
		{Role: "user", Content: prompt},
	}

	var lastText string
	for i := 0; i < maxSubagentRounds; i++ {
		resp, err := a.client.ChatCompletions(context.Background(), a.modelID, subHistory, a.subTools, maxTokensPerCall)
		if err != nil {
			return "Error: " + err.Error()
		}
		if len(resp.Choices) == 0 {
			return "Error: subagent response has no choices"
		}

		msg := resp.Choices[0].Message
		subHistory = append(subHistory, msg)

		if len(msg.ToolCalls) == 0 {
			text, _ := msg.Content.(string)
			lastText = strings.TrimSpace(text)
			if lastText == "" {
				return "(no summary)"
			}
			return lastText
		}

		toolMsgs := make([]openai.Message, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			output := fmt.Sprintf("Unknown tool: %s", tc.Function.Name)
			switch tc.Function.Name {
			case "bash":
				output = a.handleBash(tc.Function.Arguments)
			case "read_file":
				output = a.handleReadFile(tc.Function.Arguments)
			case "write_file":
				output = a.handleWriteFile(tc.Function.Arguments)
			case "edit_file":
				output = a.handleEditFile(tc.Function.Arguments)
			case "todo", "task":
				output = fmt.Sprintf("Error: subagent cannot call %s", tc.Function.Name)
			}

			toolMsgs = append(toolMsgs, openai.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    truncate(output, maxOutputChars),
			})
		}
		subHistory = append(subHistory, toolMsgs...)
	}

	return "Error: subagent exceeded max rounds"
}

func (a *Agent) handleBash(arguments string) string {
	var in bashInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	return runBash(a.workDir, in.Command)
}

func (a *Agent) handleReadFile(arguments string) string {
	var in readInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	if strings.TrimSpace(in.Path) == "" {
		return "Error: path is required"
	}
	return runRead(a.workDir, in.Path, in.Limit)
}

func (a *Agent) handleWriteFile(arguments string) string {
	var in writeInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	if strings.TrimSpace(in.Path) == "" {
		return "Error: path is required"
	}
	return runWrite(a.workDir, in.Path, in.Content)
}

func (a *Agent) handleEditFile(arguments string) string {
	var in editInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	if strings.TrimSpace(in.Path) == "" {
		return "Error: path is required"
	}
	return runEdit(a.workDir, in.Path, in.OldText, in.NewText)
}

func (a *Agent) handleTodo(arguments string) string {
	var in todoInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	rendered, err := a.todos.Update(in.Items)
	if err != nil {
		return "Error: " + err.Error()
	}
	return rendered
}

func (a *Agent) handleTask(arguments string) string {
	var in taskInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return "Error: prompt is required"
	}

	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		desc = "subtask"
	}
	fmt.Printf("> task (%s): %s\n", desc, preview(prompt, 80))
	return a.runSubagent(prompt)
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

func runRead(workDir, path string, limit int) string {
	fp, err := safePath(workDir, path)
	if err != nil {
		return "Error: " + err.Error()
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		return "Error: " + err.Error()
	}

	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if limit > 0 && limit < len(lines) {
		lines = append(lines[:limit], fmt.Sprintf("... (%d more lines)", len(lines)-limit))
	}
	return truncate(strings.Join(lines, "\n"), maxOutputChars)
}

func runWrite(workDir, path, content string) string {
	fp, err := safePath(workDir, path)
	if err != nil {
		return "Error: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return "Error: " + err.Error()
	}
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

func runEdit(workDir, path, oldText, newText string) string {
	fp, err := safePath(workDir, path)
	if err != nil {
		return "Error: " + err.Error()
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		return "Error: " + err.Error()
	}

	content := string(raw)
	if !strings.Contains(content, oldText) {
		return fmt.Sprintf("Error: Text not found in %s", path)
	}
	updated := strings.Replace(content, oldText, newText, 1)
	if err := os.WriteFile(fp, []byte(updated), 0o644); err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Edited %s", path)
}

func safePath(workDir, p string) (string, error) {
	baseAbs, err := filepath.Abs(workDir)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(filepath.Join(baseAbs, p))
	if err != nil {
		return "", err
	}

	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes workspace: %s", p)
	}
	return targetAbs, nil
}

func preview(s string, max int) string {
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
