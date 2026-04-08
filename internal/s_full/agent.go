package s_full

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
	backgroundTimeout   = 120 * time.Second
	tokenThreshold      = 100000
	summaryMaxTokens    = 2000
	transcriptTailChars = 80000
	transcriptDirName   = ".transcripts"
	maxSubagentRounds   = 30
	maxTodoItems        = 20
	nagAfterRounds      = 3
	keepRecentResults   = 3
	minToolResultChars  = 100
	notificationMaxText = 500
	teamDirName         = ".team"
	inboxDirName        = "inbox"
	tasksDirName        = ".tasks"
	skillsDirName       = "skills"
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

type taskCreateInput struct {
	Subject     string `json:"subject"`
	Description string `json:"description,omitempty"`
}

type taskUpdateInput struct {
	TaskID          int    `json:"task_id"`
	Status          string `json:"status,omitempty"`
	AddBlockedBy    []int  `json:"add_blocked_by,omitempty"`
	RemoveBlockedBy []int  `json:"remove_blocked_by,omitempty"`
}

type taskGetInput struct {
	TaskID int `json:"task_id"`
}

type todoWriteInput struct {
	Items []todoItemInput `json:"items"`
}

type todoItemInput struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm"`
}

type taskToolInput struct {
	Prompt    string `json:"prompt"`
	AgentType string `json:"agent_type,omitempty"`
}

type loadSkillInput struct {
	Name string `json:"name"`
}

type compactInput struct {
	Focus string `json:"focus,omitempty"`
}

type backgroundRunInput struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type checkBackgroundInput struct {
	TaskID string `json:"task_id,omitempty"`
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

type todoItem struct {
	Content    string
	Status     string
	ActiveForm string
}

type TodoManager struct {
	items []todoItem
}

func (t *TodoManager) Update(items []todoItemInput) (string, error) {
	if len(items) > maxTodoItems {
		return "", fmt.Errorf("Max %d todos", maxTodoItems)
	}

	validated := make([]todoItem, 0, len(items))
	inProgress := 0
	for i, item := range items {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			return "", fmt.Errorf("Item %d: content required", i)
		}

		status := strings.ToLower(strings.TrimSpace(item.Status))
		switch status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("Item %d: invalid status '%s'", i, status)
		}

		activeForm := strings.TrimSpace(item.ActiveForm)
		if activeForm == "" {
			return "", fmt.Errorf("Item %d: activeForm required", i)
		}

		if status == "in_progress" {
			inProgress++
		}

		validated = append(validated, todoItem{
			Content:    content,
			Status:     status,
			ActiveForm: activeForm,
		})
	}

	if inProgress > 1 {
		return "", errors.New("Only one in_progress allowed")
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
		marker := "[?]"
		switch item.Status {
		case "pending":
			marker = "[ ]"
		case "in_progress":
			marker = "[>]"
		case "completed":
			marker = "[x]"
			completed++
		}

		suffix := ""
		if item.Status == "in_progress" {
			suffix = " <- " + item.ActiveForm
		}
		lines = append(lines, fmt.Sprintf("%s %s%s", marker, item.Content, suffix))
	}
	lines = append(lines, fmt.Sprintf("\n(%d/%d completed)", completed, len(t.items)))
	return strings.Join(lines, "\n")
}

func (t *TodoManager) HasOpenItems() bool {
	for _, item := range t.items {
		if item.Status != "completed" {
			return true
		}
	}
	return false
}

type Skill struct {
	Name        string
	Description string
	Tags        string
	Body        string
}

type SkillLoader struct {
	skills map[string]Skill
}

func NewSkillLoader(skillsDir string) *SkillLoader {
	loader := &SkillLoader{skills: map[string]Skill{}}

	_ = filepath.WalkDir(skillsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if strings.ToLower(d.Name()) != "skill.md" {
			return nil
		}

		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		meta, body := parseFrontmatter(string(raw))
		name := strings.TrimSpace(meta["name"])
		if name == "" {
			name = filepath.Base(filepath.Dir(path))
		}
		if name == "" {
			return nil
		}

		loader.skills[name] = Skill{
			Name:        name,
			Description: strings.TrimSpace(meta["description"]),
			Tags:        strings.TrimSpace(meta["tags"]),
			Body:        strings.TrimSpace(body),
		}
		return nil
	})

	return loader
}

func parseFrontmatter(text string) (map[string]string, string) {
	meta := map[string]string{}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return meta, text
	}

	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return meta, text
	}

	head := rest[:end]
	body := rest[end+len("\n---\n"):]
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)
		meta[key] = val
	}
	return meta, body
}

func (s *SkillLoader) names() []string {
	names := make([]string, 0, len(s.skills))
	for name := range s.skills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *SkillLoader) Descriptions() string {
	if len(s.skills) == 0 {
		return "(no skills)"
	}

	lines := make([]string, 0, len(s.skills))
	for _, name := range s.names() {
		skill := s.skills[name]
		desc := skill.Description
		if desc == "" {
			desc = "-"
		}
		line := fmt.Sprintf("  - %s: %s", name, desc)
		if skill.Tags != "" {
			line += " [" + skill.Tags + "]"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (s *SkillLoader) Content(name string) string {
	skill, ok := s.skills[name]
	if !ok {
		return fmt.Sprintf("Error: Unknown skill '%s'. Available: %s", name, strings.Join(s.names(), ", "))
	}
	return fmt.Sprintf("<skill name=%q>\n%s\n</skill>", skill.Name, skill.Body)
}

type backgroundTask struct {
	Status  string
	Result  string
	Command string
}

type backgroundNotification struct {
	TaskID string
	Status string
	Result string
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

func (bm *BackgroundManager) Run(command string, timeoutSec int) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return "Error: command is required"
	}
	if timeoutSec <= 0 {
		timeoutSec = int(backgroundTimeout.Seconds())
	}

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

	go bm.execute(taskID, command, time.Duration(timeoutSec)*time.Second)
	return fmt.Sprintf("Background task %s started: %s", taskID, preview(command, 80))
}

func (bm *BackgroundManager) Check(taskID string) string {
	taskID = strings.TrimSpace(taskID)

	bm.mu.Lock()
	defer bm.mu.Unlock()

	if taskID != "" {
		t, ok := bm.tasks[taskID]
		if !ok {
			return fmt.Sprintf("Unknown: %s", taskID)
		}
		result := strings.TrimSpace(t.Result)
		if result == "" {
			result = "(running)"
		}
		return fmt.Sprintf("[%s] %s", t.Status, result)
	}

	if len(bm.tasks) == 0 {
		return "No bg tasks."
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

func (bm *BackgroundManager) Drain() []backgroundNotification {
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

func (bm *BackgroundManager) execute(taskID, command string, timeout time.Duration) {
	output, timedOut, runErr := runShellCommand(bm.workDir, command, timeout)

	status := "completed"
	if timedOut {
		status = "error"
	}
	_ = runErr

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
		TaskID: taskID,
		Status: status,
		Result: truncate(output, notificationMaxText),
	})
	bm.mu.Unlock()
}

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

func dedupePositiveInts(xs []int) []int {
	if len(xs) == 0 {
		return []int{}
	}
	set := map[int]struct{}{}
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

func (tm *TeammateManager) clearDependencyLocked(completedID int) error {
	matches, err := filepath.Glob(filepath.Join(tm.tasksDir, "task_*.json"))
	if err != nil {
		return err
	}

	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var t taskBoardTask
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		if len(t.BlockedBy) == 0 {
			continue
		}

		next := make([]int, 0, len(t.BlockedBy))
		changed := false
		for _, x := range t.BlockedBy {
			if x == completedID {
				changed = true
				continue
			}
			next = append(next, x)
		}
		if !changed {
			continue
		}

		t.BlockedBy = dedupePositiveInts(next)
		out, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return err
		}
		out = append(out, '\n')
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (tm *TeammateManager) updateTask(taskID int, status string, addBlockedBy, removeBlockedBy []int) (string, error) {
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

	status = strings.TrimSpace(status)
	if status != "" {
		switch status {
		case "pending", "in_progress", "completed", "deleted":
		default:
			return "", fmt.Errorf("Invalid status: %s", status)
		}

		if status == "deleted" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
			return fmt.Sprintf("Task %d deleted", taskID), nil
		}

		t.Status = status
		if status == "completed" {
			if err := tm.clearDependencyLocked(taskID); err != nil {
				return "", err
			}
		}
	}

	if len(addBlockedBy) > 0 {
		t.BlockedBy = append(t.BlockedBy, addBlockedBy...)
		t.BlockedBy = dedupePositiveInts(t.BlockedBy)
	}
	if len(removeBlockedBy) > 0 {
		removeSet := map[int]struct{}{}
		for _, x := range removeBlockedBy {
			if x > 0 {
				removeSet[x] = struct{}{}
			}
		}
		next := make([]int, 0, len(t.BlockedBy))
		for _, x := range t.BlockedBy {
			if _, drop := removeSet[x]; !drop {
				next = append(next, x)
			}
		}
		t.BlockedBy = dedupePositiveInts(next)
	}

	out, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", err
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", err
	}
	return string(out), nil
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
				tm.setStatus(name, "shutdown")
				return
			}
			if len(resp.Choices) == 0 {
				tm.setStatus(name, "shutdown")
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
	client         *openai.Client
	modelID        string
	workDir        string
	system         string
	tools          []openai.ToolDef
	subTools       []openai.ToolDef
	subagentSystem string
	todos          TodoManager
	skills         *SkillLoader
	bg             *BackgroundManager
	bus            *MessageBus
	team           *TeammateManager
	tracker        *ProtocolTracker
	handlers       map[string]toolHandler
	history        []openai.Message
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
	skillLoader := NewSkillLoader(filepath.Join(workDir, skillsDirName))

	a := &Agent{
		client:  client,
		modelID: cfg.ModelID,
		workDir: workDir,
		system: fmt.Sprintf(
			"You are a coding agent at %s. Use tools to solve tasks.\nPrefer task_create/task_update/task_list for multi-step work. Use TodoWrite for short checklists.\nUse task for subagent delegation. Use load_skill for specialized knowledge.\nSkills:\n%s",
			workDir,
			skillLoader.Descriptions(),
		),
		subagentSystem: fmt.Sprintf(
			"You are a coding subagent at %s. Complete the given task, then summarize your findings.",
			workDir,
		),
		tools:    buildTools(),
		subTools: buildSubTools(),
		skills:   skillLoader,
		bg:       NewBackgroundManager(workDir),
		bus:      bus,
		team:     team,
		tracker:  tracker,
	}
	a.handlers = map[string]toolHandler{
		"bash":             a.handleBash,
		"read_file":        a.handleReadFile,
		"write_file":       a.handleWriteFile,
		"edit_file":        a.handleEditFile,
		"TodoWrite":        a.handleTodoWrite,
		"task":             a.handleTask,
		"load_skill":       a.handleLoadSkill,
		"compress":         a.handleCompress,
		"background_run":   a.handleBackgroundRun,
		"check_background": a.handleCheckBackground,
		"task_create":      a.handleTaskCreate,
		"task_get":         a.handleTaskGet,
		"task_update":      a.handleTaskUpdate,
		"task_list":        a.handleTaskList,
		"spawn_teammate":   a.handleSpawnTeammate,
		"list_teammates":   a.handleListTeammates,
		"send_message":     a.handleSendMessage,
		"read_inbox":       a.handleReadInbox,
		"broadcast":        a.handleBroadcast,
		"shutdown_request": a.handleShutdownRequest,
		"plan_approval":    a.handlePlanApproval,
		"idle":             a.handleIdle,
		"claim_task":       a.handleClaimTask,
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
	}
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
			Name:        "TodoWrite",
			Description: "Update task tracking list.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"items": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"content":    map[string]any{"type": "string"},
								"status":     map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
								"activeForm": map[string]any{"type": "string"},
							},
							"required": []string{"content", "status", "activeForm"},
						},
					},
				},
				"required": []string{"items"},
			},
		},
		{
			Name:        "task",
			Description: "Spawn a subagent for isolated exploration or work.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string"},
					"agent_type": map[string]any{
						"type": "string",
						"enum": []string{"Explore", "general-purpose"},
					},
				},
				"required": []string{"prompt"},
			},
		},
		{
			Name:        "load_skill",
			Description: "Load specialized knowledge by name.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "compress",
			Description: "Manually compress conversation context.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "background_run",
			Description: "Run command in background thread.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
					"timeout": map[string]any{"type": "integer"},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "check_background",
			Description: "Check background task status.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "task_create",
			Description: "Create a persistent file task.",
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
			Name:        "task_get",
			Description: "Get task details by ID.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "integer"},
				},
				"required": []string{"task_id"},
			},
		},
		{
			Name:        "task_update",
			Description: "Update task status or dependencies.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "integer"},
					"status": map[string]any{
						"type": "string",
						"enum": []string{"pending", "in_progress", "completed", "deleted"},
					},
					"add_blocked_by": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "integer"},
					},
					"remove_blocked_by": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "integer"},
					},
				},
				"required": []string{"task_id"},
			},
		},
		{
			Name:        "task_list",
			Description: "List all tasks.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "spawn_teammate",
			Description: "Spawn a persistent autonomous teammate.",
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
			Description: "List all teammates.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "send_message",
			Description: "Send a message to a teammate.",
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
			Description: "Send message to all teammates.",
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
			Description: "Enter idle state.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "claim_task",
			Description: "Claim a task from the board.",
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
	case "/compact":
		if len(a.history) == 0 {
			return nil
		}
		fmt.Println("[manual compact via /compact]")
		return a.autoCompact("")
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
		out, err := a.team.listTasks()
		if err != nil {
			return err
		}
		fmt.Println(out)
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

	roundsWithoutTodo := 0
	for {
		a.microCompact()
		if estimateTokens(a.history) > tokenThreshold {
			fmt.Println("[auto-compact triggered]")
			if err := a.autoCompact(""); err != nil {
				return err
			}
		}

		notifs := a.bg.Drain()
		if len(notifs) > 0 {
			lines := make([]string, 0, len(notifs))
			for _, n := range notifs {
				lines = append(lines, fmt.Sprintf("[bg:%s] %s: %s", n.TaskID, n.Status, n.Result))
			}
			a.history = append(a.history, openai.Message{
				Role:    "user",
				Content: "<background-results>\n" + strings.Join(lines, "\n") + "\n</background-results>",
			})
		}

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

		usedTodo := false
		manualCompact := false
		manualFocus := ""
		toolMsgs := make([]openai.Message, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			if tc.Function.Name == "compress" {
				manualCompact = true
				manualFocus = parseCompactFocus(tc.Function.Arguments)
			}

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

			if tc.Function.Name == "TodoWrite" {
				usedTodo = true
			}
		}

		if usedTodo {
			roundsWithoutTodo = 0
		} else {
			roundsWithoutTodo++
		}
		a.history = append(a.history, toolMsgs...)

		if a.todos.HasOpenItems() && roundsWithoutTodo >= nagAfterRounds {
			a.history = append(a.history, openai.Message{
				Role:    "user",
				Content: "<reminder>Update your todos.</reminder>",
			})
		}

		if manualCompact {
			fmt.Println("[manual compact]")
			if err := a.autoCompact(manualFocus); err != nil {
				return err
			}
			return nil
		}
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

func (a *Agent) handleTodoWrite(arguments string) string {
	var in todoWriteInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}

	out, err := a.todos.Update(in.Items)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func filterSubTools(agentType string, all []openai.ToolDef) []openai.ToolDef {
	agentType = strings.TrimSpace(agentType)
	if agentType == "" || strings.EqualFold(agentType, "Explore") {
		out := make([]openai.ToolDef, 0, 2)
		for _, t := range all {
			if t.Name == "bash" || t.Name == "read_file" {
				out = append(out, t)
			}
		}
		return out
	}
	return all
}

func (a *Agent) runSubagent(prompt, agentType string) string {
	subHistory := []openai.Message{
		{Role: "system", Content: a.subagentSystem},
		{Role: "user", Content: prompt},
	}
	subTools := filterSubTools(agentType, a.subTools)

	for i := 0; i < maxSubagentRounds; i++ {
		resp, err := a.client.ChatCompletions(context.Background(), a.modelID, subHistory, subTools, maxTokensPerCall)
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
			text = strings.TrimSpace(text)
			if text == "" {
				return "(no summary)"
			}
			return text
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
			default:
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

func (a *Agent) handleTask(arguments string) string {
	var in taskToolInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return "Error: prompt is required"
	}
	agentType := strings.TrimSpace(in.AgentType)
	if agentType == "" {
		agentType = "Explore"
	}

	fmt.Printf("> task (%s): %s\n", agentType, preview(prompt, 80))
	return a.runSubagent(prompt, agentType)
}

func (a *Agent) handleLoadSkill(arguments string) string {
	var in loadSkillInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	return a.skills.Content(strings.TrimSpace(in.Name))
}

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

func (a *Agent) handleCompress(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return "Compressing..."
	}
	var in compactInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	return "Compressing..."
}

func (a *Agent) handleBackgroundRun(arguments string) string {
	var in backgroundRunInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	return a.bg.Run(in.Command, in.Timeout)
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

func (a *Agent) handleTaskUpdate(arguments string) string {
	var in taskUpdateInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.team.updateTask(in.TaskID, in.Status, in.AddBlockedBy, in.RemoveBlockedBy)
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

func (a *Agent) microCompact() {
	toolNameByCallID := make(map[string]string)
	for _, msg := range a.history {
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
	if len(toolIndexes) <= keepRecentResults {
		return
	}

	toCompact := toolIndexes[:len(toolIndexes)-keepRecentResults]
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

func runShellCommand(workDir, command string, timeout time.Duration) (output string, timedOut bool, runErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
		return "", true, nil
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
