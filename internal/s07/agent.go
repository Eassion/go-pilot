package s07

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go-pilot/internal/shared/envutil"
	"go-pilot/internal/shared/openai"
)

const (
	maxOutputChars   = 50000
	maxTokensPerCall = 8000
	commandTimeout   = 120 * time.Second
	tasksDirName     = ".tasks"
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

type taskCreateInput struct {
	Subject     string `json:"subject"`
	Description string `json:"description,omitempty"`
}

type taskUpdateInput struct {
	TaskID          int    `json:"task_id"`
	Status          string `json:"status,omitempty"`
	AddBlockedBy    []int  `json:"addBlockedBy,omitempty"`
	RemoveBlockedBy []int  `json:"removeBlockedBy,omitempty"`
}

type taskGetInput struct {
	TaskID int `json:"task_id"`
}

type toolHandler func(arguments string) string

type Task struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
	BlockedBy   []int  `json:"blockedBy"`
	Owner       string `json:"owner"`
}

type TaskManager struct {
	dir    string
	nextID int
}

func NewTaskManager(dir string) (*TaskManager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	maxID, err := maxTaskID(dir)
	if err != nil {
		return nil, err
	}

	return &TaskManager{
		dir:    dir,
		nextID: maxID + 1,
	}, nil
}

func maxTaskID(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}

	maxID := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := taskIDFromFilename(e.Name())
		if ok && id > maxID {
			maxID = id
		}
	}
	return maxID, nil
}

func taskIDFromFilename(name string) (int, bool) {
	if !strings.HasPrefix(name, "task_") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	num := strings.TrimSuffix(strings.TrimPrefix(name, "task_"), ".json")
	id, err := strconv.Atoi(num)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (tm *TaskManager) taskPath(id int) string {
	return filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", id))
}

func (tm *TaskManager) load(id int) (Task, error) {
	path := tm.taskPath(id)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Task{}, fmt.Errorf("Task %d not found", id)
		}
		return Task{}, err
	}

	var t Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return Task{}, err
	}
	return t, nil
}

func (tm *TaskManager) save(t Task) error {
	path := tm.taskPath(t.ID)
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

func taskJSON(t Task) string {
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "Error: " + err.Error()
	}
	return string(raw)
}

func (tm *TaskManager) Create(subject, description string) (string, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", errors.New("subject is required")
	}

	t := Task{
		ID:          tm.nextID,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		BlockedBy:   []int{},
		Owner:       "",
	}
	if err := tm.save(t); err != nil {
		return "", err
	}
	tm.nextID++
	return taskJSON(t), nil
}

func (tm *TaskManager) Get(id int) (string, error) {
	t, err := tm.load(id)
	if err != nil {
		return "", err
	}
	return taskJSON(t), nil
}

func dedupeInts(xs []int) []int {
	if len(xs) == 0 {
		return []int{}
	}
	set := make(map[int]struct{}, len(xs))
	out := make([]int, 0, len(xs))
	for _, x := range xs {
		if x <= 0 {
			continue
		}
		if _, ok := set[x]; ok {
			continue
		}
		set[x] = struct{}{}
		out = append(out, x)
	}
	sort.Ints(out)
	return out
}

func (tm *TaskManager) Update(id int, status string, addBlockedBy, removeBlockedBy []int) (string, error) {
	t, err := tm.load(id)
	if err != nil {
		return "", err
	}

	status = strings.TrimSpace(status)
	if status != "" {
		switch status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("Invalid status: %s", status)
		}
		t.Status = status
		if status == "completed" {
			if err := tm.clearDependency(id); err != nil {
				return "", err
			}
		}
	}

	if len(addBlockedBy) > 0 {
		t.BlockedBy = append(t.BlockedBy, addBlockedBy...)
		t.BlockedBy = dedupeInts(t.BlockedBy)
	}

	if len(removeBlockedBy) > 0 {
		next := make([]int, 0, len(t.BlockedBy))
		for _, x := range t.BlockedBy {
			if !slices.Contains(removeBlockedBy, x) {
				next = append(next, x)
			}
		}
		t.BlockedBy = dedupeInts(next)
	}

	if err := tm.save(t); err != nil {
		return "", err
	}
	return taskJSON(t), nil
}

func (tm *TaskManager) clearDependency(completedID int) error {
	entries, err := os.ReadDir(tm.dir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := taskIDFromFilename(e.Name())
		if !ok {
			continue
		}

		t, err := tm.load(id)
		if err != nil {
			return err
		}
		if !slices.Contains(t.BlockedBy, completedID) {
			continue
		}

		next := make([]int, 0, len(t.BlockedBy))
		for _, x := range t.BlockedBy {
			if x != completedID {
				next = append(next, x)
			}
		}
		t.BlockedBy = dedupeInts(next)
		if err := tm.save(t); err != nil {
			return err
		}
	}

	return nil
}

func (tm *TaskManager) ListAll() (string, error) {
	entries, err := os.ReadDir(tm.dir)
	if err != nil {
		return "", err
	}

	tasks := make([]Task, 0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := taskIDFromFilename(e.Name())
		if !ok {
			continue
		}
		t, err := tm.load(id)
		if err != nil {
			return "", err
		}
		tasks = append(tasks, t)
	}

	if len(tasks) == 0 {
		return "No tasks.", nil
	}

	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].ID < tasks[j].ID
	})

	lines := make([]string, 0, len(tasks))
	for _, t := range tasks {
		marker := "[?]"
		switch t.Status {
		case "pending":
			marker = "[ ]"
		case "in_progress":
			marker = "[>]"
		case "completed":
			marker = "[x]"
		}

		blocked := ""
		if len(t.BlockedBy) > 0 {
			blocked = fmt.Sprintf(" (blocked by: %v)", t.BlockedBy)
		}
		lines = append(lines, fmt.Sprintf("%s #%d: %s%s", marker, t.ID, t.Subject, blocked))
	}
	return strings.Join(lines, "\n"), nil
}

type Agent struct {
	client   *openai.Client
	modelID  string
	workDir  string
	system   string
	tools    []openai.ToolDef
	tasks    *TaskManager
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

	tasks, err := NewTaskManager(filepath.Join(workDir, tasksDirName))
	if err != nil {
		return nil, err
	}

	a := &Agent{
		client:  openai.NewClient(cfg.APIKey, cfg.BaseURL, 180*time.Second),
		modelID: cfg.ModelID,
		workDir: workDir,
		system:  fmt.Sprintf("You are a coding agent at %s. Use task tools to plan and track work.", workDir),
		tools:   buildTools(),
		tasks:   tasks,
	}
	a.handlers = map[string]toolHandler{
		"bash":        a.handleBash,
		"read_file":   a.handleReadFile,
		"write_file":  a.handleWriteFile,
		"edit_file":   a.handleEditFile,
		"task_create": a.handleTaskCreate,
		"task_update": a.handleTaskUpdate,
		"task_list":   a.handleTaskList,
		"task_get":    a.handleTaskGet,
	}

	return a, nil
}

func buildTools() []openai.ToolDef {
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
		{
			Name:        "task_create",
			Description: "Create a new task.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"subject":     map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
				},
				"required": []string{"subject"},
			},
		},
		{
			Name:        "task_update",
			Description: "Update a task's status or dependencies.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "integer"},
					"status": map[string]any{
						"type": "string",
						"enum": []string{"pending", "in_progress", "completed"},
					},
					"addBlockedBy": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "integer"},
					},
					"removeBlockedBy": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "integer"},
					},
				},
				"required": []string{"task_id"},
			},
		},
		{
			Name:        "task_list",
			Description: "List all tasks with status summary.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "task_get",
			Description: "Get full details of a task by ID.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "integer"},
				},
				"required": []string{"task_id"},
			},
		},
	}
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

		toolMsgs := make([]openai.Message, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			handler := a.handlers[tc.Function.Name]
			output := fmt.Sprintf("Unknown tool: %s", tc.Function.Name)
			if handler != nil {
				output = handler(tc.Function.Arguments)
			}

			fmt.Printf("> %s:\n", tc.Function.Name)
			fmt.Println(preview(output, 200))

			toolMsgs = append(toolMsgs, openai.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
			})
		}

		a.history = append(a.history, toolMsgs...)
	}
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

func (a *Agent) handleTaskCreate(arguments string) string {
	var in taskCreateInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.tasks.Create(in.Subject, in.Description)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleTaskUpdate(arguments string) string {
	var in taskUpdateInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	if in.TaskID <= 0 {
		return "Error: task_id is required"
	}
	out, err := a.tasks.Update(in.TaskID, in.Status, in.AddBlockedBy, in.RemoveBlockedBy)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleTaskList(arguments string) string {
	if strings.TrimSpace(arguments) != "" && strings.TrimSpace(arguments) != "{}" {
		var dummy map[string]any
		if err := json.Unmarshal([]byte(arguments), &dummy); err != nil {
			return "Error: invalid tool input"
		}
	}
	out, err := a.tasks.ListAll()
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleTaskGet(arguments string) string {
	var in taskGetInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	if in.TaskID <= 0 {
		return "Error: task_id is required"
	}
	out, err := a.tasks.Get(in.TaskID)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
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
