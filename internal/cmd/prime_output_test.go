package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestOutputRoleDirectives(t *testing.T) {
	t.Parallel()

	t.Run("no directives emits nothing visible", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if strings.Contains(out, "Directives") {
			t.Errorf("expected no header when no directives, got: %s", out)
		}
	})

	t.Run("town-level directive emits town header", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "polecat.md"), []byte("Always be polite."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town Directives") {
			t.Errorf("expected Town Directives header, got: %s", out)
		}
		if !strings.Contains(out, "Always be polite.") {
			t.Errorf("expected directive content, got: %s", out)
		}
	})

	t.Run("rig-level directive emits rig header", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := filepath.Join(townRoot, "myrig", "directives")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "witness.md"), []byte("Watch closely."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RoleWitness,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Rig Directives") {
			t.Errorf("expected Rig Directives header, got: %s", out)
		}
		if !strings.Contains(out, "Watch closely.") {
			t.Errorf("expected directive content, got: %s", out)
		}
	})

	t.Run("both levels emits combined header", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		townDir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(townDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townDir, "polecat.md"), []byte("Town rule."), 0644); err != nil {
			t.Fatal(err)
		}

		rigDir := filepath.Join(townRoot, "myrig", "directives")
		if err := os.MkdirAll(rigDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigDir, "polecat.md"), []byte("Rig rule."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town & Rig Directives") {
			t.Errorf("expected combined header, got: %s", out)
		}
		if !strings.Contains(out, "Town rule.") {
			t.Errorf("expected town content, got: %s", out)
		}
		if !strings.Contains(out, "Rig rule.") {
			t.Errorf("expected rig content, got: %s", out)
		}
	})

	t.Run("explain mode shows file paths", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, true)
		out := buf.String()

		if !strings.Contains(out, "[EXPLAIN]") {
			t.Errorf("expected EXPLAIN output, got: %s", out)
		}
		if !strings.Contains(out, filepath.Join("directives", "polecat.md")) {
			t.Errorf("expected file path in explain output, got: %s", out)
		}
	})

	t.Run("empty rig name skips rig path", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		townDir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(townDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townDir, "mayor.md"), []byte("Mayor directive."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RoleMayor,
			TownRoot: townRoot,
			Rig:      "",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town Directives") {
			t.Errorf("expected Town Directives header, got: %s", out)
		}
		if !strings.Contains(out, "Mayor directive.") {
			t.Errorf("expected directive content, got: %s", out)
		}
	})
}

func TestRenderPendingMailWorkRequiresClaimBeforeActing(t *testing.T) {
	metadata, err := mail.EncodeMailWorkMetadata(nil, &mail.WorkMetadata{Schema: mail.MailWorkSchema, Route: mail.WorkRouteDirect})
	if err != nil {
		t.Fatal(err)
	}
	messages := []*mail.Message{
		{ID: "hq-pending", Subject: "Review exact object", From: "mayor/", To: "gastown/Toast", Type: mail.TypeTask, Status: mail.WorkStateOpen, Labels: []string{"gt:message", mail.MailWorkLabel}, Metadata: metadata},
		{ID: "hq-active", Subject: "Already claimed", From: "mayor/", To: "gastown/Toast", Type: mail.TypeTask, Status: mail.WorkStateInProgress, Labels: []string{"gt:message", mail.MailWorkLabel}},
		{ID: "hq-malformed", Subject: "Broken enrollment", From: "mayor/", To: "gastown/Toast", Type: mail.TypeTask, Status: mail.WorkStateOpen, Labels: []string{"gt:message", mail.MailWorkLabel}},
		{ID: "hq-note", Subject: "FYI", From: "mayor/", To: "gastown/Toast", Type: mail.TypeNotification, Status: mail.WorkStateOpen, Labels: []string{"gt:message"}},
	}
	var output bytes.Buffer
	renderPendingMailWork(&output, messages)
	text := output.String()
	for _, want := range []string{"Pending Task Mail", "hq-pending", "Review exact object", "gt mail claim --id hq-pending", "claim before acting"} {
		if !strings.Contains(text, want) {
			t.Fatalf("pending mail output missing %q: %s", want, text)
		}
	}
	for _, unwanted := range []string{"hq-active", "hq-malformed", "hq-note"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("pending mail output included %q: %s", unwanted, text)
		}
	}
}

func TestContainedWitnessPrimeGuidanceIsExactlyBrokerSafe(t *testing.T) {
	output, err := containedWitnessPrimeOutput(RoleContext{Role: RoleWitness, Rig: "testrig"})
	if err != nil {
		t.Fatal(err)
	}
	segments := strings.Split(output, "`")
	var commands []string
	for index := 1; index < len(segments); index += 2 {
		command := segments[index]
		if !strings.HasPrefix(command, "gt ") {
			t.Fatalf("non-command actionable guidance %q", segments[index])
		}
		commands = append(commands, command)
	}
	want := []string{
		"gt hook", "gt hook show", "gt mol current", "gt mol step close",
		"gt mail inbox --unread", "gt mail read '<message-id>'",
		"gt mail send mayor/ --subject '<subject>' --message '<body>' --no-notify",
		"gt nudge mayor '<message>' --mode queue",
		"gt nudge mayor '<message>' --mode immediate",
		"gt patrol scan --rig testrig --json",
		"gt patrol report --summary '<brief summary of observations>'",
		"gt agents list", "gt polecat list testrig", "gt status --fast",
	}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("contained guidance commands = %q, want %q", commands, want)
	}
	for _, forbidden := range []string{"witness status", "hook attach", "bd close", "polecat nuke", "gt peek", "gt handoff"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("contained guidance advertises unavailable path %q: %s", forbidden, output)
		}
	}
}

func TestOutputRoleContextContainedWitnessSkipsUnbrokeredOperatorContent(t *testing.T) {
	t.Setenv(tmux.EnvSessionBrokerWorker, "1")
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "directives"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(townRoot, "directives", "witness.md"),
		[]byte("DIRECTIVE_SENTINEL `gt handoff -s unsafe -m unsafe`\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(townRoot, "CONTEXT.md"),
		[]byte("CONTEXT_SENTINEL `gt polecat nuke testrig/example --force`\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	var roleErr error
	output := captureStdout(t, func() {
		_, roleErr = outputRoleContext(RoleContext{
			Role: RoleWitness, Rig: "testrig", TownRoot: townRoot, WorkDir: townRoot,
		})
	})
	if roleErr != nil {
		t.Fatal(roleErr)
	}
	if !strings.Contains(output, "# Contained Witness Context") {
		t.Fatalf("contained role context missing reviewed guidance: %s", output)
	}
	for _, forbidden := range []string{"DIRECTIVE_SENTINEL", "CONTEXT_SENTINEL", "gt handoff", "gt polecat nuke"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("contained role context emitted unbrokered operator content %q: %s", forbidden, output)
		}
	}
}

func TestOutputCommandQuickReferenceBootBlocksRawTmux(t *testing.T) {
	output := captureStdout(t, func() {
		outputCommandQuickReference(RoleContext{Role: RoleBoot})
	})

	for _, want := range []string{
		"gt nudge deacon",
		"blocked; can stage unsubmitted input",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("Boot quick reference missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "tmux send-keys~~ (unreliable)") {
		t.Fatalf("Boot quick reference still calls raw tmux merely unreliable:\n%s", output)
	}
}
