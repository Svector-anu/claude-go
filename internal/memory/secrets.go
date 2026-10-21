package memory

import "regexp"

// secretPatterns are formats of credentials that must never be written to a
// notes file. The check is deliberately conservative: a false positive costs
// the model one retry, a false negative writes a secret to disk.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"an Anthropic API key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`)},
	{"an API key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}`)},
	{"a GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`)},
	{"an AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"a Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"a JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
}

// assignment finds "password: value", "api_key=value" and similar.
var assignment = regexp.MustCompile(`(?i)\b(?:pass(?:word|wd)?|secret|token|api[_-]?key)\s*[:=]\s*["']?([^\s"']{8,})`)

// looksLikeSecretValue reports whether the right-hand side of an assignment has
// the shape of a credential (mixed letters and digits, or long) rather than a
// plain word or description ("strategy", "JWT-with-rotation").
func looksLikeSecretValue(v string) bool {
	letters, digits := 0, 0
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters++
		}
	}
	return (letters > 0 && digits > 0) || len(v) >= 24
}

// LooksLikeSecret reports what kind of credential text appears to contain, if any.
func LooksLikeSecret(text string) (string, bool) {
	for _, p := range secretPatterns {
		if p.re.MatchString(text) {
			return p.name, true
		}
	}
	for _, m := range assignment.FindAllStringSubmatch(text, -1) {
		if looksLikeSecretValue(m[1]) {
			return "a password or token assignment", true
		}
	}
	return "", false
}

// looksLikeSecretKey checks a fact key. Keys are namespaces ("auth:strategy"),
// so only credential formats count, never the assignment shape.
func looksLikeSecretKey(key string) (string, bool) {
	for _, p := range secretPatterns {
		if p.re.MatchString(key) {
			return p.name, true
		}
	}
	return "", false
}
