package s12

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go-pilot/internal/shared/envutil"
	"go-pilot/internal/shared/openai"
)

const (
	maxOutputChars         = 50000
	maxTokensPerCall       = 8000
	commandTimeout         = 120 * time.Second
	worktreeCommandTimeout = 300 * time.Second
	tasksDirName           = ".tasks"
	worktreesDirName       = ".worktrees"
	worktreeIndexName      = "index.json"
	worktreeEventsFileName = "events.jsonl"
	openAITimeout          = 180 * time.Second
	repoDetectTimeout      = 10 * time.Second
)

var worktreeNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)

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
	TaskID int     `json:"task_id"`
	Status string  `json:"status,omitempty"`
	Owner  *string `json:"owner,omitempty"`
}

type taskGetInput struct {
	TaskID int `json:"task_id"`
}

type taskBindWorktreeInput struct {
	TaskID   int    `json:"task_id"`
	Worktree string `json:"worktree"`
	Owner    string `json:"owner,omitempty"`
}

type worktreeCreateInput struct {
	Name    string `json:"name"`
	TaskID  *int   `json:"task_id,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
}

type worktreeNameInput struct {
	Name string `json:"name"`
}

type worktreeRunInput struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

type worktreeRemoveInput struct {
	Name         string `json:"name"`
	Force        bool   `json:"force,omitempty"`
	CompleteTask bool   `json:"complete_task,omitempty"`
}

type worktreeEventsInput struct {
	Limit int `json:"limit,omitempty"`
}

type toolHandler func(arguments string) string

type Task struct {
	ID          int     `json:"id"`
	Subject     string  `json:"subject"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	BlockedBy   []int   `json:"blockedBy"`
	Owner       string  `json:"owner"`
	Worktree    string  `json:"worktree"`
	CreatedAt   float64 `json:"created_at"`
	UpdatedAt   float64 `json:"updated_at"`
}

type TaskManager struct {
	dir    string
	nextID int
	mu     sync.Mutex
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

func normalizeTask(t *Task) {
	if strings.TrimSpace(t.Status) == "" {
		t.Status = "pending"
	}
	if t.BlockedBy == nil {
		t.BlockedBy = []int{}
	}
}

func nowUnixFloat() float64 {
	return float64(time.Now().UnixNano()) / 1e9
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
	normalizeTask(&t)
	return t, nil
}

func (tm *TaskManager) save(t Task) error {
	normalizeTask(&t)
	path := tm.taskPath(t.ID)
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

func taskJSON(t Task) string {
	normalizeTask(&t)
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

	tm.mu.Lock()
	defer tm.mu.Unlock()

	now := nowUnixFloat()
	t := Task{
		ID:          tm.nextID,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		BlockedBy:   []int{},
		Owner:       "",
		Worktree:    "",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := tm.save(t); err != nil {
		return "", err
	}
	tm.nextID++
	return taskJSON(t), nil
}

func (tm *TaskManager) Get(id int) (string, error) {
	if id <= 0 {
		return "", errors.New("task_id is required")
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	t, err := tm.load(id)
	if err != nil {
		return "", err
	}
	return taskJSON(t), nil
}

func (tm *TaskManager) Exists(id int) bool {
	if id <= 0 {
		return false
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	_, err := os.Stat(tm.taskPath(id))
	return err == nil
}

func (tm *TaskManager) UpdateStatusOwner(id int, status string, owner *string) (string, error) {
	if id <= 0 {
		return "", errors.New("task_id is required")
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

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
	}

	if owner != nil {
		t.Owner = strings.TrimSpace(*owner)
	}
	t.UpdatedAt = nowUnixFloat()

	if err := tm.save(t); err != nil {
		return "", err
	}
	return taskJSON(t), nil
}

func (tm *TaskManager) BindWorktree(taskID int, worktree, owner string) (string, error) {
	if taskID <= 0 {
		return "", errors.New("task_id is required")
	}
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return "", errors.New("worktree is required")
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	t, err := tm.load(taskID)
	if err != nil {
		return "", err
	}
	t.Worktree = worktree
	owner = strings.TrimSpace(owner)
	if owner != "" {
		t.Owner = owner
	}
	if t.Status == "pending" {
		t.Status = "in_progress"
	}
	t.UpdatedAt = nowUnixFloat()

	if err := tm.save(t); err != nil {
		return "", err
	}
	return taskJSON(t), nil
}

func (tm *TaskManager) UnbindWorktree(taskID int) (string, error) {
	if taskID <= 0 {
		return "", errors.New("task_id is required")
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	t, err := tm.load(taskID)
	if err != nil {
		return "", err
	}
	t.Worktree = ""
	t.UpdatedAt = nowUnixFloat()

	if err := tm.save(t); err != nil {
		return "", err
	}
	return taskJSON(t), nil
}

func (tm *TaskManager) ListAll() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

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

		owner := ""
		if strings.TrimSpace(t.Owner) != "" {
			owner = " owner=" + t.Owner
		}
		worktree := ""
		if strings.TrimSpace(t.Worktree) != "" {
			worktree = " wt=" + t.Worktree
		}
		lines = append(lines, fmt.Sprintf("%s #%d: %s%s%s", marker, t.ID, t.Subject, owner, worktree))
	}
	return strings.Join(lines, "\n"), nil
}

type EventBus struct {
	path string
	mu   sync.Mutex
}

func NewEventBus(path string) (*EventBus, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	return &EventBus{path: path}, nil
}

func (eb *EventBus) Emit(event string, task map[string]any, worktree map[string]any, errText string) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	payload := map[string]any{
		"event":    event,
		"ts":       nowUnixFloat(),
		"task":     map[string]any{},
		"worktree": map[string]any{},
	}
	if task != nil {
		payload["task"] = task
	}
	if worktree != nil {
		payload["worktree"] = worktree
	}
	if strings.TrimSpace(errText) != "" {
		payload["error"] = errText
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}

	f, err := os.OpenFile(eb.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

func (eb *EventBus) ListRecent(limit int) (string, error) {
	limit = clamp(limit, 1, 200)

	eb.mu.Lock()
	defer eb.mu.Unlock()

	raw, err := os.ReadFile(eb.path)
	if err != nil {
		return "", err
	}

	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}

	items := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		item := map[string]any{}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			item = map[string]any{
				"event": "parse_error",
				"raw":   line,
			}
		}
		items = append(items, item)
	}

	out, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

type WorktreeEntry struct {
	Name      string  `json:"name"`
	Path      string  `json:"path"`
	Branch    string  `json:"branch"`
	TaskID    *int    `json:"task_id,omitempty"`
	Status    string  `json:"status"`
	CreatedAt float64 `json:"created_at,omitempty"`
	RemovedAt float64 `json:"removed_at,omitempty"`
	KeptAt    float64 `json:"kept_at,omitempty"`
}

type worktreeIndex struct {
	Worktrees []WorktreeEntry `json:"worktrees"`
}

type WorktreeManager struct {
	repoRoot     string
	dir          string
	indexPath    string
	gitAvailable bool
	tasks        *TaskManager
	events       *EventBus
	mu           sync.Mutex
}

func NewWorktreeManager(repoRoot string, tasks *TaskManager, events *EventBus) (*WorktreeManager, error) {
	dir := filepath.Join(repoRoot, worktreesDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	indexPath := filepath.Join(dir, worktreeIndexName)
	if _, err := os.Stat(indexPath); errors.Is(err, os.ErrNotExist) {
		idx := worktreeIndex{Worktrees: []WorktreeEntry{}}
		raw, _ := json.MarshalIndent(idx, "", "  ")
		raw = append(raw, '\n')
		if err := os.WriteFile(indexPath, raw, 0o644); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	return &WorktreeManager{
		repoRoot:     repoRoot,
		dir:          dir,
		indexPath:    indexPath,
		gitAvailable: isGitRepo(repoRoot),
		tasks:        tasks,
		events:       events,
	}, nil
}

func isGitRepo(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), repoDetectTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func (wm *WorktreeManager) GitAvailable() bool {
	return wm.gitAvailable
}

func (wm *WorktreeManager) loadIndexLocked() (worktreeIndex, error) {
	raw, err := os.ReadFile(wm.indexPath)
	if err != nil {
		return worktreeIndex{}, err
	}

	var idx worktreeIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return worktreeIndex{}, err
	}
	if idx.Worktrees == nil {
		idx.Worktrees = []WorktreeEntry{}
	}
	return idx, nil
}

func (wm *WorktreeManager) saveIndexLocked(idx worktreeIndex) error {
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(wm.indexPath, raw, 0o644)
}

func (wm *WorktreeManager) findLocked(idx worktreeIndex, name string) (int, *WorktreeEntry) {
	for i := range idx.Worktrees {
		if idx.Worktrees[i].Name == name {
			return i, &idx.Worktrees[i]
		}
	}
	return -1, nil
}

func (wm *WorktreeManager) runGit(args ...string) (string, error) {
	if !wm.gitAvailable {
		return "", errors.New("Not in a git repository. worktree tools require git.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = wm.repoRoot
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", errors.New("git command timeout (120s)")
	}

	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", errors.New(text)
	}
	if text == "" {
		text = "(no output)"
	}
	return text, nil
}

func (wm *WorktreeManager) Create(name string, taskID *int, baseRef string) (string, error) {
	name = strings.TrimSpace(name)
	if !worktreeNamePattern.MatchString(name) {
		return "", errors.New("Invalid worktree name. Use 1-40 chars: letters, numbers, ., _, -")
	}
	if strings.TrimSpace(baseRef) == "" {
		baseRef = "HEAD"
	}
	if taskID != nil && *taskID <= 0 {
		return "", errors.New("task_id must be positive")
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	idx, err := wm.loadIndexLocked()
	if err != nil {
		return "", err
	}
	if _, existing := wm.findLocked(idx, name); existing != nil {
		return "", fmt.Errorf("Worktree '%s' already exists in index", name)
	}
	if taskID != nil && !wm.tasks.Exists(*taskID) {
		return "", fmt.Errorf("Task %d not found", *taskID)
	}

	path := filepath.Join(wm.dir, name)
	branch := "wt/" + name
	taskMap := map[string]any{}
	if taskID != nil {
		taskMap["id"] = *taskID
	}
	wm.events.Emit("worktree.create.before", taskMap, map[string]any{"name": name, "base_ref": baseRef}, "")

	if _, err := wm.runGit("worktree", "add", "-b", branch, path, baseRef); err != nil {
		wm.events.Emit("worktree.create.failed", taskMap, map[string]any{"name": name, "base_ref": baseRef}, err.Error())
		return "", err
	}

	entry := WorktreeEntry{
		Name:      name,
		Path:      path,
		Branch:    branch,
		TaskID:    taskID,
		Status:    "active",
		CreatedAt: nowUnixFloat(),
	}
	idx.Worktrees = append(idx.Worktrees, entry)
	if err := wm.saveIndexLocked(idx); err != nil {
		wm.events.Emit("worktree.create.failed", taskMap, map[string]any{"name": name, "path": path}, err.Error())
		return "", err
	}
	if taskID != nil {
		if _, err := wm.tasks.BindWorktree(*taskID, name, ""); err != nil {
			wm.events.Emit("worktree.create.failed", taskMap, map[string]any{"name": name, "path": path}, err.Error())
			return "", err
		}
	}

	wm.events.Emit("worktree.create.after", taskMap, map[string]any{
		"name":   name,
		"path":   path,
		"branch": branch,
		"status": "active",
	}, "")

	raw, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (wm *WorktreeManager) ListAll() (string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	idx, err := wm.loadIndexLocked()
	if err != nil {
		return "", err
	}
	if len(idx.Worktrees) == 0 {
		return "No worktrees in index.", nil
	}

	lines := make([]string, 0, len(idx.Worktrees))
	for _, wt := range idx.Worktrees {
		taskSuffix := ""
		if wt.TaskID != nil {
			taskSuffix = fmt.Sprintf(" task=%d", *wt.TaskID)
		}
		lines = append(lines, fmt.Sprintf("[%s] %s -> %s (%s)%s", defaultString(wt.Status, "unknown"), wt.Name, wt.Path, defaultString(wt.Branch, "-"), taskSuffix))
	}
	return strings.Join(lines, "\n"), nil
}

func (wm *WorktreeManager) Status(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is required")
	}

	wm.mu.Lock()
	idx, err := wm.loadIndexLocked()
	if err != nil {
		wm.mu.Unlock()
		return "", err
	}
	_, wt := wm.findLocked(idx, name)
	if wt == nil {
		wm.mu.Unlock()
		return "", fmt.Errorf("Unknown worktree '%s'", name)
	}
	path := wt.Path
	wm.mu.Unlock()

	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("Worktree path missing: %s", path)
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "status", "--short", "--branch")
	cmd.Dir = path
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", errors.New("git status timeout (120s)")
	}
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", errors.New(text)
	}
	if text == "" {
		text = "Clean worktree"
	}
	return truncate(text, maxOutputChars), nil
}

func (wm *WorktreeManager) Run(name, command string) (string, error) {
	name = strings.TrimSpace(name)
	command = strings.TrimSpace(command)
	if name == "" {
		return "", errors.New("name is required")
	}
	if command == "" {
		return "", errors.New("command is required")
	}
	if isDangerousCommand(command) {
		return "Error: Dangerous command blocked", nil
	}

	wm.mu.Lock()
	idx, err := wm.loadIndexLocked()
	if err != nil {
		wm.mu.Unlock()
		return "", err
	}
	_, wt := wm.findLocked(idx, name)
	if wt == nil {
		wm.mu.Unlock()
		return "", fmt.Errorf("Unknown worktree '%s'", name)
	}
	path := wt.Path
	wm.mu.Unlock()

	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("Worktree path missing: %s", path)
	}

	text, err := runShell(path, command, worktreeCommandTimeout)
	if errors.Is(err, context.DeadlineExceeded) {
		return "Error: Timeout (300s)", nil
	}
	if err != nil && strings.TrimSpace(text) == "" {
		return "", err
	}
	return truncate(text, maxOutputChars), nil
}

func (wm *WorktreeManager) Remove(name string, force, completeTask bool) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is required")
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	idx, err := wm.loadIndexLocked()
	if err != nil {
		return "", err
	}
	pos, wt := wm.findLocked(idx, name)
	if wt == nil {
		return "", fmt.Errorf("Unknown worktree '%s'", name)
	}

	taskMap := map[string]any{}
	if wt.TaskID != nil {
		taskMap["id"] = *wt.TaskID
	}
	wm.events.Emit("worktree.remove.before", taskMap, map[string]any{"name": name, "path": wt.Path}, "")

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wt.Path)
	if _, err := wm.runGit(args...); err != nil {
		wm.events.Emit("worktree.remove.failed", taskMap, map[string]any{"name": name, "path": wt.Path}, err.Error())
		return "", err
	}

	if completeTask && wt.TaskID != nil {
		taskID := *wt.TaskID
		taskSubject := ""
		if raw, err := wm.tasks.Get(taskID); err == nil {
			var t Task
			if json.Unmarshal([]byte(raw), &t) == nil {
				taskSubject = t.Subject
			}
		}
		if _, err := wm.tasks.UpdateStatusOwner(taskID, "completed", nil); err != nil {
			wm.events.Emit("worktree.remove.failed", map[string]any{"id": taskID}, map[string]any{"name": name}, err.Error())
			return "", err
		}
		if _, err := wm.tasks.UnbindWorktree(taskID); err != nil {
			wm.events.Emit("worktree.remove.failed", map[string]any{"id": taskID}, map[string]any{"name": name}, err.Error())
			return "", err
		}
		wm.events.Emit("task.completed", map[string]any{"id": taskID, "subject": taskSubject, "status": "completed"}, map[string]any{"name": name}, "")
	}

	idx.Worktrees[pos].Status = "removed"
	idx.Worktrees[pos].RemovedAt = nowUnixFloat()
	if err := wm.saveIndexLocked(idx); err != nil {
		return "", err
	}

	wm.events.Emit("worktree.remove.after", taskMap, map[string]any{"name": name, "path": wt.Path, "status": "removed"}, "")
	return fmt.Sprintf("Removed worktree '%s'", name), nil
}

func (wm *WorktreeManager) Keep(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is required")
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	idx, err := wm.loadIndexLocked()
	if err != nil {
		return "", err
	}
	pos, wt := wm.findLocked(idx, name)
	if wt == nil {
		return "", fmt.Errorf("Unknown worktree '%s'", name)
	}

	idx.Worktrees[pos].Status = "kept"
	idx.Worktrees[pos].KeptAt = nowUnixFloat()
	if err := wm.saveIndexLocked(idx); err != nil {
		return "", err
	}

	taskMap := map[string]any{}
	if wt.TaskID != nil {
		taskMap["id"] = *wt.TaskID
	}
	wm.events.Emit("worktree.keep", taskMap, map[string]any{"name": name, "path": wt.Path, "status": "kept"}, "")

	raw, err := json.MarshalIndent(idx.Worktrees[pos], "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

type Agent struct {
	client    *openai.Client
	modelID   string
	workDir   string
	repoRoot  string
	system    string
	tools     []openai.ToolDef
	tasks     *TaskManager
	worktrees *WorktreeManager
	events    *EventBus
	handlers  map[string]toolHandler
	history   []openai.Message
}

func detectRepoRoot(workDir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), repoDetectTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return workDir
	}

	root := strings.TrimSpace(string(out))
	if root == "" {
		return workDir
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return workDir
	}
	return root
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
	repoRoot := detectRepoRoot(workDir)

	tasks, err := NewTaskManager(filepath.Join(repoRoot, tasksDirName))
	if err != nil {
		return nil, err
	}
	events, err := NewEventBus(filepath.Join(repoRoot, worktreesDirName, worktreeEventsFileName))
	if err != nil {
		return nil, err
	}
	worktrees, err := NewWorktreeManager(repoRoot, tasks, events)
	if err != nil {
		return nil, err
	}

	a := &Agent{
		client:    openai.NewClient(cfg.APIKey, cfg.BaseURL, openAITimeout),
		modelID:   cfg.ModelID,
		workDir:   workDir,
		repoRoot:  repoRoot,
		system:    fmt.Sprintf("You are a coding agent at %s. Use task + worktree tools for multi-task work. For parallel or risky changes: create tasks, allocate worktree lanes, run commands in those lanes, then choose keep/remove for closeout. Use worktree_events when you need lifecycle visibility.", workDir),
		tools:     buildTools(),
		tasks:     tasks,
		worktrees: worktrees,
		events:    events,
	}
	a.handlers = map[string]toolHandler{
		"bash":               a.handleBash,
		"read_file":          a.handleReadFile,
		"write_file":         a.handleWriteFile,
		"edit_file":          a.handleEditFile,
		"task_create":        a.handleTaskCreate,
		"task_list":          a.handleTaskList,
		"task_get":           a.handleTaskGet,
		"task_update":        a.handleTaskUpdate,
		"task_bind_worktree": a.handleTaskBindWorktree,
		"worktree_create":    a.handleWorktreeCreate,
		"worktree_list":      a.handleWorktreeList,
		"worktree_status":    a.handleWorktreeStatus,
		"worktree_run":       a.handleWorktreeRun,
		"worktree_keep":      a.handleWorktreeKeep,
		"worktree_remove":    a.handleWorktreeRemove,
		"worktree_events":    a.handleWorktreeEvents,
	}

	return a, nil
}

func (a *Agent) RepoRoot() string {
	return a.repoRoot
}

func (a *Agent) WorktreeGitAvailable() bool {
	return a.worktrees != nil && a.worktrees.GitAvailable()
}

func buildTools() []openai.ToolDef {
	return []openai.ToolDef{
		{
			Name:        "bash",
			Description: "Run a shell command in the current workspace (blocking).",
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
			Description: "Create a new task on the shared task board.",
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
			Description: "Update task status or owner.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "integer"},
					"status": map[string]any{
						"type": "string",
						"enum": []string{"pending", "in_progress", "completed"},
					},
					"owner": map[string]any{"type": "string"},
				},
				"required": []string{"task_id"},
			},
		},
		{
			Name:        "task_list",
			Description: "List all tasks with status, owner, and worktree binding.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
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
			Name:        "task_bind_worktree",
			Description: "Bind a task to a worktree name.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id":  map[string]any{"type": "integer"},
					"worktree": map[string]any{"type": "string"},
					"owner":    map[string]any{"type": "string"},
				},
				"required": []string{"task_id", "worktree"},
			},
		},
		{
			Name:        "worktree_create",
			Description: "Create a git worktree and optionally bind it to a task.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":     map[string]any{"type": "string"},
					"task_id":  map[string]any{"type": "integer"},
					"base_ref": map[string]any{"type": "string"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "worktree_list",
			Description: "List worktrees tracked in .worktrees/index.json.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "worktree_status",
			Description: "Show git status for one worktree.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "worktree_run",
			Description: "Run a shell command in a named worktree directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":    map[string]any{"type": "string"},
					"command": map[string]any{"type": "string"},
				},
				"required": []string{"name", "command"},
			},
		},
		{
			Name:        "worktree_remove",
			Description: "Remove a worktree and optionally mark its bound task completed.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":          map[string]any{"type": "string"},
					"force":         map[string]any{"type": "boolean"},
					"complete_task": map[string]any{"type": "boolean"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "worktree_keep",
			Description: "Mark a worktree as kept in lifecycle state without removing it.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "worktree_events",
			Description: "List recent worktree/task lifecycle events from .worktrees/events.jsonl.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "integer"},
				},
			},
		},
	}
}

func (a *Agent) RunTurn(query string) error {
	q := strings.TrimSpace(query)
	switch q {
	case "/tasks":
		out, err := a.tasks.ListAll()
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	case "/worktrees":
		out, err := a.worktrees.ListAll()
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	case "/events":
		out, err := a.events.ListRecent(20)
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
	out, err := a.tasks.UpdateStatusOwner(in.TaskID, in.Status, in.Owner)
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
	out, err := a.tasks.Get(in.TaskID)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleTaskBindWorktree(arguments string) string {
	var in taskBindWorktreeInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.tasks.BindWorktree(in.TaskID, in.Worktree, in.Owner)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleWorktreeCreate(arguments string) string {
	var in worktreeCreateInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.worktrees.Create(in.Name, in.TaskID, in.BaseRef)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleWorktreeList(arguments string) string {
	args := strings.TrimSpace(arguments)
	if args != "" && args != "{}" {
		var dummy map[string]any
		if err := json.Unmarshal([]byte(arguments), &dummy); err != nil {
			return "Error: invalid tool input"
		}
	}
	out, err := a.worktrees.ListAll()
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleWorktreeStatus(arguments string) string {
	var in worktreeNameInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.worktrees.Status(in.Name)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleWorktreeRun(arguments string) string {
	var in worktreeRunInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.worktrees.Run(in.Name, in.Command)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleWorktreeKeep(arguments string) string {
	var in worktreeNameInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.worktrees.Keep(in.Name)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleWorktreeRemove(arguments string) string {
	var in worktreeRemoveInput
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return "Error: invalid tool input"
	}
	out, err := a.worktrees.Remove(in.Name, in.Force, in.CompleteTask)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (a *Agent) handleWorktreeEvents(arguments string) string {
	limit := 20
	args := strings.TrimSpace(arguments)
	if args != "" && args != "{}" {
		var in worktreeEventsInput
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "Error: invalid tool input"
		}
		if in.Limit > 0 {
			limit = in.Limit
		}
	}
	out, err := a.events.ListRecent(limit)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func runBash(workDir, command string) string {
	if isDangerousCommand(command) {
		return "Error: Dangerous command blocked"
	}

	text, err := runShell(workDir, command, commandTimeout)
	if errors.Is(err, context.DeadlineExceeded) {
		return "Error: Timeout (120s)"
	}
	if err != nil && strings.TrimSpace(text) == "" {
		return "Error: " + err.Error()
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

func isDangerousCommand(command string) bool {
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return true
		}
	}
	return false
}

func runShell(cwd, command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// Force UTF-8 output on Windows to avoid mojibake in CombinedOutput.
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
	cmd.Dir = cwd

	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", context.DeadlineExceeded
	}
	text := strings.TrimSpace(string(out))
	if err != nil && text == "" {
		text = err.Error()
	}
	if text == "" {
		text = "(no output)"
	}
	return text, err
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
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
