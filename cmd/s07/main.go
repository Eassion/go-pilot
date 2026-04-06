package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s07"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s07.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	fmt.Println("s07 task-system mode. Enter a prompt, or q/exit/empty to quit.")
	repl.Run("\x1b[36ms07 >> \x1b[0m", agent.RunTurn)
}
