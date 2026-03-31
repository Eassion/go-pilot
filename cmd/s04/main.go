package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s04"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s04.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	fmt.Println("s04 subagent mode. Enter a prompt, or q/exit/empty to quit.")
	repl.Run("\x1b[36ms04 >> \x1b[0m", agent.RunTurn)
}
