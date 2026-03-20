package sqliteutil

import "strings"

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// EscapeLike escapes SQL LIKE wildcard characters so user input stays literal.
func EscapeLike(s string) string {
	return likeEscaper.Replace(s)
}
