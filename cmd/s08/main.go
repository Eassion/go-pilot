package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s08"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s08.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	fmt.Println("s08 background-tasks mode. Enter a prompt, or q/exit/empty to quit.")
	repl.Run("\x1b[36ms08 >> \x1b[0m", agent.RunTurn)
}
