package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s_full"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s_full.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	fmt.Println("s_full mode. Enter a prompt, or q/exit/empty to quit.")
	fmt.Println("Shortcuts: /compact, /tasks, /team, /inbox")
	repl.Run("\x1b[36ms_full >> \x1b[0m", agent.RunTurn)
}
