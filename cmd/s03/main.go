package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s03"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s03.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	repl.Run("s03 >> ", agent.RunTurn)
}
