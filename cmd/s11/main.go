package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s11"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s11.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	fmt.Println("s11 autonomous-agents mode. Enter a prompt, or q/exit/empty to quit.")
	fmt.Println("Shortcuts: /team, /inbox, /tasks")
	repl.Run("\x1b[36ms11 >> \x1b[0m", agent.RunTurn)
}
