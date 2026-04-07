package s11

import (
	"bufio"
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
	teamDirName         = ".team"
	inboxDirName        = "inbox"
	tasksDirName        = ".tasks"
	teammateMaxRounds   = 50
	teammateHTTPTimeout = 180 * time.Second
	pollInterval        = 5 * time.Second
	idleTimeout         = 60 * time.Second
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

type taskCreateInput struct {
	Subject     string `json:"subject"`
	Description string `json:"description,omitempty"`
}

type taskGetInput struct {
	TaskID int `json:"task_id"`
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

// shutdownRequestInput is the input schema for the shutdown_request tool.
type shutdownRequestInput struct {
	Teammate string `json:"teammate"`
}

type shutdownStatusInput struct {
	RequestID string `json:"request_id"`
}

// planReviewInput is the input schema for the plan_approval tool.
type planReviewInput struct {
	RequestID string `json:"request_id"`
	Approve   bool   `json:"approve"`
	Feedback  string `json:"feedback,omitempty"`
}

// teammateShutdownResponseInput is the input schema for teammates to respond to shutdown requests.
type teammateShutdownResponseInput struct {
	RequestID string `json:"request_id"`
	Approve   bool   `json:"approve"`
	Reason    string `json:"reason,omitempty"`
}

// teammatePlanApprovalInput is the input schema for teammates to submit plans for approval.
type teammatePlanApprovalInput struct {
	Plan string `json:"plan"`
}

// claimTaskInput is the input schema for the claim_task tool.
type claimTaskInput struct {
	TaskID int `json:"task_id"`
}

// taskBoardTask represents a task on the task board.
// Tasks are stored as JSON files in the tasks directory.
type taskBoardTask struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
	BlockedBy   []int  `json:"blockedBy"`
	Owner       string `json:"owner"`
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

// 寰€鏀朵欢绠眎nbox涓姞閿佸啓閭欢
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

// 璇诲彇鏌愪釜鎴愬憳鏀朵欢绠卞苟娓呯┖
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

	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		return nil, err
	}
	return messages, nil
}

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

type ProtocolTracker struct {
	mu sync.Mutex

	shutdownRequests map[string]map[string]any
	planRequests     map[string]map[string]any
}

func NewProtocolTracker() *ProtocolTracker {
	return &ProtocolTracker{
		shutdownRequests: map[string]map[string]any{},
		planRequests:     map[string]map[string]any{},
	}
}

// 生成4字节的随机16进制字符串作为请求ID，确保在shutdownRequests和planRequests中唯一。
func (pt *ProtocolTracker) nextRequestIDLocked() string {
	for {
		id := randomHex(4)
		if _, ok := pt.shutdownRequests[id]; ok {
			continue
		}
		if _, ok := pt.planRequests[id]; ok {
			continue
		}
		return id
	}
}

// 在tracker中创建一个新的shutdown请求，状态初始为pending，返回请求ID。
func (pt *ProtocolTracker) CreateShutdownRequest(teammate string) string {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	reqID := pt.nextRequestIDLocked()
	pt.shutdownRequests[reqID] = map[string]any{
		"target": teammate,
		"status": "pending",
	}
	return reqID
}

// 更新shutdown请求的状态为approved或rejected，返回是否成功找到请求ID。
func (pt *ProtocolTracker) UpdateShutdownResponse(requestID string, approve bool) bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	req, ok := pt.shutdownRequests[requestID]
	if !ok {
		return false
	}
	if approve {
		req["status"] = "approved"
	} else {
		req["status"] = "rejected"
	}
	return true
}

// 根据请求ID获取shutdown请求的当前状态和目标队友，返回一个包含target和status的map。如果请求ID不存在，返回一个包含error的map。
func (pt *ProtocolTracker) GetShutdownRequest(requestID string) map[string]any {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	req, ok := pt.shutdownRequests[requestID]
	if !ok {
		return map[string]any{"error": "not found"}
	}
	return cloneMap(req)
}

// 在tracker中创建一个新的plan请求，状态初始为pending，返回请求ID。
func (pt *ProtocolTracker) CreatePlanRequest(sender, plan string) string {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	reqID := pt.nextRequestIDLocked()
	pt.planRequests[reqID] = map[string]any{
		"from":   sender,
		"plan":   plan,
		"status": "pending",
	}
	return reqID
}

// 更新plan请求的状态为approved或rejected，返回更新后的请求信息和是否成功找到请求ID。
func (pt *ProtocolTracker) ReviewPlanRequest(requestID string, approve bool) (map[string]any, bool) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	req, ok := pt.planRequests[requestID]
	if !ok {
		return nil, false
	}
	if approve {
		req["status"] = "approved"
	} else {
		req["status"] = "rejected"
	}
	return cloneMap(req), true
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
	workDir  string
	tasksDir string
	client   *openai.Client
	modelID  string
	bus      *MessageBus
	tracker  *ProtocolTracker

	configPath string

	mu      sync.Mutex
	config  TeamConfig
	threads map[string]struct{}
	claimMu sync.Mutex
}

func NewTeammateManager(
	workDir, teamDir string,
	client *openai.Client,
	modelID string,
	bus *MessageBus,
	tracker *ProtocolTracker,
) (*TeammateManager, error) {
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		return nil, err
	}

	tm := &TeammateManager{
		workDir:    workDir,
		tasksDir:   filepath.Join(workDir, tasksDirName),
		client:     client,
		modelID:    modelID,
		bus:        bus,
		tracker:    tracker,
		configPath: filepath.Join(teamDir, "config.json"),
		threads:    map[string]struct{}{},
	}
	if err := os.MkdirAll(tm.tasksDir, 0o755); err != nil {
		return nil, err
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

func (tm *TeammateManager) setStatus(name, status string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if _, member := tm.findMemberLocked(name); member != nil {
		member.Status = status
		_ = tm.saveConfigLocked()
	}
}

// scans the tasks directory for tasks that are pending, have no owner, and are not blocked by other tasks.
// It returns a list of such tasks sorted by ID.
func (tm *TeammateManager) scanUnclaimedTasks() ([]taskBoardTask, error) {
	if err := os.MkdirAll(tm.tasksDir, 0o755); err != nil {
		return nil, err
	}

	matches, err := filepath.Glob(filepath.Join(tm.tasksDir, "task_*.json"))
	if err != nil {
		return nil, err
	}

	out := make([]taskBoardTask, 0)
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var t taskBoardTask
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		if t.Status == "pending" && strings.TrimSpace(t.Owner) == "" && len(t.BlockedBy) == 0 {
			out = append(out, t)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func taskBoardTaskJSON(t taskBoardTask) (string, error) {
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (tm *TeammateManager) listAllTasksLocked() ([]taskBoardTask, error) {
	matches, err := filepath.Glob(filepath.Join(tm.tasksDir, "task_*.json"))
	if err != nil {
		return nil, err
	}

	tasks := make([]taskBoardTask, 0, len(matches))
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var t taskBoardTask
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		tasks = append(tasks, t)
	}

	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].ID < tasks[j].ID
	})
	return tasks, nil
}

func (tm *TeammateManager) createTask(subject, description string) (string, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", errors.New("subject is required")
	}

	tm.claimMu.Lock()
	defer tm.claimMu.Unlock()

	if err := os.MkdirAll(tm.tasksDir, 0o755); err != nil {
		return "", err
	}

	tasks, err := tm.listAllTasksLocked()
	if err != nil {
		return "", err
	}

	nextID := 1
	for _, t := range tasks {
		if t.ID >= nextID {
			nextID = t.ID + 1
		}
	}

	task := taskBoardTask{
		ID:          nextID,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		BlockedBy:   []int{},
		Owner:       "",
	}

	path := filepath.Join(tm.tasksDir, fmt.Sprintf("task_%d.json", task.ID))
	out, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return "", err
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", err
	}

	return taskBoardTaskJSON(task)
}

func (tm *TeammateManager) listTasks() (string, error) {
	tm.claimMu.Lock()
	defer tm.claimMu.Unlock()

	if err := os.MkdirAll(tm.tasksDir, 0o755); err != nil {
		return "", err
	}

	tasks, err := tm.listAllTasksLocked()
	if err != nil {
		return "", err
	}
	if len(tasks) == 0 {
		return "No tasks.", nil
	}

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

		owner := ""
		if strings.TrimSpace(t.Owner) != "" {
			owner = " @" + t.Owner
		}

		blocked := ""
		if len(t.BlockedBy) > 0 {
			blocked = fmt.Sprintf(" (blocked by: %v)", t.BlockedBy)
		}

		lines = append(lines, fmt.Sprintf("%s #%d: %s%s%s", marker, t.ID, t.Subject, owner, blocked))
	}
	return strings.Join(lines, "\n"), nil
}

func (tm *TeammateManager) getTask(taskID int) (string, error) {
	if taskID <= 0 {
		return "", errors.New("task_id is required")
	}

	tm.claimMu.Lock()
	defer tm.claimMu.Unlock()

	path := filepath.Join(tm.tasksDir, fmt.Sprintf("task_%d.json", taskID))
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("Task %d not found", taskID)
		}
		return "", err
	}

	var t taskBoardTask
	if err := json.Unmarshal(raw, &t); err != nil {
		return "", err
	}
	return taskBoardTaskJSON(t)
}

// claimTask attempts to claim a task for a teammate.
// It checks if the task exists, is pending, has no owner, and is not blocked.
func (tm *TeammateManager) claimTask(taskID int, owner string) string {
	if taskID <= 0 {
		return "Error: task_id is required"
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return "Error: owner is required"
	}

	tm.claimMu.Lock()
	defer tm.claimMu.Unlock()

	path := filepath.Join(tm.tasksDir, fmt.Sprintf("task_%d.json", taskID))
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("Error: Task %d not found", taskID)
		}
		return "Error: " + err.Error()
	}

	var t taskBoardTask
	if err := json.Unmarshal(raw, &t); err != nil {
		return "Error: " + err.Error()
	}
	if strings.TrimSpace(t.Owner) != "" {
		return fmt.Sprintf("Error: Task %d has already been claimed by %s", taskID, t.Owner)
	}
	if t.Status != "pending" {
		return fmt.Sprintf("Error: Task %d cannot be claimed because its status is '%s'", taskID, t.Status)
	}
	if len(t.BlockedBy) > 0 {
		return fmt.Sprintf("Error: Task %d is blocked by other task(s) and cannot be claimed yet", taskID)
	}

	t.Owner = owner
	t.Status = "in_progress"

	out, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "Error: " + err.Error()
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Claimed task #%d for %s", taskID, owner)
}

// makeIdentityBlock creates a system message block that reaffirms the teammate's identity, role, and team.
func makeIdentityBlock(name, role, teamName string) openai.Message {
	return openai.Message{
		Role: "user",
		Content: fmt.Sprintf(
			"<identity>You are '%s', role: %s, team: %s. Continue your work.</identity>",
			name, role, teamName,
		),
	}
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
	tm.mu.Lock()
	teamName := tm.config.TeamName
	tm.mu.Unlock()
	if strings.TrimSpace(teamName) == "" {
		teamName = "default"
	}

	sysPrompt := fmt.Sprintf(
		"You are '%s', role: %s, team: %s, at %s. Use idle tool when you have no more work. You will auto-claim new tasks.",
		name, role, teamName, tm.workDir,
	)

	messages := []openai.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: prompt},
	}
	tools := tm.teammateTools()

	for {
		for i := 0; i < teammateMaxRounds; i++ {
			inbox, err := tm.bus.ReadInbox(name)
			if err == nil && len(inbox) > 0 {
				// Prepend inbox messages to the conversation history
				for _, msg := range inbox {
					// Process shutdown requests immediately
					msgType, _ := msg["type"].(string)
					if msgType == "shutdown_request" {
						tm.setStatus(name, "shutdown")
						return
					}
					raw, _ := json.Marshal(msg)
					messages = append(messages, openai.Message{
						Role:    "user",
						Content: string(raw),
					})
				}
			}

			resp, err := tm.client.ChatCompletions(context.Background(), tm.modelID, messages, tools, maxTokensPerCall)
			if err != nil {
				tm.setStatus(name, "idle")
				return
			}
			if len(resp.Choices) == 0 {
				tm.setStatus(name, "idle")
				return
			}

			msg := resp.Choices[0].Message
			messages = append(messages, msg)
			if len(msg.ToolCalls) == 0 {
				break
			}

			idleRequested := false
			toolMsgs := make([]openai.Message, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				output := ""
				if tc.Function.Name == "idle" {
					idleRequested = true
					output = "Entering idle phase. Will poll for new tasks."
				} else {
					output = tm.execTeammateTool(name, tc.Function.Name, tc.Function.Arguments)
				}
				fmt.Printf("  [%s] %s: %s\n", name, tc.Function.Name, preview(output, 120))
				toolMsgs = append(toolMsgs, openai.Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    output,
				})
			}
			messages = append(messages, toolMsgs...)
			if idleRequested {
				break
			}
		}

		tm.setStatus(name, "idle")

		resume := false
		//                 60/5
		polls := int(idleTimeout / pollInterval)
		if polls <= 0 {
			polls = 1
		}

		for i := 0; i < polls; i++ {
			time.Sleep(pollInterval)

			inbox, err := tm.bus.ReadInbox(name)
			if err == nil && len(inbox) > 0 {
				for _, msg := range inbox {
					msgType, _ := msg["type"].(string)
					if msgType == "shutdown_request" {
						tm.setStatus(name, "shutdown")
						return
					}
					raw, _ := json.Marshal(msg)
					messages = append(messages, openai.Message{
						Role:    "user",
						Content: string(raw),
					})
				}
				resume = true
				//break pool loop and resume work immediately after processing inbox messages
				break
			}

			unclaimed, err := tm.scanUnclaimedTasks()
			if err != nil || len(unclaimed) == 0 {
				continue
			}

			task := unclaimed[0]
			result := tm.claimTask(task.ID, name)
			if strings.HasPrefix(result, "Error:") {
				continue
			}

			taskPrompt := fmt.Sprintf(
				"<auto-claimed>Task #%d: %s\n%s</auto-claimed>",
				task.ID,
				task.Subject,
				task.Description,
			)
			// compressed, prepend an identity block to reaffirm their identity and role.
			if len(messages) <= 3 {
				prefix := []openai.Message{
					makeIdentityBlock(name, role, teamName),
					{Role: "assistant", Content: fmt.Sprintf("I am %s. Continuing.", name)},
				}
				messages = append(prefix, messages...)
			}
			messages = append(messages,
				openai.Message{Role: "user", Content: taskPrompt},
				openai.Message{Role: "assistant", Content: fmt.Sprintf("Claimed task #%d. Working on it.", task.ID)},
			)
			resume = true
			// break pool loop and resume work immediately after claiming a task
			break
		}

		// if no messages received and no tasks claimed during idle period,
		// teammate will continue to idle until a new message arrives or a task becomes available.
		if !resume {
			tm.setStatus(name, "shutdown")
			return
		}
		tm.setStatus(name, "working")
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
	case "shutdown_response":
		var in teammateShutdownResponseInput
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "Error: invalid tool input"
		}
		if strings.TrimSpace(in.RequestID) == "" {
			return "Error: request_id is required"
		}
		tm.tracker.UpdateShutdownResponse(in.RequestID, in.Approve)
		_, err := tm.bus.Send(sender, "lead", in.Reason, "shutdown_response", map[string]any{
			"request_id": in.RequestID,
			"approve":    in.Approve,
		})
		if err != nil {
			return "Error: " + err.Error()
		}
		if in.Approve {
			return "Shutdown approved"
		}
		return "Shutdown rejected"
	case "plan_approval":
		var in teammatePlanApprovalInput
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "Error: invalid tool input"
		}
		in.Plan = strings.TrimSpace(in.Plan)
		if in.Plan == "" {
			return "Error: plan is required"
		}
		reqID := tm.tracker.CreatePlanRequest(sender, in.Plan)
		_, err := tm.bus.Send(sender, "lead", in.Plan, "plan_approval_response", map[string]any{
			"request_id": reqID,
			"plan":       in.Plan,
		})
		if err != nil {
			return "Error: " + err.Error()
		}
		return fmt.Sprintf("Plan submitted (request_id=%s). Waiting for approval.", reqID)
	case "claim_task":
		var in claimTaskInput
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "Error: invalid tool input"
		}
		return tm.claimTask(in.TaskID, sender)
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
		{
			Name:        "shutdown_response",
			Description: "Respond to a shutdown request. Approve to shut down, reject to keep working.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
					"approve":    map[string]any{"type": "boolean"},
					"reason":     map[string]any{"type": "string"},
				},
				"required": []string{"request_id", "approve"},
			},
		},
		{
			Name:        "plan_approval",
			Description: "Submit a plan for lead approval. Provide plan text.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"plan": map[string]any{"type": "string"},
				},
				"required": []string{"plan"},
			},
		},
		{
			Name:        "idle",
			Description: "Signal that you have no more work. Enters idle polling phase.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "claim_task",
			Description: "Claim a task from the task board by ID.",
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
	tracker  *ProtocolTracker
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

	tracker := NewProtocolTracker()
	client := openai.NewClient(cfg.APIKey, cfg.BaseURL, teammateHTTPTimeout)
	team, err := NewTeammateManager(workDir, teamDir, client, cfg.ModelID, bus, tracker)
	if err != nil {
		return nil, err
	}

	a := &Agent{
		client:  client,
		modelID: cfg.ModelID,
		workDir: workDir,
		system: fmt.Sprintf(
			"You are a team lead at %s. Teammates are autonomous -- they find work themselves.",
			workDir,
		),
		tools:   buildTools(),
		bus:     bus,
		team:    team,
		tracker: tracker,
	}
	a.handlers = map[string]toolHandler{
		"bash":              a.handleBash,
		"read_file":         a.handleReadFile,
		"write_file":        a.handleWriteFile,
		"edit_file":         a.handleEditFile,
		"task_create":       a.handleTaskCreate,
		"task_list":         a.handleTaskList,
		"task_get":          a.handleTaskGet,
		"spawn_teammate":    a.handleSpawnTeammate,
		"list_teammates":    a.handleListTeammates,
		"send_message":      a.handleSendMessage,
		"read_inbox":        a.handleReadInbox,
		"broadcast":         a.handleBroadcast,
		"shutdown_request":  a.handleShutdownRequest,
		"shutdown_response": a.handleShutdownResponse,
		"plan_approval":     a.handlePlanApproval,
		"idle":              a.handleIdle,
		"claim_task":        a.handleClaimTask,
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
			Description: "Create a new task on the board.",
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
			Name:        "task_list",
			Description: "List all tasks on the board.",
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
		{
			Name:        "spawn_teammate",
			Description: "Spawn an autonomous teammate.",
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
		{
			Name:        "shutdown_request",
			Description: "Request a teammate to shut down.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"teammate": map[string]any{"type": "string"},
				},
				"required": []string{"teammate"},
			},
		},
		{
			Name:        "shutdown_response",
			Description: "Check shutdown request status.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
				},
				"required": []string{"request_id"},
			},
		},
		{
			Name:        "plan_approval",
			Description: "Approve or reject a teammate's plan.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
					"approve":    map[string]any{"type": "boolean"},
					"feedback":   map[string]any{"type": "string"},
				},
				"required": []string{"request_id", "approve"},
			},
		},
		{
			Name:        "idle",
			Description: "Enter idle state (for lead -- rarely used).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "claim_task",
			Description: "Claim a task from the board by ID.",
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
	case "/tasks":
		fmt.Println(a.renderTaskBoard())
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
		inbox, err := a.bus.ReadInbox("lead")
		if err == nil && len(inbox) > 0 {
			raw, _ := json.MarshalIndent(inbox, "", "  ")
			a.history = append(a.history, openai.Message{
				Role:    "user",
				Content: "<inbox>" + string(raw) + "</inbox>",
			})
		}

		//璋冪敤llm
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

	out, err := a.team.createTask(in.Subject, in.Description)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleTaskList(arguments string) string {
	args := strings.TrimSpace(arguments)
	if args != "" && args != "{}" {
		var dummy map[string]any
		if err := json.Unmarshal([]byte(arguments), &dummy); err != nil {
			return "Error: invalid tool input"
		}
	}

	out, err := a.team.listTasks()
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

	out, err := a.team.getTask(in.TaskID)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
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
	args := strings.TrimSpace(arguments)
	if args != "" && args != "{}" {
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

func (a *Agent) handleIdle(arguments string) string {
	args := strings.TrimSpace(arguments)
	if args != "" && args != "{}" {
		var dummy map[string]any
		if err := json.Unmarshal([]byte(arguments), &dummy); err != nil {
			return "Error: invalid tool input"
		}
	}
	return "Lead does not idle."
}

func (a *Agent) handleClaimTask(arguments string) string {
	var in claimTaskInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	return a.team.claimTask(in.TaskID, "lead")
}

// 创建关闭请求，并发送给指定队友
func (a *Agent) handleShutdownRequest(arguments string) string {
	var in shutdownRequestInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	in.Teammate = strings.TrimSpace(in.Teammate)
	if in.Teammate == "" {
		return "Error: teammate is required"
	}

	reqID := a.tracker.CreateShutdownRequest(in.Teammate)
	_, err := a.bus.Send(
		"lead",
		in.Teammate,
		"Please shut down gracefully.", //content
		"shutdown_request",             //msg_type
		map[string]any{"request_id": reqID},
	)
	if err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Shutdown request %s sent to '%s' (status: pending)", reqID, in.Teammate)
}

func (a *Agent) handleShutdownResponse(arguments string) string {
	var in shutdownStatusInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	in.RequestID = strings.TrimSpace(in.RequestID)
	if in.RequestID == "" {
		return "Error: request_id is required"
	}

	raw, err := json.Marshal(a.tracker.GetShutdownRequest(in.RequestID))
	if err != nil {
		return "Error: " + err.Error()
	}
	return string(raw)
}

// 处理计划审批，更新状态并发送反馈给队友
func (a *Agent) handlePlanApproval(arguments string) string {
	var in planReviewInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	in.RequestID = strings.TrimSpace(in.RequestID)
	if in.RequestID == "" {
		return "Error: request_id is required"
	}

	req, ok := a.tracker.ReviewPlanRequest(in.RequestID, in.Approve)
	if !ok {
		return fmt.Sprintf("Error: Unknown plan request_id '%s'", in.RequestID)
	}
	from, _ := req["from"].(string)
	status, _ := req["status"].(string)
	if strings.TrimSpace(from) == "" {
		return "Error: plan request has no sender"
	}

	_, err := a.bus.Send(
		"lead",
		from,
		in.Feedback,
		"plan_approval_response",
		map[string]any{
			"request_id": in.RequestID,
			"approve":    in.Approve,
			"feedback":   in.Feedback,
		},
	)
	if err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Plan %s for '%s'", status, from)
}

// 渲染任务看板，显示所有任务及其状态，并标记未被认领的任务
func (a *Agent) renderTaskBoard() string {
	tasks, err := a.team.scanUnclaimedTasks()
	if err != nil {
		return "Error: " + err.Error()
	}

	matches, err := filepath.Glob(filepath.Join(a.team.tasksDir, "task_*.json"))
	if err != nil {
		return "Error: " + err.Error()
	}
	if len(matches) == 0 {
		return "No tasks."
	}

	all := make([]taskBoardTask, 0, len(matches))
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var t taskBoardTask
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		all = append(all, t)
	}
	if len(all) == 0 {
		return "No tasks."
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].ID < all[j].ID
	})

	unclaimedSet := map[int]struct{}{}
	for _, t := range tasks {
		unclaimedSet[t.ID] = struct{}{}
	}

	lines := make([]string, 0, len(all))
	for _, t := range all {
		marker := "[?]"
		switch t.Status {
		case "pending":
			marker = "[ ]"
		case "in_progress":
			marker = "[>]"
		case "completed":
			marker = "[x]"
		}

		owner := ""
		if strings.TrimSpace(t.Owner) != "" {
			owner = " @" + t.Owner
		}
		extra := ""
		if _, ok := unclaimedSet[t.ID]; ok {
			extra = " (unclaimed)"
		}
		lines = append(lines, fmt.Sprintf("%s #%d: %s%s%s", marker, t.ID, t.Subject, owner, extra))
	}
	return strings.Join(lines, "\n")
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
		// Force UTF-8 output on Windows to avoid mojibake when reading CombinedOutput.
		script := strings.Join([]string{
			"$OutputEncoding = [System.Text.UTF8Encoding]::new($false)",
			"[Console]::InputEncoding = [System.Text.UTF8Encoding]::new($false)",
			"[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)",
			"chcp 65001 > $null",
			command,
		}, "; ")
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
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

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
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
