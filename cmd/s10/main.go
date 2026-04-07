package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s10"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s10.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	fmt.Println("s10 team-protocols mode. Enter a prompt, or q/exit/empty to quit.")
	fmt.Println("Shortcuts: /team, /inbox")
	repl.Run("\x1b[36ms10 >> \x1b[0m", agent.RunTurn)
}
