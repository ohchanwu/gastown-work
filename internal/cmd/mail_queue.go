package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// runMailClaim claims the oldest unclaimed message from a work queue.
// If a queue name is provided, claims from that specific queue.
// If no queue name is provided, claims from any queue the caller is eligible for.
func runMailClaim(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	caller := mail.AddressToIdentity(detectSender())
	beadsDir := beads.ResolveBeadsDir(townRoot)
	bd := beads.NewWithBeadsDir(townRoot, beadsDir)
	generation, err := captureCurrentMailWorkGeneration()
	if err != nil {
		return err
	}
	openCtx, openCancel := context.WithTimeout(context.Background(), 30*time.Second)
	store, cleanup, err := bd.OpenStore(openCtx)
	openCancel()
	if err != nil {
		return fmt.Errorf("opening mail work store: %w", err)
	}
	defer cleanup()
	workStore := mail.NewMailWorkStore(store)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queueEligible := func(queue, actor string) bool {
		fields, lookupErr := findMailQueueFields(bd, queue)
		return lookupErr == nil && fields.Status == beads.QueueStatusActive && beads.MatchClaimPattern(fields.ClaimPattern, actor)
	}
	if mailClaimID != "" {
		work, claimErr := workStore.Claim(ctx, mailClaimID, caller, generation, queueEligible)
		if claimErr != nil {
			return fmt.Errorf("claiming mail work: %w", claimErr)
		}
		if ackErr := mail.AcknowledgeDeliveryBead(townRoot, beadsDir, mailClaimID, caller); ackErr != nil {
			fmt.Fprintf(os.Stderr, "gt mail claim: delivery ack failed for %s: %v\n", mailClaimID, ackErr)
		}
		fmt.Printf("%s Claimed %s mail work\n", style.Bold.Render("✓"), work.Route)
		fmt.Printf("  ID: %s\n", mailClaimID)
		return nil
	}

	var queueName string
	var queueFields *beads.QueueFields

	if len(args) > 0 {
		queueName = args[0]
		queueFields, err = findMailQueueFields(bd, queueName)
		if err != nil {
			return err
		}
		if !beads.MatchClaimPattern(queueFields.ClaimPattern, caller) {
			return fmt.Errorf("not eligible to claim from queue %s (caller: %s, pattern: %s)",
				queueName, caller, queueFields.ClaimPattern)
		}
	} else {
		eligibleIssues, eligibleFields, err := bd.FindEligibleQueues(caller)
		if err != nil {
			return fmt.Errorf("finding eligible queues: %w", err)
		}
		if len(eligibleIssues) == 0 {
			fmt.Printf("%s No queues available for claiming (caller: %s)\n",
				style.Dim.Render("○"), caller)
			return nil
		}

		queueFields = eligibleFields[0]
		queueName = queueFields.Name
		if queueName == "" {
			queueName = eligibleIssues[0].ID
		}
	}

	messages, err := listUnclaimedQueueMessages(beadsDir, queueName)
	if err != nil {
		return fmt.Errorf("listing queue messages: %w", err)
	}

	if len(messages) == 0 {
		fmt.Printf("%s No messages to claim in queue %s\n", style.Dim.Render("○"), queueName)
		return nil
	}

	var claimed *queueMessage
	for i := range messages {
		candidate := &messages[i]
		_, claimErr := workStore.Claim(ctx, candidate.ID, caller, generation, func(queue, actor string) bool {
			return queue == queueName && queueFields.Status == beads.QueueStatusActive && beads.MatchClaimPattern(queueFields.ClaimPattern, actor)
		})
		if errors.Is(claimErr, mail.ErrMailWorkConflict) {
			continue
		}
		if claimErr != nil {
			return fmt.Errorf("claiming message: %w", claimErr)
		}
		if ackErr := mail.AcknowledgeDeliveryBead(townRoot, beadsDir, candidate.ID, caller); ackErr != nil {
			fmt.Fprintf(os.Stderr, "gt mail claim: delivery ack failed for %s: %v\n", candidate.ID, ackErr)
		}
		claimed = candidate
		break
	}

	if claimed == nil {
		fmt.Printf("%s No messages to claim in queue %s (all contested)\n",
			style.Dim.Render("○"), queueName)
		return nil
	}

	// Print claimed message details
	fmt.Printf("%s Claimed message from queue %s\n", style.Bold.Render("✓"), queueName)
	fmt.Printf("  ID: %s\n", claimed.ID)
	fmt.Printf("  Subject: %s\n", claimed.Title)
	if claimed.Description != "" {
		// Show first line of description
		lines := strings.SplitN(claimed.Description, "\n", 2)
		preview := lines[0]
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		fmt.Printf("  Preview: %s\n", style.Dim.Render(preview))
	}
	fmt.Printf("  From: %s\n", claimed.From)
	fmt.Printf("  Created: %s\n", claimed.Created.Local().Format("2006-01-02 15:04"))

	return nil
}

func findMailQueueFields(bd *beads.Beads, queueName string) (*beads.QueueFields, error) {
	for _, townLevel := range []bool{true, false} {
		issue, fields, err := bd.GetQueueBead(beads.QueueBeadID(queueName, townLevel))
		if err != nil {
			return nil, fmt.Errorf("looking up queue: %w", err)
		}
		if issue != nil && fields != nil {
			return fields, nil
		}
	}
	return nil, fmt.Errorf("unknown queue: %s", queueName)
}

// queueMessage represents a message in a queue.
type queueMessage struct {
	ID          string
	Title       string
	Description string
	From        string
	Created     time.Time
	Priority    int
	ClaimedBy   string
	ClaimedAt   *time.Time
}

// listUnclaimedQueueMessages lists unclaimed messages in a queue.
// Unclaimed messages have queue:<name> label but no claimed-by label.
func listUnclaimedQueueMessages(beadsDir, queueName string) ([]queueMessage, error) {
	// Use bd list to find messages with queue:<name> label and status=open
	args := []string{"list",
		"--label", "queue:" + queueName,
		"--status", "open",
		"--label", "gt:message",
		"--label", mail.MailWorkLabel,
		"--json",
		"--limit", "0",
	}

	cmd := exec.Command("bd", args...)
	cmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return nil, err
	}

	// Parse JSON output
	var issues []struct {
		ID          string    `json:"id"`
		Title       string    `json:"title"`
		Description string    `json:"description"`
		Labels      []string  `json:"labels"`
		CreatedAt   time.Time `json:"created_at"`
		Priority    int       `json:"priority"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil {
		// If no messages, bd might output empty or error
		if strings.TrimSpace(stdout.String()) == "" || strings.TrimSpace(stdout.String()) == "[]" {
			return nil, nil
		}
		return nil, fmt.Errorf("parsing bd output: %w", err)
	}

	// Convert to queueMessage, filtering out already claimed messages
	var messages []queueMessage
	for _, issue := range issues {
		msg := queueMessage{
			ID:          issue.ID,
			Title:       issue.Title,
			Description: issue.Description,
			Created:     issue.CreatedAt,
			Priority:    issue.Priority,
		}

		// Extract labels
		for _, label := range issue.Labels {
			if strings.HasPrefix(label, "from:") {
				msg.From = strings.TrimPrefix(label, "from:")
			} else if strings.HasPrefix(label, "claimed-by:") {
				msg.ClaimedBy = strings.TrimPrefix(label, "claimed-by:")
			} else if strings.HasPrefix(label, "claimed-at:") {
				ts := strings.TrimPrefix(label, "claimed-at:")
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					msg.ClaimedAt = &t
				}
			}
		}
		// Only include unclaimed messages - check both ClaimedBy and ClaimedAt
		// to handle orphaned claimed-at labels from interrupted releases
		if msg.ClaimedBy == "" && msg.ClaimedAt == nil {
			messages = append(messages, msg)
		}
	}

	// Sort by created time (oldest first) for FIFO ordering
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Created.Before(messages[j].Created)
	})

	return messages, nil
}

func runMailRelease(_ *cobra.Command, args []string) error {
	messageID := args[0]
	return withCurrentMailWorkStore(func(ctx context.Context, store *mail.MailWorkStore, actor string, generation tmux.SessionGeneration) error {
		work, err := store.Release(ctx, messageID, actor, generation)
		if err != nil {
			return fmt.Errorf("releasing mail work: %w", err)
		}
		fmt.Printf("%s Released %s mail work\n", style.Bold.Render("✓"), work.Route)
		fmt.Printf("  ID: %s\n", messageID)
		return nil
	})
}

// Queue management commands (beads-native)

var (
	mailQueueClaimers string
	mailQueueJSON     bool
)

var mailQueueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Manage mail queues",
	Long: `Manage beads-native mail queues.

Queues provide a way to distribute work to eligible workers.
Messages sent to a queue can be claimed by workers matching the claim pattern.

COMMANDS:
  create    Create a new queue
  show      Show queue details
  list      List all queues
  delete    Delete a queue

Examples:
  gt mail queue create work --claimers 'gastown/polecats/*'
  gt mail queue show work
  gt mail queue list
  gt mail queue delete work`,
	RunE: requireSubcommand,
}

var mailQueueCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new queue",
	Long: `Create a new beads-native mail queue.

The --claimers flag specifies a pattern for who can claim messages from this queue.
Patterns support wildcards: 'gastown/polecats/*' matches any polecat in gastown rig.

Examples:
  gt mail queue create work --claimers 'gastown/polecats/*'
  gt mail queue create dispatch --claimers 'gastown/crew/*'
  gt mail queue create urgent --claimers '*'`,
	Args: cobra.ExactArgs(1),
	RunE: runMailQueueCreate,
}

var mailQueueShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show queue details",
	Long: `Show details about a mail queue.

Displays the queue's claim pattern, status, and message counts.

Examples:
  gt mail queue show work
  gt mail queue show dispatch --json`,
	Args: cobra.ExactArgs(1),
	RunE: runMailQueueShow,
}

var mailQueueListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all queues",
	Long: `List all beads-native mail queues.

Shows queue names, claim patterns, and status.

Examples:
  gt mail queue list
  gt mail queue list --json`,
	RunE: runMailQueueList,
}

var mailQueueDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a queue",
	Long: `Delete a mail queue.

This permanently removes the queue bead. Messages in the queue are not affected.

Examples:
  gt mail queue delete work`,
	Args: cobra.ExactArgs(1),
	RunE: runMailQueueDelete,
}

func init() {
	// Queue create flags
	mailQueueCreateCmd.Flags().StringVar(&mailQueueClaimers, "claimers", "", "Pattern for who can claim from this queue (required)")
	_ = mailQueueCreateCmd.MarkFlagRequired("claimers")

	// Queue show/list flags
	mailQueueShowCmd.Flags().BoolVar(&mailQueueJSON, "json", false, "Output as JSON")
	mailQueueListCmd.Flags().BoolVar(&mailQueueJSON, "json", false, "Output as JSON")

	// Add queue subcommands
	mailQueueCmd.AddCommand(mailQueueCreateCmd)
	mailQueueCmd.AddCommand(mailQueueShowCmd)
	mailQueueCmd.AddCommand(mailQueueListCmd)
	mailQueueCmd.AddCommand(mailQueueDeleteCmd)

	// Add queue command to mail
	mailCmd.AddCommand(mailQueueCmd)
}

// runMailQueueCreate creates a new beads-native queue.
func runMailQueueCreate(cmd *cobra.Command, args []string) error {
	queueName := args[0]

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Get caller identity for created_by
	caller := detectSender()

	// Create queue bead
	b := beads.NewWithBeadsDir(townRoot, beads.ResolveBeadsDir(townRoot))

	// Generate queue bead ID (town-level: hq-q-<name>)
	queueID := beads.QueueBeadID(queueName, true)

	// Check if queue already exists
	existing, _, err := b.GetQueueBead(queueID)
	if err != nil {
		return fmt.Errorf("checking for existing queue: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("queue %q already exists", queueName)
	}

	// Create queue fields
	fields := &beads.QueueFields{
		Name:         queueName,
		ClaimPattern: mailQueueClaimers,
		Status:       beads.QueueStatusActive,
		CreatedBy:    caller,
		CreatedAt:    time.Now().Format(time.RFC3339),
	}

	title := fmt.Sprintf("Queue: %s", queueName)
	_, err = b.CreateQueueBead(queueID, title, fields)
	if err != nil {
		return fmt.Errorf("creating queue: %w", err)
	}

	fmt.Printf("%s Created queue %s\n", style.Bold.Render("✓"), queueName)
	fmt.Printf("  ID: %s\n", queueID)
	fmt.Printf("  Claimers: %s\n", mailQueueClaimers)

	return nil
}

// runMailQueueShow shows details about a queue.
func runMailQueueShow(cmd *cobra.Command, args []string) error {
	queueName := args[0]

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Get queue bead
	b := beads.NewWithBeadsDir(townRoot, beads.ResolveBeadsDir(townRoot))

	queueID := beads.QueueBeadID(queueName, true)
	issue, fields, err := b.GetQueueBead(queueID)
	if err != nil {
		return fmt.Errorf("getting queue: %w", err)
	}
	if issue == nil {
		return fmt.Errorf("queue %q not found", queueName)
	}

	if mailQueueJSON {
		output := map[string]interface{}{
			"id":               issue.ID,
			"name":             fields.Name,
			"claim_pattern":    fields.ClaimPattern,
			"status":           fields.Status,
			"available_count":  fields.AvailableCount,
			"processing_count": fields.ProcessingCount,
			"completed_count":  fields.CompletedCount,
			"failed_count":     fields.FailedCount,
			"created_by":       fields.CreatedBy,
			"created_at":       fields.CreatedAt,
		}
		jsonBytes, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	// Human-readable output
	fmt.Printf("%s Queue: %s\n", style.Bold.Render("📬"), queueName)
	fmt.Printf("  ID: %s\n", issue.ID)
	fmt.Printf("  Claimers: %s\n", fields.ClaimPattern)
	fmt.Printf("  Status: %s\n", fields.Status)
	fmt.Printf("  Available: %d\n", fields.AvailableCount)
	fmt.Printf("  Processing: %d\n", fields.ProcessingCount)
	fmt.Printf("  Completed: %d\n", fields.CompletedCount)
	if fields.FailedCount > 0 {
		fmt.Printf("  Failed: %d\n", fields.FailedCount)
	}
	if fields.CreatedBy != "" {
		fmt.Printf("  Created by: %s\n", fields.CreatedBy)
	}
	if fields.CreatedAt != "" {
		fmt.Printf("  Created at: %s\n", fields.CreatedAt)
	}

	return nil
}

// runMailQueueList lists all queues.
func runMailQueueList(cmd *cobra.Command, args []string) error {
	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// List queue beads
	b := beads.NewWithBeadsDir(townRoot, beads.ResolveBeadsDir(townRoot))

	queues, err := b.ListQueueBeads()
	if err != nil {
		return fmt.Errorf("listing queues: %w", err)
	}

	if len(queues) == 0 {
		fmt.Printf("%s No queues found\n", style.Dim.Render("○"))
		return nil
	}

	if mailQueueJSON {
		var output []map[string]interface{}
		for _, issue := range queues {
			fields := beads.ParseQueueFields(issue.Description)
			output = append(output, map[string]interface{}{
				"id":            issue.ID,
				"name":          fields.Name,
				"claim_pattern": fields.ClaimPattern,
				"status":        fields.Status,
			})
		}
		jsonBytes, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	// Human-readable output
	fmt.Printf("%s Queues (%d)\n\n", style.Bold.Render("📬"), len(queues))
	for _, issue := range queues {
		fields := beads.ParseQueueFields(issue.Description)
		fmt.Printf("  %s\n", style.Bold.Render(fields.Name))
		fmt.Printf("    Claimers: %s\n", fields.ClaimPattern)
		fmt.Printf("    Status: %s\n", fields.Status)
	}

	return nil
}

// runMailQueueDelete deletes a queue.
func runMailQueueDelete(cmd *cobra.Command, args []string) error {
	queueName := args[0]

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Delete queue bead
	b := beads.NewWithBeadsDir(townRoot, beads.ResolveBeadsDir(townRoot))

	queueID := beads.QueueBeadID(queueName, true)

	// Verify queue exists
	issue, _, err := b.GetQueueBead(queueID)
	if err != nil {
		return fmt.Errorf("getting queue: %w", err)
	}
	if issue == nil {
		return fmt.Errorf("queue %q not found", queueName)
	}

	if err := b.DeleteQueueBead(queueID); err != nil {
		return fmt.Errorf("deleting queue: %w", err)
	}

	fmt.Printf("%s Deleted queue %s\n", style.Bold.Render("✓"), queueName)

	return nil
}
