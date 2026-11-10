package sanitize

import (
	"strings"
	"testing"
)

// TestScrubKnownShapes: secret is what must not survive; "" means the line
// must come back untouched.
func TestScrubKnownShapes(t *testing.T) {
	cases := []struct{ in, secret string }{
		{"key AKIAIOSFODNN7EXAMPLE in aws", "AKIA"},
		{"openai sk-proj-abcdefghij0123456789 done", "sk-proj"},
		{"recurs sk-aaaaaaaaaaaaaaaaaaaa glued xsk-aaaaaaaaaaaaaaaaaaaa", "sk-a"},
		{"github ghp_0123456789abcdefghij012345 end", "ghp_"},
		{"slack xoxb-123456789012-abcd done", "xoxb"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c end", "eyJ"},
		{"sha 0123456789abcdef0123456789abcdef01234567 end", "0123456789abcdef"},
		{"blob abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJK end", "abcdefghijklmnopqrstuvwxyz"},
		{"db is postgres://admin:hunter2secret@db.internal:5432/app", "hunter2"},
		{"redis redis://:hunter2@cache:6379 and mysql://u:p@ss@h/db", "hunter2"},
		{"mysql://u:p@ss@h/db", "p@ss"},
		{"see https://x:pw@example.com/a", ":pw@"},
		{`curl -H "Authorization: Bearer abc123def456" https://api.example.com`, "abc123def456"},
		{`curl -H "Authorization: Bearer $API_TOKEN" https://api.example.com`, ""},
		{`psql "password=$DB_PASSWORD host=localhost"`, ""},
		{"export API_KEY=${OPENAI_API_KEY}", ""},
		{"set token: {{deploy_token}} in the template", ""},
		{"see https://example.com:8080/a@b", ""},
		{"clone git@github.com:org/repo.git", ""},
		{"https://user@host/x", ""},
		{"config lives in internal/hostapi/permissions_reader.go today", ""},
		{"plain english sentence with common words", ""},
	}
	for _, tc := range cases {
		out := ScrubShapes(tc.in)
		switch {
		case tc.secret == "" && out != tc.in:
			t.Errorf("%q scrubbed to %q", tc.in, out)
		case tc.secret == "":
		case !strings.Contains(out, "[redacted"):
			t.Errorf("%q: not scrubbed: %q", tc.in, out)
		case strings.Contains(out, tc.secret):
			t.Errorf("%q: secret survived: %q", tc.in, out)
		}
	}
}
