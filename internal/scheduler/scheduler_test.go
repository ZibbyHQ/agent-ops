// Copyright 2026 Zibby Lab. Apache-2.0.

package scheduler

import (
	"strings"
	"testing"
)

// TestAppendNotifyClause_EnvUnset_PromptUnchanged pins the backward-compat
// guarantee: anyone NOT setting AGENT_OPS_NOTIFY_WORKFLOW_ID gets the prompt
// verbatim, no surprise instructions appended.
func TestAppendNotifyClause_EnvUnset_PromptUnchanged(t *testing.T) {
	t.Setenv("AGENT_OPS_NOTIFY_WORKFLOW_ID", "")
	in := "Verify the app is alive. Report status."
	out := appendNotifyClause(in)
	if out != in {
		t.Fatalf("prompt mutated when env unset:\nin =%q\nout=%q", in, out)
	}
}

// TestAppendNotifyClause_EnvSet_AppendsClauseWithID verifies the workflow id
// is substituted verbatim into the appended clause so the LLM knows which
// workflow to fire.
func TestAppendNotifyClause_EnvSet_AppendsClauseWithID(t *testing.T) {
	t.Setenv("AGENT_OPS_NOTIFY_WORKFLOW_ID", "wf_abc123")
	in := "Verify the app is alive. Report status."
	out := appendNotifyClause(in)
	if !strings.HasPrefix(out, in) {
		t.Fatalf("original prompt should be preserved as prefix:\n%s", out)
	}
	if !strings.Contains(out, "wf_abc123") {
		t.Fatalf("appended clause missing workflow id:\n%s", out)
	}
	if !strings.Contains(out, "zibby_workflow") {
		t.Fatalf("appended clause should reference zibby_workflow tool:\n%s", out)
	}
	if !strings.Contains(out, "severity") {
		t.Fatalf("appended clause missing severity field:\n%s", out)
	}
}

// TestAppendNotifyClause_WhitespaceOnly_TreatedAsUnset guards the trim — a
// stray "  " in the env should not flip on the notification clause.
func TestAppendNotifyClause_WhitespaceOnly_TreatedAsUnset(t *testing.T) {
	t.Setenv("AGENT_OPS_NOTIFY_WORKFLOW_ID", "   ")
	in := "Report status."
	out := appendNotifyClause(in)
	if out != in {
		t.Fatalf("whitespace-only env should be treated as unset:\nin =%q\nout=%q", in, out)
	}
}
