package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

var captureMailWorkGeneration = func(name string) (tmux.SessionGeneration, error) {
	return tmux.NewTmux().CaptureSessionGeneration(name)
}

func validateMailClaimArgs(_ *cobra.Command, args []string) error {
	if mailClaimID != "" && len(args) != 0 {
		return fmt.Errorf("claim accepts either --id or a queue name, not both")
	}
	if len(args) > 1 {
		return fmt.Errorf("claim accepts at most one queue name")
	}
	return nil
}

func captureCurrentMailWorkGeneration() (tmux.SessionGeneration, error) {
	sessionName := os.Getenv("GT_SESSION")
	if sessionName == "" {
		return tmux.SessionGeneration{}, fmt.Errorf("GT_SESSION not set; mail work requires exact session custody")
	}
	generation, err := captureMailWorkGeneration(sessionName)
	if err != nil {
		return tmux.SessionGeneration{}, fmt.Errorf("capturing exact session generation: %w", err)
	}
	return generation, nil
}

func withCurrentMailWorkStore(run func(context.Context, *mail.MailWorkStore, string, tmux.SessionGeneration) error) error {
	workDir, err := findMailWorkDir()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	generation, err := captureCurrentMailWorkGeneration()
	if err != nil {
		return err
	}
	beadsDir := beads.ResolveBeadsDir(workDir)
	openCtx, openCancel := context.WithTimeout(context.Background(), 30*time.Second)
	store, cleanup, err := beads.NewWithBeadsDir(workDir, beadsDir).OpenStore(openCtx)
	openCancel()
	if err != nil {
		return fmt.Errorf("opening mail work store: %w", err)
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return run(ctx, mail.NewMailWorkStore(store), mail.AddressToIdentity(detectSender()), generation)
}

func runMailBlock(_ *cobra.Command, args []string) error {
	id := args[0]
	return withCurrentMailWorkStore(func(ctx context.Context, store *mail.MailWorkStore, actor string, generation tmux.SessionGeneration) error {
		if _, err := store.Block(ctx, id, actor, generation, mailBlockMessage); err != nil {
			return fmt.Errorf("blocking mail work: %w", err)
		}
		fmt.Printf("%s Blocked mail work %s\n", style.Bold.Render("✓"), id)
		return nil
	})
}

func validateMailBlock(_ *cobra.Command, _ []string) error {
	if strings.TrimSpace(mailBlockMessage) == "" {
		return fmt.Errorf("block reason required: use --message")
	}
	return nil
}

func runMailResume(_ *cobra.Command, args []string) error {
	id := args[0]
	return withCurrentMailWorkStore(func(ctx context.Context, store *mail.MailWorkStore, actor string, generation tmux.SessionGeneration) error {
		if _, err := store.Resume(ctx, id, actor, generation); err != nil {
			return fmt.Errorf("resuming mail work: %w", err)
		}
		fmt.Printf("%s Resumed mail work %s\n", style.Bold.Render("✓"), id)
		return nil
	})
}
