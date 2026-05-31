// Copyright 2026 Zibby Lab. Apache-2.0.

package examples

import (
	"strings"
	"testing"
)

// TestList_HasAllExpectedTemplates pins the set of bundled templates so a
// regression that drops the `//go:embed` directive (or renames a file)
// surfaces immediately in CI. New templates ARE expected to appear here as
// they're added — bump the expected set when they do.
func TestList_HasAllExpectedTemplates(t *testing.T) {
	got := List()
	if len(got) < 3 {
		t.Fatalf("expected at least 3 embedded templates, got %d: %+v", len(got), got)
	}
	want := map[string]bool{
		"wordpress-multisite": false,
		"single-app":          false,
		"nodejs-server":       false,
	}
	for _, tmpl := range got {
		if _, ok := want[tmpl.Name]; ok {
			want[tmpl.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("template %q missing from List(); got=%+v", name, got)
		}
	}
}

// TestGet_ReturnsNonEmptyBytes is the actual embed-correctness gate: if
// the //go:embed directive doesn't pick up the YAML the function returns a
// not-found error rather than the file body.
func TestGet_ReturnsNonEmptyBytes(t *testing.T) {
	for _, name := range []string{"wordpress-multisite", "single-app", "nodejs-server"} {
		body, err := Get(name)
		if err != nil {
			t.Errorf("Get(%q): %v", name, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("Get(%q): empty body", name)
		}
		// Smoke-check: every template starts with the `state_dir:` top-level
		// key once you strip leading comments. Catches a future "we embedded
		// the file but accidentally truncated it" bug.
		if !strings.Contains(string(body), "state_dir:") {
			t.Errorf("Get(%q): missing expected `state_dir:` key", name)
		}
	}
}

// TestGet_UnknownTemplate verifies the error message lists the available
// names so the CLI's "Available templates: …" hint stays in sync with the
// underlying error.
func TestGet_UnknownTemplate(t *testing.T) {
	_, err := Get("does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown template, got nil")
	}
	for _, name := range []string{"wordpress-multisite", "single-app", "nodejs-server"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("expected error to list %q as available, got: %s", name, err)
		}
	}
}

// TestList_Descriptions parses each template's first-comment line and
// confirms we got a non-empty, non-default description. Pins the
// convention so a future template author who forgets the leading `#
// Example config — …` line gets an immediate test failure.
func TestList_Descriptions(t *testing.T) {
	for _, tmpl := range List() {
		if tmpl.Description == "" {
			t.Errorf("template %q has empty description", tmpl.Name)
		}
		// A description that just echoes the name means firstCommentLine
		// fell through to fallback — the YAML's leading comment is missing
		// or malformed.
		if tmpl.Description == tmpl.Name {
			t.Errorf("template %q description fell back to its name — leading `# … — <desc>` comment is missing", tmpl.Name)
		}
	}
}

// TestList_SortedByName guards the ordering contract used by the CLI's
// --list-templates output and the MCP agent_list_templates tool. Both rely
// on a deterministic sort for stable docs / golden-test matching.
func TestList_SortedByName(t *testing.T) {
	prev := ""
	for _, tmpl := range List() {
		if prev != "" && tmpl.Name < prev {
			t.Errorf("List() not sorted: %q came before %q", prev, tmpl.Name)
		}
		prev = tmpl.Name
	}
}

// TestNames_MatchesList sanity-checks that Names() is just the .Name
// projection of List() so callers can use either interchangeably.
func TestNames_MatchesList(t *testing.T) {
	list := List()
	names := Names()
	if len(list) != len(names) {
		t.Fatalf("List() len %d != Names() len %d", len(list), len(names))
	}
	for i := range list {
		if list[i].Name != names[i] {
			t.Errorf("position %d: List().Name=%q, Names()=%q", i, list[i].Name, names[i])
		}
	}
}

// TestTemplates_AgentDrivenDiscoveryShape is the structural gate on the
// agent-driven philosophy. Every shipped template MUST:
//
//  1. carry a `bootstrap:` block (so the agent discovers the host's stack
//     on first run, rather than the YAML hardcoding service names), AND
//  2. have its `bootstrap.name:` (the task the operator manually re-runs
//     to refresh discovery) point at the discover_stack convention, AND
//  3. mention the state file path in at least one schedule prompt — that
//     proves the schedules read what discovery wrote (the whole point of
//     the bootstrap-then-cache pattern), AND
//  4. NOT hardcode well-known service names like `apache2`/`nginx`/
//     `mysql` inside a schedule prompt as the unit-to-restart — those
//     belong in discovered state, not in the prompt. (We allow them in
//     comments and discovery prompts.)
//
// A future template author who drops discovery and goes back to writing
// "systemctl restart apache2" verbatim in the prompt will trip this and
// have to either add discovery or update the test with a justification.
func TestTemplates_AgentDrivenDiscoveryShape(t *testing.T) {
	const (
		stateFile      = "/var/lib/agent-ops/discovered-stack.json"
		discoveryTask  = "discover_stack"
		bootstrapKey   = "bootstrap:"
		schedulesKey   = "schedules:"
	)
	for _, name := range []string{"wordpress-multisite", "single-app", "nodejs-server"} {
		body, err := Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		s := string(body)

		// (1) bootstrap block present
		if !strings.Contains(s, "\n"+bootstrapKey) && !strings.HasPrefix(s, bootstrapKey) {
			t.Errorf("template %q: missing top-level `bootstrap:` block (agent-driven discovery is mandatory)", name)
			continue
		}
		// (2) the bootstrap task is named discover_stack
		if !strings.Contains(s, "name: "+discoveryTask) {
			t.Errorf("template %q: bootstrap.name must be %q so operators have one well-known task to re-trigger discovery", name, discoveryTask)
		}

		// (3) at least one schedule prompt reads the state file
		idx := strings.Index(s, "\n"+schedulesKey)
		if idx < 0 {
			t.Errorf("template %q: missing `schedules:` block", name)
			continue
		}
		schedulesSection := s[idx:]
		if !strings.Contains(schedulesSection, stateFile) {
			t.Errorf("template %q: no schedule prompt references %s — schedules must read discovered state, not re-discover every run", name, stateFile)
		}

		// (4) no hardcoded restart of well-known service names inside a
		// schedule prompt. We grep the schedules section for the
		// telltale pattern `systemctl restart <hardcoded-name>` (or
		// `systemctl reload`) for the legacy hardcoded names. The
		// agent-driven templates should use placeholders read from
		// state (e.g. `<web_restart_cmd>`).
		forbidden := []string{
			"systemctl restart apache2",
			"systemctl reload apache2",
			"systemctl restart nginx",
			"systemctl reload nginx",
			"systemctl restart mysql",
			"systemctl restart mariadb",
			"pm2 restart my-node-app",
		}
		for _, f := range forbidden {
			if strings.Contains(schedulesSection, f) {
				t.Errorf("template %q: schedule prompt contains hardcoded %q — should reference a state-file field (e.g. <web_restart_cmd>) instead", name, f)
			}
		}
	}
}
