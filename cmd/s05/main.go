package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s05"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s05.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	fmt.Println("s05 skill-loading mode. Enter a prompt, or q/exit/empty to quit.")
	repl.Run("\x1b[36ms05 >> \x1b[0m", agent.RunTurn)
}
