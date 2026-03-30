package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)
import "go-pilot/internal/s01"

func main() {
	agent, err := s01.NewAgent()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		os.Exit(1)
	}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("s01 >> ")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}

		if err := agent.RunTurn(query); err != nil {
			fmt.Fprintln(os.Stderr, "run error:", err)
		}
		fmt.Println()
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "input error:", err)
	}
}
