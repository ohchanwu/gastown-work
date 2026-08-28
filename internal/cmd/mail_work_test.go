package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestMailWorkCommandSurface(t *testing.T) {
	if flag := mailClaimCmd.Flags().Lookup("id"); flag == nil {
		t.Fatal("mail claim is missing --id")
	}
	if flag := mailReplyCmd.Flags().Lookup("complete"); flag == nil {
		t.Fatal("mail reply is missing --complete")
	}

	commands := make(map[string]bool)
	for _, command := range mailCmd.Commands() {
		commands[command.Name()] = true
	}
	for _, want := range []string{"block", "resume"} {
		if !commands[want] {
			t.Fatalf("mail command is missing %q", want)
		}
	}
}

func TestValidateMailClaimArgs(t *testing.T) {
	oldID := mailClaimID
	t.Cleanup(func() { mailClaimID = oldID })
	command := &cobra.Command{}

	mailClaimID = ""
	if err := validateMailClaimArgs(command, []string{"repairs"}); err != nil {
		t.Fatalf("queue claim args: %v", err)
	}
	mailClaimID = "hq-task"
	if err := validateMailClaimArgs(command, nil); err != nil {
		t.Fatalf("exact claim args: %v", err)
	}
	if err := validateMailClaimArgs(command, []string{"repairs"}); err == nil {
		t.Fatal("claim accepted both --id and queue")
	}
}

func TestValidateMailBlock(t *testing.T) {
	oldMessage := mailBlockMessage
	t.Cleanup(func() { mailBlockMessage = oldMessage })

	mailBlockMessage = "  "
	if err := validateMailBlock(nil, nil); err == nil {
		t.Fatal("block accepted an empty reason")
	}
	mailBlockMessage = "waiting for owner input"
	if err := validateMailBlock(nil, nil); err != nil {
		t.Fatalf("block rejected reason: %v", err)
	}
}

func TestQueueWorkSelectionRequiresEnrollmentLabel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bd stub")
	}
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeBDStub(t, binDir, `#!/usr/bin/env sh
printf '%s\n' "$*" > "$BD_STUB_LOG"
printf '[]\n'
`, "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logPath)

	if _, err := listUnclaimedQueueMessages(t.TempDir(), "repairs"); err != nil {
		t.Fatalf("listUnclaimedQueueMessages: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "--label gt:mail-work") {
		t.Fatalf("queue selection omitted enrollment label: %s", log)
	}
}

func TestCaptureCurrentMailWorkGeneration(t *testing.T) {
	oldCapture := captureMailWorkGeneration
	t.Cleanup(func() { captureMailWorkGeneration = oldCapture })

	t.Setenv("GT_SESSION", "")
	if _, err := captureCurrentMailWorkGeneration(); err == nil {
		t.Fatal("capture succeeded without GT_SESSION")
	}

	t.Setenv("GT_SESSION", "gt-gastown-Toast")
	want := tmux.SessionGeneration{Name: "gt-gastown-Toast", SessionID: "$9", Nonce: "generation"}
	captureMailWorkGeneration = func(name string) (tmux.SessionGeneration, error) {
		if name != "gt-gastown-Toast" {
			t.Fatalf("captured session %q", name)
		}
		return want, nil
	}
	got, err := captureCurrentMailWorkGeneration()
	if err != nil {
		t.Fatalf("captureCurrentMailWorkGeneration: %v", err)
	}
	if got.Name != want.Name || got.SessionID != want.SessionID {
		t.Fatalf("generation = %+v, want %+v", got, want)
	}

	captureMailWorkGeneration = func(string) (tmux.SessionGeneration, error) {
		return tmux.SessionGeneration{}, errors.New("tmux unavailable")
	}
	if _, err := captureCurrentMailWorkGeneration(); err == nil {
		t.Fatal("capture swallowed tmux error")
	}
}
