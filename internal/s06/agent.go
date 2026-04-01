package s06

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
	maxOutputChars        = 50000
	maxTokensPerCall      = 8000
	commandTimeout        = 120 * time.Second
	compactThreshold      = 50000
	keepRecentToolResults = 3
	minToolResultChars    = 100
	summaryMaxTokens      = 2000
	transcriptTailChars   = 80000
	transcriptDirName     = ".transcripts"
)

var preserveResultTools = map[string]bool{
	"read_file": true,
}

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

type compactInput struct {
	Focus string `json:"focus,omitempty"`
}

type toolHandler func(arguments string) string

type Agent struct {
	client   *openai.Client
	modelID  string
	workDir  string
	system   string
	tools    []openai.ToolDef
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
		system:  fmt.Sprintf("You are a coding agent at %s. Use tools to solve tasks. Use compact when context grows too large.", workDir),
		tools:   buildTools(),
	}
	a.handlers = map[string]toolHandler{
		"bash":       a.handleBash,
		"read_file":  a.handleReadFile,
		"write_file": a.handleWriteFile,
		"edit_file":  a.handleEditFile,
		"compact":    a.handleCompact,
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
			Name:        "compact",
			Description: "Trigger manual conversation compression.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"focus": map[string]any{
						"type":        "string",
						"description": "What to preserve in the summary",
					},
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
		a.microCompact()

		if estimateTokens(a.history) > compactThreshold {
			fmt.Println("[auto compact triggered]")
			if err := a.autoCompact(""); err != nil {
				return err
			}
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

		manualCompact := false
		manualFocus := ""
		toolMsgs := make([]openai.Message, 0, len(msg.ToolCalls))

		for _, tc := range msg.ToolCalls {

			//手动调用压缩命令  后面参数为需要重点保留的命令
			if tc.Function.Name == "compact" {
				manualCompact = true
				manualFocus = parseCompactFocus(tc.Function.Arguments)
			}

			handler := a.handlers[tc.Function.Name]
			output := fmt.Sprintf("Unknown tool: %s", tc.Function.Name)
			if handler != nil {

				//如果tool调用了compact命令，则先生成输出（"Compressing..."），执行压缩逻辑在
				//样用户就能看到压缩命令的反馈，同时也触发了后续的压缩处理，保证在用户明确要求压缩时能够及时进行上下文管理，避免后续对话因上下文过长而出现问题。
				//对于其他工具调用，则正常执行并返回结果。
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

		if manualCompact {
			fmt.Println("[manual compact]")
			if err := a.autoCompact(manualFocus); err != nil {
				return err
			}
			return nil
		}
	}
}


//	microCompact示例：
// 原来有 5 条 tool 消息（按时间）：
// 1) bash: 很长输出(500字)
// 2) write_file: 很长输出(300字)
// 3) read_file: 很长输出(800字)
// 4) edit_file: 很长输出(200字)
// 5) bash: 很长输出(400字)

// 执行 microCompact 后（只保留最近3条：3/4/5）：
// 1) bash -> "[Previous: used bash]"      (被压缩)
// 2) write_file -> "[Previous: used write_file]" (被压缩)
// 3) read_file -> 保留原文                 (最近3条且read_file本来也受保护)
// 4) edit_file -> 保留原文                 (最近3条)
// 5) bash -> 保留原文                      (最近3条)

func (a *Agent) microCompact() {
	toolNameByCallID := make(map[string]string)
	for _, msg := range a.history {
		//只有assistant调用时才有tool_call_id
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		for _, tc := range msg.ToolCalls {
			toolNameByCallID[tc.ID] = tc.Function.Name
		}
	}

	toolIndexes := make([]int, 0)
	for i, msg := range a.history {
		if msg.Role == "tool" {
			toolIndexes = append(toolIndexes, i)
		}
	}
	if len(toolIndexes) <= keepRecentToolResults {
		return
	}

	toCompact := toolIndexes[:len(toolIndexes)-keepRecentToolResults]
	for _, idx := range toCompact {
		msg := &a.history[idx]
		content, ok := msg.Content.(string)
		if !ok || len(content) <= minToolResultChars {
			continue
		}

		toolName := toolNameByCallID[msg.ToolCallID]
		if toolName == "" {
			toolName = "unknown"
		}
		if preserveResultTools[toolName] {
			continue
		}
		msg.Content = fmt.Sprintf("[Previous: used %s]", toolName)
	}
}

//old history保存在文件里，然后调用大模型生成summary，最后更新history为：system + summary，summary里包含了压缩前的conversation的关键信息和状态，以及保存的transcript文件路径（如果有）。
//这样既能保留重要信息，又能大幅减少history长度，避免上下文过长导致模型无法处理。summary里还可以根据需要保留特定focus相关的信息，保证压缩后仍然保留对当前任务重要的细节。
func (a *Agent) autoCompact(focus string) error {
	transcriptPath, err := a.saveTranscript()
	if err != nil {
		return err
	}
	fmt.Printf("[transcript saved: %s]\n", transcriptPath)

	raw, err := json.Marshal(a.history)
	if err != nil {
		return err
	}
	conversationText := string(raw)
	//控制传给“压缩总结”模型的输入大小，避免太长，优先保留最新部分内容
	if len(conversationText) > transcriptTailChars {
		conversationText = conversationText[len(conversationText)-transcriptTailChars:]
	}

	prompt := "Summarize this conversation for continuity. Include: 1) What was accomplished, 2) Current state, 3) Key decisions made. Be concise but preserve critical details."
	focus = strings.TrimSpace(focus)
	if focus != "" {
		prompt += "\n4) Preserve details related to: " + focus
	}
	prompt += "\n\n" + conversationText

	resp, err := a.client.ChatCompletions(
		context.Background(),
		a.modelID,
		[]openai.Message{{Role: "user", Content: prompt}},
		nil,
		summaryMaxTokens,
	)
	if err != nil {
		return err
	}
	if len(resp.Choices) == 0 {
		return errors.New("summary response has no choices")
	}

	summary, _ := resp.Choices[0].Message.Content.(string)
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = "(empty summary)"
	}

	a.history = []openai.Message{
		{Role: "system", Content: a.system},
		{
			Role:    "user",
			Content: fmt.Sprintf("[Conversation compressed. Transcript: %s]\n\n%s", transcriptPath, summary),
		},
	}
	return nil
}

func (a *Agent) saveTranscript() (string, error) {
	dir := filepath.Join(a.workDir, transcriptDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	path := filepath.Join(dir, fmt.Sprintf("transcript_%d.jsonl", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	for _, msg := range a.history {
		line, err := json.Marshal(msg)
		if err != nil {
			return "", err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return "", err
		}
	}
	return path, nil
}

func estimateTokens(messages []openai.Message) int {
	raw, err := json.Marshal(messages)
	if err != nil {
		return len(fmt.Sprintf("%v", messages)) / 4
	}
	return len(raw) / 4
}

//处理用户输入的压缩命令，提取其中的focus参数，供后续在autoCompact时使用，以保留用户指定的关键信息。
func parseCompactFocus(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return ""
	}
	var in compactInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return ""
	}
	return strings.TrimSpace(in.Focus)
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

func (a *Agent) handleCompact(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return "Compressing..."
	}
	var in compactInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	return "Compressing..."
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
