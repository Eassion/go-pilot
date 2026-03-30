package main

import (
	"fmt"
	"os"

	"go-pilot/internal/s02"
	"go-pilot/internal/shared/repl"
)

func main() {
	agent, err := s02.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	repl.Run("s02 >> ", agent.RunTurn)
}
