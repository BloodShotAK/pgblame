package pgss

import "regexp"

// Utility statements aren't always normalized, so a CREATE ROLE ... PASSWORD
// or a foreign server's options can reach pg_stat_statements verbatim.
var secrets = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(?i)(\bpassword\s+)(?:[Ee]'(?:[^'\\]|\\.|'')*'|'(?:[^']|'')*'|\$[A-Za-z_]*\$[\s\S]*?\$[A-Za-z_]*\$)`), `${1}'***'`},
	// key=value connection strings; $1 and friends are parameters, not secrets
	{regexp.MustCompile(`(?i)(\bpassword\s*=\s*)(?:[^\s'",;)$]|\$[^\d\s])[^\s'",;)]*`), `${1}***`},
}

// Redact masks passwords in query text.
func Redact(text string) string {
	for _, s := range secrets {
		text = s.re.ReplaceAllString(text, s.with)
	}
	return text
}
