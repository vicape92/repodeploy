package envvars

import (
	"errors"
	"fmt"
	"strings"
)

// ParseDotEnv reads KEY=value lines in the common .env format. Blank lines
// and # comments are skipped and an "export " prefix is allowed. Single-quoted
// values are literal; double-quoted values understand \n, \r, \t, \", \\ and
// \$. Unquoted values end at " #". Build flags are left unset.
func ParseDotEnv(text string) ([]Update, error) {
	var out []Update
	for n, raw := range strings.Split(strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !ValidName(name) {
			return nil, fmt.Errorf("line %d: expected NAME=value with a valid name", n+1)
		}
		spaced := strings.HasPrefix(value, " ") || strings.HasPrefix(value, "\t")
		value = strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(value, `"`):
			end := closingQuote(value, '"')
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated double quote", n+1)
			}
			if !onlyComment(value[end+1:]) {
				return nil, fmt.Errorf("line %d: unexpected text after the closing quote", n+1)
			}
			unquoted, err := unescape(value[1:end])
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", n+1, err)
			}
			value = unquoted
		case strings.HasPrefix(value, "'"):
			end := closingQuote(value, '\'')
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated single quote", n+1)
			}
			if !onlyComment(value[end+1:]) {
				return nil, fmt.Errorf("line %d: unexpected text after the closing quote", n+1)
			}
			value = value[1:end]
		case spaced && strings.HasPrefix(value, "#"):
			value = ""
		default:
			if i := strings.Index(value, " #"); i >= 0 {
				value = strings.TrimSpace(value[:i])
			}
		}
		out = append(out, Update{Name: name, Value: value})
	}
	if len(out) == 0 {
		return nil, errors.New("no variables found")
	}
	return out, nil
}

func unescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i == len(s) {
			return "", errors.New("trailing backslash")
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '"', '\\', '$':
			b.WriteByte(s[i])
		default:
			return "", fmt.Errorf("unsupported escape \\%c", s[i])
		}
	}
	return b.String(), nil
}

// closingQuote returns the index of the quote that closes the quoted value
// starting at s[0], or -1. Double-quoted values skip backslash escapes.
func closingQuote(s string, quote byte) int {
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && quote == '"':
			i++
		case s[i] == quote:
			return i
		}
	}
	return -1
}

// onlyComment reports whether the text after a closing quote is blank or a
// # comment.
func onlyComment(rest string) bool {
	rest = strings.TrimSpace(rest)
	return rest == "" || strings.HasPrefix(rest, "#")
}
