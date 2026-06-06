// Package ui is winc.cpp's tiny console helper: colored status lines, prompts,
// step headers, and an ASCII progress bar. ANSI only (enabled on Windows via the
// platform layer); honors NO_COLOR.
package ui

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const (
	reset  = "\033[0m"
	green  = "\033[32m"
	yellow = "\033[33m"
	red    = "\033[31m"
	cyan   = "\033[36m"
	white  = "\033[97m"
	dim    = "\033[90m"
)

var noColor = os.Getenv("NO_COLOR") != ""

func col(c, s string) string {
	if noColor {
		return s
	}
	return c + s + reset
}

func msg(format string, a ...any) string {
	if len(a) == 0 {
		return format
	}
	return fmt.Sprintf(format, a...)
}

// Say prints a plain line.
func Say(format string, a ...any) { fmt.Println(msg(format, a...)) }

// Good/Warn/Err/Info print status lines with colored markers.
func Good(format string, a ...any) { fmt.Println(col(green, "[+] ") + msg(format, a...)) }
func Warn(format string, a ...any) { fmt.Println(col(yellow, "[!] ") + msg(format, a...)) }
func Err(format string, a ...any)  { fmt.Fprintln(os.Stderr, col(red, "[x] ")+msg(format, a...)) }
func Info(format string, a ...any) { fmt.Println(col(cyan, "[.] ") + msg(format, a...)) }
func Dim(format string, a ...any)  { fmt.Println(col(dim, msg(format, a...))) }

// Prompt asks a question and returns the trimmed reply.
func Prompt(question string) string {
	fmt.Print(question + " ")
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

// Confirm asks a yes/no question; def is used on an empty reply.
func Confirm(question string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	ans := strings.ToLower(Prompt(question + " " + hint))
	if ans == "" {
		return def
	}
	return ans == "y" || ans == "yes"
}

// Step prints a numbered installer step header with a progress bar.
func Step(n, total int, title string) {
	if total < 1 {
		total = 1
	}
	width := 24
	filled := n * width / total
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
	pct := n * 100 / total
	fmt.Println()
	fmt.Println(col(white, fmt.Sprintf("[%s] %3d%%  step %d/%d  %s", bar, pct, n, total, title)))
}
