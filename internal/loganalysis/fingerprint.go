package loganalysis

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SQLShape removes literal values and comments before SQL reaches analytics.
func SQLShape(engine, sql string) (string, string) {
	const maximum = 8192
	var shape strings.Builder
	for len(sql) > 0 {
		char, width := utf8.DecodeRuneInString(sql)
		switch {
		case unicode.IsSpace(char):
			sql = sql[width:]
			continue
		case strings.HasPrefix(sql, "--"):
			_, rest, _ := strings.Cut(sql, "\n")
			sql = rest
			continue
		case strings.HasPrefix(sql, "/*"):
			sql = afterComment(sql[2:])
			continue
		}
		if shape.Len() > maximum {
			return "", ""
		}
		if shape.Len() > 0 {
			shape.WriteByte(' ')
		}
		switch {
		case char == '\'':
			shape.WriteByte('?')
			sql = afterQuoted(sql, '\'')
		case char == '"' || char == '`':
			rest := afterQuoted(sql, byte(char))
			shape.WriteString(sql[:len(sql)-len(rest)])
			sql = rest
		case char == '$':
			var literal bool
			sql, literal = afterDollar(sql)
			if literal {
				shape.WriteByte('?')
			} else {
				shape.WriteByte('$')
			}
		case char >= '0' && char <= '9' || char == '.' && len(sql) > 1 && sql[1] >= '0' && sql[1] <= '9':
			shape.WriteByte('?')
			sql = afterNumber(sql)
		case unicode.IsLetter(char) || char == '_':
			end := width
			for end < len(sql) {
				next, n := utf8.DecodeRuneInString(sql[end:])
				if !unicode.IsLetter(next) && !unicode.IsDigit(next) && next != '_' && next != '$' {
					break
				}
				end += n
			}
			token := strings.ToLower(sql[:end])
			sql = sql[end:]
			if (token == "e" || token == "n" || token == "x" || token == "b") && strings.HasPrefix(sql, "'") {
				shape.WriteByte('?')
				sql = afterQuoted(sql, '\'')
			} else {
				shape.WriteString(token)
			}
		default:
			shape.WriteRune(char)
			sql = sql[width:]
		}
	}
	normalized := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(shape.String()), ";"))
	if normalized == "" || len(normalized) > maximum {
		return "", ""
	}
	return normalized, digest("sql-v1\x00" + engine + "\x00" + normalized)
}

func afterComment(s string) string {
	depth := 1
	for len(s) > 0 {
		switch {
		case strings.HasPrefix(s, "/*"):
			depth++
			s = s[2:]
		case strings.HasPrefix(s, "*/"):
			depth--
			s = s[2:]
			if depth == 0 {
				return s
			}
		default:
			s = s[1:]
		}
	}
	return ""
}

func afterQuoted(s string, quote byte) string {
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' && quote == '\'' {
			i++
			continue
		}
		if s[i] == quote {
			if i+1 < len(s) && s[i+1] == quote {
				i++
				continue
			}
			return s[i+1:]
		}
	}
	return ""
}

func afterDollar(s string) (string, bool) {
	i := 1
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 1 {
		return s[i:], true
	}
	for i < len(s) && (s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' ||
		s[i] >= '0' && s[i] <= '9' || s[i] == '_') {
		i++
	}
	if i < len(s) && s[i] == '$' {
		_, rest, _ := strings.Cut(s[i+1:], s[:i+1])
		return rest, true
	}
	return s[1:], false
}

func afterNumber(s string) string {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == '_') {
		i++
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '_') {
			i++
		}
	}
	return s[i:]
}
