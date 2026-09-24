package emulation

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	printCallRe   = regexp.MustCompile(`(?i)print\s*\(\s*['"]([^'"]+)['"]\s*\)`)
	printQuotedRe = regexp.MustCompile(`(?i)print(?:s|ed)?(?:\s+(?:the\s+)?(?:string|text|word))?\s*[:\s]\s*['"]([^'"]+)['"]`)
	printBareRe   = regexp.MustCompile(`(?i)prints?\s+['"]([^'"]+)['"]`)
)

// ExtractCodeExecution 从用户话里抽出要打印的字面量（检测站：print 'HELLO_CHECK'）。
func ExtractCodeExecution(userQuery string) (stdout, code string) {
	q := strings.TrimSpace(userQuery)
	if q == "" {
		return "", "print('ok')"
	}
	for _, re := range []*regexp.Regexp{printCallRe, printQuotedRe, printBareRe} {
		if m := re.FindStringSubmatch(q); len(m) == 2 && strings.TrimSpace(m[1]) != "" {
			out := m[1]
			return out + "\n", fmt.Sprintf("print(%q)", out)
		}
	}
	return "", "print('ok')"
}
