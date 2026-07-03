package builtin

import "strconv"

func pyStr(s string) string { return strconv.Quote(s) }

func firstOutputLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return s[:i]
		}
	}
	return s
}
