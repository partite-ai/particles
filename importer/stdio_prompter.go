package importer

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// StdioPrompter is the default [Prompter] for `particle build`.
// Reads from stdin, writes to stderr (so stdout stays clean for
// machine-readable output).
//
// Password masking uses [golang.org/x/term]; if stdin is not a
// terminal (CI, piped input), Secret reads a line from stdin
// without masking — non-interactive flows (`particle build` in a
// pipeline) typically pre-populate credentials before reaching
// the importer, so the unmasked path is rarely the right one.
type StdioPrompter struct {
	in  *bufio.Reader
	out io.Writer
	// stdinFd is the file descriptor used for terminal-mode
	// detection; defaults to os.Stdin's fd.
	stdinFd int
}

// NewStdioPrompter returns a [StdioPrompter] reading from
// os.Stdin and writing to os.Stderr.
func NewStdioPrompter() *StdioPrompter {
	return &StdioPrompter{
		in:      bufio.NewReader(os.Stdin),
		out:     os.Stderr,
		stdinFd: int(os.Stdin.Fd()),
	}
}

// IsTerminal reports whether stdin is connected to a terminal.
// Useful for falling back to non-interactive errors when the
// user runs `particle build` in a pipeline.
func (p *StdioPrompter) IsTerminal() bool {
	return term.IsTerminal(p.stdinFd)
}

func (p *StdioPrompter) String(question, defaultValue string) (string, error) {
	for {
		if defaultValue != "" {
			fmt.Fprintf(p.out, "%s [%s]: ", question, defaultValue)
		} else {
			fmt.Fprintf(p.out, "%s: ", question)
		}
		line, err := p.in.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read input: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if defaultValue != "" {
				return defaultValue, nil
			}
			fmt.Fprintln(p.out, "  (value cannot be empty)")
			continue
		}
		return line, nil
	}
}

func (p *StdioPrompter) Secret(question string) (string, error) {
	for {
		s, err := p.readSecretLine(question)
		if err != nil {
			return "", err
		}
		if s == "" {
			fmt.Fprintln(p.out, "  (value cannot be empty)")
			continue
		}
		return s, nil
	}
}

// clearSecretSentinel is the literal a user types at a
// SecretWithKeep prompt to explicitly request removal of the
// stored secret. We confirm before acting on it, so a user
// whose real secret happens to be "-" still has a way out.
const clearSecretSentinel = "-"

// SecretWithKeep prompts for a secret with three possible
// outcomes:
//
//   - Empty input → [SecretKept]: keep the stored value.
//   - Sentinel "-" + confirm → [SecretCleared]: explicitly
//     remove the stored value. The confirm guards against a
//     user whose real secret happens to be "-": pressing N
//     re-prompts.
//   - Any other input → [SecretSet]: rotate to that value.
func (p *StdioPrompter) SecretWithKeep(question string) (string, SecretChoice, error) {
	full := question + ` (press Enter to keep current; "` + clearSecretSentinel + `" to clear)`
	for {
		s, err := p.readSecretLine(full)
		if err != nil {
			return "", SecretKept, err
		}
		if s == "" {
			return "", SecretKept, nil
		}
		if s == clearSecretSentinel {
			ok, err := p.Confirm("Clear the stored value?", false)
			if err != nil {
				return "", SecretKept, err
			}
			if !ok {
				// Re-prompt — the user backed out, and we
				// don't want to fall through to treating
				// "-" as a literal value just because they
				// hesitated.
				continue
			}
			return "", SecretCleared, nil
		}
		return s, SecretSet, nil
	}
}

// readSecretLine prints the prompt and reads one line, masking
// the entry when stdin is a terminal. Empty result is allowed —
// callers (Secret vs SecretWithKeep) interpret it differently.
func (p *StdioPrompter) readSecretLine(question string) (string, error) {
	fmt.Fprintf(p.out, "%s: ", question)
	var bytes []byte
	var err error
	if term.IsTerminal(p.stdinFd) {
		bytes, err = term.ReadPassword(p.stdinFd)
		fmt.Fprintln(p.out) // newline after the masked entry
	} else {
		line, lerr := p.in.ReadString('\n')
		err = lerr
		bytes = []byte(strings.TrimRight(line, "\r\n"))
	}
	if err != nil && len(bytes) == 0 {
		return "", fmt.Errorf("read input: %w", err)
	}
	return string(bytes), nil
}

func (p *StdioPrompter) Choice(question string, options []ChoiceOption) (string, error) {
	if len(options) == 0 {
		return "", fmt.Errorf("Choice: no options provided")
	}
	for {
		fmt.Fprintf(p.out, "%s\n", question)
		for i, o := range options {
			fmt.Fprintf(p.out, "  %d) %s", i+1, o.Label)
			if o.Description != "" {
				fmt.Fprintf(p.out, " — %s", o.Description)
			}
			fmt.Fprintln(p.out)
		}
		fmt.Fprintf(p.out, "Choose [1-%d]: ", len(options))

		line, err := p.in.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read input: %w", err)
		}
		line = strings.TrimSpace(line)
		idx, err := strconv.Atoi(line)
		if err != nil || idx < 1 || idx > len(options) {
			fmt.Fprintf(p.out, "  (please enter a number between 1 and %d)\n", len(options))
			continue
		}
		return options[idx-1].Value, nil
	}
}

func (p *StdioPrompter) Confirm(question string, defaultYes bool) (bool, error) {
	hint := "[y/N]"
	if defaultYes {
		hint = "[Y/n]"
	}
	for {
		fmt.Fprintf(p.out, "%s %s: ", question, hint)
		line, err := p.in.ReadString('\n')
		if err != nil && line == "" {
			return false, fmt.Errorf("read input: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return defaultYes, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(p.out, "  (please answer y or n)")
		}
	}
}

func (p *StdioPrompter) Warn(message string) {
	fmt.Fprintf(p.out, "⚠  %s\n", message)
}

func (p *StdioPrompter) Info(message string) {
	fmt.Fprintln(p.out, message)
}
