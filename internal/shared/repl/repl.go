package repl

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Run starts a simple REPL loop and invokes runTurn for each user input.
func Run(prompt string, runTurn func(string) error) {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(prompt)
		if !scanner.Scan() {
			break
		}

		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}

		if err := runTurn(query); err != nil {
			fmt.Fprintln(os.Stderr, "run error:", err)
		}
		fmt.Println()
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "input error:", err)
	}
}
