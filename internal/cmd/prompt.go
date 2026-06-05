package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func defaultProjectPrompt() func(string) (string, error) {
	reader := bufio.NewReader(os.Stdin)
	return func(prompt string) (string, error) {
		if prompt != "" {
			fmt.Print(prompt)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
}
