package wire

import (
	"regexp"
	"strings"
)

type SweepGuard struct {
	Path   string `json:"path"`
	Symbol string `json:"symbol,omitempty"`
}

func GuardName(symbol string) string {
	s := strings.TrimSpace(symbol)
	if i := strings.LastIndexAny(s, ".#:"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, "()")
	for _, r := range s {
		if !(r == '_' || r == '$' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return ""
		}
	}
	return s
}

func DefinesSymbol(content string,
	symbol string) bool {
	name := GuardName(symbol)
	if name == "" {
		return false
	}
	q := regexp.QuoteMeta(name)
	re := regexp.MustCompile(`(?m)(^|[\s;{(])(func(\s*\([^)]*\))?|function\*?|class|def|const|let|var|type|interface|enum)\s+` + q + `\b`)
	return re.MatchString(content)
}
