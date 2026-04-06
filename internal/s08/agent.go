package s08

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"go-pilot/internal/shared/envutil"
	"go-pilot/internal/shared/openai"
)

const (
	maxOutputChars      = 50000
	maxTokensPerCall    = 8000
	commandTimeout      = 120 * time.Second
	backgroundTimeout   = 300 * time.Second
	notificationMaxText = 500
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

type backgroundRunInput struct {
	Command string `json:"command"`
}

type checkBackgroundInput struct {
	TaskID string `json:"task_id,omitempty"`
}

type toolHandler func(arguments string) string

type backgroundTask struct {
	Status  string
	Result  string
	Command string
}

type backgroundNotification struct {
	TaskID  string
	Status  string
	Command string
	Result  string
}

type BackgroundManager struct {
	workDir string

	mu            sync.Mutex
	tasks         map[string]backgroundTask
	notifications []backgroundNotification
}

func NewBackgroundManager(workDir string) *BackgroundManager {
	return &BackgroundManager{
		workDir: workDir,
		tasks:   map[string]backgroundTask{},
	}
}

//
func (bm *BackgroundManager) Run(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return "Error: command is required"
	}

	//加锁访问taskMap  随机生成taskId
	bm.mu.Lock()
	taskID := ""
	for {
		taskID = randomHex(4)
		if _, exists := bm.tasks[taskID]; !exists {
			break
		}
	}
	bm.tasks[taskID] = backgroundTask{
		Status:  "running",
		Result:  "",
		Command: command,
	}
	bm.mu.Unlock()

	go bm.execute(taskID, command)
	return fmt.Sprintf("Background task %s started: %s", taskID, preview(command, 80))
}

//查询任务完成情况，taskId参数为空时查询所有任务状态
func (bm *BackgroundManager) Check(taskID string) string {
	taskID = strings.TrimSpace(taskID)

	//查询taskMap需要加锁，防止和execute函数中的写操作冲突   保证并发安全
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if taskID != "" {
		t, ok := bm.tasks[taskID]
		if !ok {
			return fmt.Sprintf("Error: Unknown task %s", taskID)
		}
		result := t.Result
		if strings.TrimSpace(result) == "" {
			result = "(running)"
		}
		return fmt.Sprintf("[%s] %s\n%s", t.Status, preview(t.Command, 60), result)
	}

	if len(bm.tasks) == 0 {
		return "No background tasks."
	}

	ids := make([]string, 0, len(bm.tasks))
	for id := range bm.tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		t := bm.tasks[id]
		lines = append(lines, fmt.Sprintf("%s: [%s] %s", id, t.Status, preview(t.Command, 60)))
	}
	return strings.Join(lines, "\n")
}

func (bm *BackgroundManager) DrainNotifications() []backgroundNotification {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if len(bm.notifications) == 0 {
		return nil
	}
	out := make([]backgroundNotification, len(bm.notifications))
	copy(out, bm.notifications)
	bm.notifications = nil
	return out
}

//execute函数在后台goroutine中运行，执行shell命令，并将结果保存到taskMap中，同时将完成通知添加到notifications队列中
func (bm *BackgroundManager) execute(taskID, command string) {
	output, timedOut, runErr := runShellCommand(bm.workDir, command, backgroundTimeout)

	status := "completed"
	if timedOut {
		status = "timeout"
	} else if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			status = "error"
		}
	}

	output = truncate(output, maxOutputChars)
	if strings.TrimSpace(output) == "" {
		output = "(no output)"
	}

	bm.mu.Lock()
	bm.tasks[taskID] = backgroundTask{
		Status:  status,
		Result:  output,
		Command: command,
	}
	bm.notifications = append(bm.notifications, backgroundNotification{
		TaskID:  taskID,
		Status:  status,
		Command: preview(command, 80),
		Result:  truncate(output, notificationMaxText),
	})
	bm.mu.Unlock()
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

type Agent struct {
	client   *openai.Client
	modelID  string
	workDir  string
	system   string
	tools    []openai.ToolDef
	bg       *BackgroundManager
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
		system:  fmt.Sprintf("You are a coding agent at %s. Use background_run for long-running commands.", workDir),
		tools:   buildTools(),
		bg:      NewBackgroundManager(workDir),
	}
	a.handlers = map[string]toolHandler{
		"bash":             a.handleBash,
		"read_file":        a.handleReadFile,
		"write_file":       a.handleWriteFile,
		"edit_file":        a.handleEditFile,
		"background_run":   a.handleBackgroundRun,
		"check_background": a.handleCheckBackground,
	}

	return a, nil
}

func buildTools() []openai.ToolDef {
	return []openai.ToolDef{
		{
			Name:        "bash",
			Description: "Run a shell command (blocking).",
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
			Name:        "background_run",
			Description: "Run command in a background goroutine. Returns task_id immediately.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "check_background",
			Description: "Check background task status. Omit task_id to list all.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string"},
				},
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
		//如果队列有任务完成通知，将这些内容作为user消息添加到对话历史中，供模型参考
		if notifs := a.bg.DrainNotifications(); len(notifs) > 0 {
			lines := make([]string, 0, len(notifs))
			for _, n := range notifs {
				lines = append(lines, fmt.Sprintf("[bg:%s] %s: %s", n.TaskID, n.Status, n.Result))
			}
			a.history = append(a.history, openai.Message{
				Role:    "user",
				Content: "<background-results>\n" + strings.Join(lines, "\n") + "\n</background-results>",
			})
		}

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

func (a *Agent) handleBackgroundRun(arguments string) string {
	var in backgroundRunInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	return a.bg.Run(in.Command)
}

func (a *Agent) handleCheckBackground(arguments string) string {
	args := strings.TrimSpace(arguments)
	if args == "" || args == "{}" {
		return a.bg.Check("")
	}

	var in checkBackgroundInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	return a.bg.Check(in.TaskID)
}

func runBash(workDir, command string) string {
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}

	output, timedOut, _ := runShellCommand(workDir, command, commandTimeout)
	if timedOut {
		return "Error: Timeout (120s)"
	}
	return truncate(output, maxOutputChars)
}

func runShellCommand(workDir, command string, timeout time.Duration) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := shellCommand(ctx, command)
	cmd.Dir = workDir

	//执行命令并捕获输出和错误，如果ctx超时了，err会包含context.DeadlineExceeded，可以据此判断是否是超时导致的错误
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "Error: Timeout (" + strconvSeconds(timeout) + "s)", true, err
	}

	text := strings.TrimSpace(string(out))
	if err != nil && text == "" {
		text = err.Error()
	}
	if text == "" {
		text = "(no output)"
	}
	return text, false, err
}

//根据操作系统选择合适的shell命令执行方式，Windows使用powershell，其他系统使用bash
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "powershell", "-Command", command)
	}
	return exec.CommandContext(ctx, "bash", "-lc", command)
}

func strconvSeconds(d time.Duration) string {
	return fmt.Sprintf("%d", int(d.Seconds()))
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
