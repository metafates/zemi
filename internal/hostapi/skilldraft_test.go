package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReflectDraftsSkills: a proposed skill lands as a disabled learned
// draft with secrets scrubbed; an invalid name is skipped.
func TestReflectDraftsSkills(t *testing.T) {
	h, _ := newTestHostSkills(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	reflectStub(t, h, `{"skills":[
		{"name":"run-harness","description":"when asked to run the integration harness","body":"1. export api_key = sk-abcdefghijklmnopqrs\n2. make harness"},
		{"name":"Bad Name","description":"x","body":"y"}]}`)

	if err := h.reflectSession(context.Background(), sid); err != nil {
		t.Fatalf("reflectSession: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(h.learnedSkillsDir(), "run-harness", "SKILL.md"))
	if err != nil {
		t.Fatalf("draft not written: %v", err)
	}
	if doc := string(data); strings.Contains(doc, "sk-abcdefghijklmnopqrs") || !strings.Contains(doc, "make harness") {
		t.Fatalf("draft content:\n%s", doc)
	}
	if _, ok := h.skillsReg.get("run-harness"); !ok || h.skillsReg.isEnabled("run-harness") {
		t.Fatal("draft must be discovered and start disabled")
	}
	if _, err := os.Stat(filepath.Join(h.learnedSkillsDir(), "Bad Name")); err == nil {
		t.Fatal("invalid skill name was written")
	}
}
