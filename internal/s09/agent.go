package s09

import (
	"bufio"
	"context"
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
	teamDirName         = ".team"
	inboxDirName        = "inbox"
	teammateMaxRounds   = 50
	teammateHTTPTimeout = 180 * time.Second
)

var validMessageTypes = map[string]bool{
	"message":                true,
	"broadcast":              true,
	"shutdown_request":       true,
	"shutdown_response":      true,
	"plan_approval_response": true,
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

// spawnTeammateInput is the input schema for the spawn_teammate tool.
type spawnTeammateInput struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Prompt string `json:"prompt"`
}

// sendMessageInput is the input schema for the send_message tool.
type sendMessageInput struct {
	To      string `json:"to"`
	Content string `json:"content"`
	MsgType string `json:"msg_type,omitempty"`
}

// broadcastInput is the input schema for the broadcast tool.
type broadcastInput struct {
	Content string `json:"content"`
}

type toolHandler func(arguments string) string

// busMessage represents a message sent between teammates via the MessageBus.
type busMessage struct {
	Type      string         `json:"type"`
	From      string         `json:"from"`
	Content   string         `json:"content"`
	Timestamp float64        `json:"timestamp"`
	Extra     map[string]any `json:"-"`
}

func (m busMessage) toMap() map[string]any {
	out := map[string]any{
		"type":      m.Type,
		"from":      m.From,
		"content":   m.Content,
		"timestamp": m.Timestamp,
	}
	for k, v := range m.Extra {
		out[k] = v
	}
	return out
}

// MessageBus provides a simple file-based messaging system for teammates to communicate via inboxes.
type MessageBus struct {
	inboxDir string
	mu       sync.Mutex
}

func NewMessageBus(teamDir string) (*MessageBus, error) {
	inboxDir := filepath.Join(teamDir, inboxDirName)
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		return nil, err
	}
	return &MessageBus{inboxDir: inboxDir}, nil
}

// 往收件箱inbox中加锁写邮件
func (b *MessageBus) Send(sender, to, content, msgType string, extra map[string]any) (string, error) {
	sender = strings.TrimSpace(sender)
	to = strings.TrimSpace(to)
	content = strings.TrimSpace(content)
	msgType = strings.TrimSpace(msgType)
	if msgType == "" {
		msgType = "message"
	}

	if sender == "" {
		return "", errors.New("sender is required")
	}
	if to == "" {
		return "", errors.New("to is required")
	}
	if content == "" {
		return "", errors.New("content is required")
	}
	if !validMessageTypes[msgType] {
		return "", fmt.Errorf("invalid type '%s'", msgType)
	}

	msg := busMessage{
		Type:      msgType,
		From:      sender,
		Content:   content,
		Timestamp: float64(time.Now().UnixNano()) / 1e9,
		Extra:     extra,
	}
	raw, err := json.Marshal(msg.toMap())
	if err != nil {
		return "", err
	}

	path := filepath.Join(b.inboxDir, to+".jsonl")
	b.mu.Lock()
	defer b.mu.Unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := f.Write(append(raw, '\n')); err != nil {
		return "", err
	}
	return fmt.Sprintf("Sent %s to %s", msgType, to), nil
}

//读取某个成员收件箱并清空
func (b *MessageBus) ReadInbox(name string) ([]map[string]any, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name is required")
	}

	path := filepath.Join(b.inboxDir, name+".jsonl")

	b.mu.Lock()
	defer b.mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []map[string]any{}, nil
		}
		return nil, err
	}

	//按行解析，每行都是一条消息的JSON对象，最终返回消息列表
	messages := make([]map[string]any, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		messages = append(messages, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// 清空收件箱
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		return nil, err
	}
	return messages, nil
}

//广播发消息
func (b *MessageBus) Broadcast(sender, content string, teammates []string) (string, error) {
	count := 0
	for _, name := range teammates {
		if strings.TrimSpace(name) == "" || name == sender {
			continue
		}
		if _, err := b.Send(sender, name, content, "broadcast", nil); err == nil {
			count++
		}
	}
	return fmt.Sprintf("Broadcast to %d teammates", count), nil
}

type TeamMember struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type TeamConfig struct {
	TeamName string       `json:"team_name"`
	Members  []TeamMember `json:"members"`
}

type TeammateManager struct {
	workDir string
	client  *openai.Client
	modelID string
	bus     *MessageBus

	configPath string

	mu      sync.Mutex
	config  TeamConfig
	//正在运行队友 goroutine 的集合
	threads map[string]struct{}
}

func NewTeammateManager(workDir, teamDir string, client *openai.Client, modelID string, bus *MessageBus) (*TeammateManager, error) {
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		return nil, err
	}

	tm := &TeammateManager{
		workDir:    workDir,
		client:     client,
		modelID:    modelID,
		bus:        bus,
		configPath: filepath.Join(teamDir, "config.json"),
		threads:    map[string]struct{}{},
	}
	if err := tm.loadConfig(); err != nil {
		return nil, err
	}
	return tm, nil
}

// loadConfig loads the team configuration from disk. If the config file does not exist, it initializes a default config and saves it.
func (tm *TeammateManager) loadConfig() error {
	raw, err := os.ReadFile(tm.configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			tm.config = TeamConfig{
				TeamName: "default",
				Members:  []TeamMember{},
			}
			return tm.saveConfigLocked()
		}
		return err
	}

	var cfg TeamConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.TeamName) == "" {
		cfg.TeamName = "default"
	}
	if cfg.Members == nil {
		cfg.Members = []TeamMember{}
	}
	tm.config = cfg
	return nil
}

func (tm *TeammateManager) saveConfigLocked() error {
	raw, err := json.MarshalIndent(tm.config, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(tm.configPath, raw, 0o644)
}

func (tm *TeammateManager) findMemberLocked(name string) (int, *TeamMember) {
	for i := range tm.config.Members {
		if tm.config.Members[i].Name == name {
			return i, &tm.config.Members[i]
		}
	}
	return -1, nil
}

// Spawn creates a new teammate with the given name, role, and prompt. 
// If a teammate with the same name already exists and is idle or shutdown, it will be respawned with the new role and prompt. 
func (tm *TeammateManager) Spawn(name, role, prompt string) (string, error) {
	name = strings.TrimSpace(name)
	role = strings.TrimSpace(role)
	prompt = strings.TrimSpace(prompt)
	if name == "" || role == "" || prompt == "" {
		return "", errors.New("name, role, and prompt are required")
	}

	tm.mu.Lock()
	_, member := tm.findMemberLocked(name)
	if member != nil {
		if member.Status != "idle" && member.Status != "shutdown" {
			status := member.Status
			tm.mu.Unlock()
			return "", fmt.Errorf("'%s' is currently %s", name, status)
		}
		member.Status = "working"
		member.Role = role
	} else {
		tm.config.Members = append(tm.config.Members, TeamMember{
			Name:   name,
			Role:   role,
			Status: "working",
		})
	}

	if err := tm.saveConfigLocked(); err != nil {
		tm.mu.Unlock()
		return "", err
	}
	tm.threads[name] = struct{}{}
	tm.mu.Unlock()

	go tm.teammateLoop(name, role, prompt)
	return fmt.Sprintf("Spawned '%s' (role: %s)", name, role), nil
}

// teammateLoop is the main loop for a teammate. 
// It runs in its own goroutine and processes messages until it reaches the maximum number of rounds or encounters an error.
func (tm *TeammateManager) teammateLoop(name, role, prompt string) {
	sysPrompt := fmt.Sprintf(
		"You are '%s', role: %s, at %s. Use send_message to communicate. Complete your task.",
		name, role, tm.workDir,
	)

	messages := []openai.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: prompt},
	}

	tools := tm.teammateTools()

	//teammateLoop runs for a fixed number of rounds to prevent infinite loops. 
	// Each round, it first reads the inbox for new messages, then calls the LLM, executes any tool calls, and appends the results back to the conversation.
	for i := 0; i < teammateMaxRounds; i++ {
		inbox, err := tm.bus.ReadInbox(name)
		if err == nil && len(inbox) > 0 {
			raw, _ := json.MarshalIndent(inbox, "", "  ")
			messages = append(messages, openai.Message{
				Role:    "user",
				Content: "<inbox>" + string(raw) + "</inbox>",
			})
		}

		resp, err := tm.client.ChatCompletions(context.Background(), tm.modelID, messages, tools, maxTokensPerCall)
		if err != nil {
			break
		}
		if len(resp.Choices) == 0 {
			break
		}

		msg := resp.Choices[0].Message
		messages = append(messages, msg)
		if len(msg.ToolCalls) == 0 {
			break
		}

		toolMsgs := make([]openai.Message, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			output := tm.execTeammateTool(name, tc.Function.Name, tc.Function.Arguments)
			fmt.Printf("  [%s] %s: %s\n", name, tc.Function.Name, preview(output, 120))
			toolMsgs = append(toolMsgs, openai.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
			})
		}
		messages = append(messages, toolMsgs...)
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()
	//删除正在运行的标志
	delete(tm.threads, name)
	if _, member := tm.findMemberLocked(name); member != nil && member.Status != "shutdown" {
		member.Status = "idle"
		_ = tm.saveConfigLocked()
	}
}

func (tm *TeammateManager) execTeammateTool(sender, toolName, arguments string) string {
	switch toolName {
	case "bash":
		var in bashInput
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "Error: invalid tool input"
		}
		return runBash(tm.workDir, in.Command)
	case "read_file":
		var in readInput
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "Error: invalid tool input"
		}
		if strings.TrimSpace(in.Path) == "" {
			return "Error: path is required"
		}
		return runRead(tm.workDir, in.Path, in.Limit)
	case "write_file":
		var in writeInput
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "Error: invalid tool input"
		}
		if strings.TrimSpace(in.Path) == "" {
			return "Error: path is required"
		}
		return runWrite(tm.workDir, in.Path, in.Content)
	case "edit_file":
		var in editInput
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "Error: invalid tool input"
		}
		if strings.TrimSpace(in.Path) == "" {
			return "Error: path is required"
		}
		return runEdit(tm.workDir, in.Path, in.OldText, in.NewText)
	case "send_message":
		var in sendMessageInput
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "Error: invalid tool input"
		}
		out, err := tm.bus.Send(sender, in.To, in.Content, in.MsgType, nil)
		if err != nil {
			return "Error: " + err.Error()
		}
		return out
	case "read_inbox":
		messages, err := tm.bus.ReadInbox(sender)
		if err != nil {
			return "Error: " + err.Error()
		}
		raw, _ := json.MarshalIndent(messages, "", "  ")
		return string(raw)
	default:
		return fmt.Sprintf("Unknown tool: %s", toolName)
	}
}

func (tm *TeammateManager) teammateTools() []openai.ToolDef {
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
					"path": map[string]any{"type": "string"},
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
			Name:        "send_message",
			Description: "Send message to a teammate.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to":      map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
					"msg_type": map[string]any{
						"type": "string",
						"enum": messageTypeEnum(),
					},
				},
				"required": []string{"to", "content"},
			},
		},
		{
			Name:        "read_inbox",
			Description: "Read and drain your inbox.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func (tm *TeammateManager) ListAll() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if len(tm.config.Members) == 0 {
		return "No teammates."
	}
	lines := []string{fmt.Sprintf("Team: %s", tm.config.TeamName)}

	members := make([]TeamMember, 0, len(tm.config.Members))
	members = append(members, tm.config.Members...)
	sort.Slice(members, func(i, j int) bool {
		return members[i].Name < members[j].Name
	})

	for _, m := range members {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", m.Name, m.Role, m.Status))
	}
	return strings.Join(lines, "\n")
}

func (tm *TeammateManager) MemberNames() []string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	out := make([]string, 0, len(tm.config.Members))
	for _, m := range tm.config.Members {
		out = append(out, m.Name)
	}
	return out
}

type Agent struct {
	client   *openai.Client
	modelID  string
	workDir  string
	system   string
	tools    []openai.ToolDef
	bus      *MessageBus
	team     *TeammateManager
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

	teamDir := filepath.Join(workDir, teamDirName)
	bus, err := NewMessageBus(teamDir)
	if err != nil {
		return nil, err
	}

	client := openai.NewClient(cfg.APIKey, cfg.BaseURL, teammateHTTPTimeout)
	team, err := NewTeammateManager(workDir, teamDir, client, cfg.ModelID, bus)
	if err != nil {
		return nil, err
	}

	a := &Agent{
		client:  client,
		modelID: cfg.ModelID,
		workDir: workDir,
		system:  fmt.Sprintf("You are a team lead at %s. Spawn teammates and communicate via inboxes.", workDir),
		tools:   buildTools(),
		bus:     bus,
		team:    team,
	}
	a.handlers = map[string]toolHandler{
		"bash":           a.handleBash,
		"read_file":      a.handleReadFile,
		"write_file":     a.handleWriteFile,
		"edit_file":      a.handleEditFile,
		"spawn_teammate": a.handleSpawnTeammate,
		"list_teammates": a.handleListTeammates,
		"send_message":   a.handleSendMessage,
		"read_inbox":     a.handleReadInbox,
		"broadcast":      a.handleBroadcast,
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
			Name:        "spawn_teammate",
			Description: "Spawn a persistent teammate that runs in its own thread.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":   map[string]any{"type": "string"},
					"role":   map[string]any{"type": "string"},
					"prompt": map[string]any{"type": "string"},
				},
				"required": []string{"name", "role", "prompt"},
			},
		},
		{
			Name:        "list_teammates",
			Description: "List all teammates with name, role, status.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "send_message",
			Description: "Send a message to a teammate's inbox.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to":      map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
					"msg_type": map[string]any{
						"type": "string",
						"enum": messageTypeEnum(),
					},
				},
				"required": []string{"to", "content"},
			},
		},
		{
			Name:        "read_inbox",
			Description: "Read and drain the lead's inbox.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "broadcast",
			Description: "Send a message to all teammates.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{"type": "string"},
				},
				"required": []string{"content"},
			},
		},
	}
}

func messageTypeEnum() []string {
	out := make([]string, 0, len(validMessageTypes))
	for k := range validMessageTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (a *Agent) RunTurn(query string) error {
	q := strings.TrimSpace(query)
	switch q {
	case "/team":
		fmt.Println(a.team.ListAll())
		return nil
	case "/inbox":
		msgs, err := a.bus.ReadInbox("lead")
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(msgs, "", "  ")
		fmt.Println(string(raw))
		return nil
	}

	if len(a.history) == 0 {
		a.history = append(a.history, openai.Message{
			Role:    "system",
			Content: a.system,
		})
	}

	a.history = append(a.history, openai.Message{
		Role:    "user",
		Content: q,
	})

	for {
		//每次调用llm前先读取一次收件箱，如果有新消息就加到对话里，这样llm就能及时看到队友的回复
		inbox, err := a.bus.ReadInbox("lead")
		if err == nil && len(inbox) > 0 {
			raw, _ := json.MarshalIndent(inbox, "", "  ")
			a.history = append(a.history, openai.Message{
				Role:    "user",
				Content: "<inbox>" + string(raw) + "</inbox>",
			})
		}

		//调用llm
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

func (a *Agent) handleSpawnTeammate(arguments string) string {
	var in spawnTeammateInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}

	out, err := a.team.Spawn(in.Name, in.Role, in.Prompt)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleListTeammates(arguments string) string {
	args := strings.TrimSpace(arguments)
	if args != "" && args != "{}" {
		var dummy map[string]any
		if err := json.Unmarshal([]byte(arguments), &dummy); err != nil {
			return "Error: invalid tool input"
		}
	}
	return a.team.ListAll()
}

func (a *Agent) handleSendMessage(arguments string) string {
	var in sendMessageInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.bus.Send("lead", in.To, in.Content, in.MsgType, nil)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleReadInbox(arguments string) string {
	//无参工具调用 检查合法性
	args := strings.TrimSpace(arguments)
	if args != "" && args != "{}" {
		//不为空至少得是合法的JSON对象，否则可能是调用格式错误了
		var dummy map[string]any
		if err := json.Unmarshal([]byte(arguments), &dummy); err != nil {
			return "Error: invalid tool input"
		}
	}

	messages, err := a.bus.ReadInbox("lead")
	if err != nil {
		return "Error: " + err.Error()
	}
	raw, _ := json.MarshalIndent(messages, "", "  ")
	return string(raw)
}

func (a *Agent) handleBroadcast(arguments string) string {
	var in broadcastInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.bus.Broadcast("lead", in.Content, a.team.MemberNames())
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
