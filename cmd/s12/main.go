package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s12"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s12.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	fmt.Println("s12 worktree-task-isolation mode. Enter a prompt, or q/exit/empty to quit.")
	fmt.Printf("Repo root for s12: %s\n", agent.RepoRoot())
	if !agent.WorktreeGitAvailable() {
		fmt.Println("Note: Not in a git repo. worktree_* tools will return errors.")
	}
	fmt.Println("Shortcuts: /tasks, /worktrees, /events")
	repl.Run("\x1b[36ms12 >> \x1b[0m", agent.RunTurn)
}
