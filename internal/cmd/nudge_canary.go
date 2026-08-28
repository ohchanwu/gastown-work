package cmd

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/delivery"
	"github.com/steveyegge/gastown/internal/hooks"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/witness"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	nudgeCanaryConfirm bool
)

const wakeCanaryTurns = 20

type wakeCanaryResult struct {
	ID          string    `json:"id"`
	Session     string    `json:"session"`
	Turns       int       `json:"turns"`
	Submitted   int       `json:"submitted"`
	Queued      int       `json:"queued"`
	Failed      int       `json:"failed"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

type wakeCanaryState struct {
	SchemaVersion         int                 `json:"schema_version"`
	InstalledBinaryCommit string              `json:"installed_binary_commit"`
	MayorPreset           string              `json:"mayor_preset"`
	MayorProvider         string              `json:"mayor_provider"`
	PolecatPreset         string              `json:"polecat_preset"`
	PolecatProvider       string              `json:"polecat_provider"`
	Session               string              `json:"session"`
	Runtime               string              `json:"runtime"`
	SessionAuthority      string              `json:"session_authority"`
	Turns                 int                 `json:"turns"`
	Submitted             int                 `json:"submitted"`
	Queued                int                 `json:"queued"`
	Failed                int                 `json:"failed"`
	Receipts              []wakeCanaryReceipt `json:"receipts"`
	ReceiptCount          int                 `json:"receipt_count"`
	ReceiptDigest         string              `json:"receipt_digest"`
	AttemptedAt           time.Time           `json:"attempted_at"`
	CompletedAt           time.Time           `json:"completed_at,omitempty"`
	Result                string              `json:"result"`
	LatencyMS             int64               `json:"latency_ms"`
	FailureCode           string              `json:"failure_code"`
}

type wakeCanaryReceipt struct {
	Turn              int       `json:"turn"`
	DeliveryID        string    `json:"delivery_id"`
	NonceDigest       string    `json:"nonce_digest"`
	SubmittedAt       time.Time `json:"submitted_at"`
	WindowStartedAt   time.Time `json:"window_started_at"`
	WindowCompletedAt time.Time `json:"window_completed_at"`
}

type wakeCanaryReceiptEvent struct {
	SchemaVersion int       `json:"schema_version"`
	Event         string    `json:"event"`
	DeliveryID    string    `json:"delivery_id"`
	Session       string    `json:"session"`
	Runtime       string    `json:"runtime"`
	SubmittedAt   time.Time `json:"submitted_at"`
}

type wakeCanaryRoles struct {
	MayorPreset     string
	MayorProvider   string
	PolecatPreset   string
	PolecatProvider string
}

func configureWakeCanarySandboxRoles(sandbox *wakeCanarySandbox, sourceTownRoot, sourceRigPath string) (wakeCanaryRoles, error) {
	mayor := config.ResolveRoleAgentConfig(constants.RoleMayor, sourceTownRoot, sourceRigPath)
	polecat := config.ResolveRoleAgentConfig(constants.RolePolecat, sourceTownRoot, sourceRigPath)
	roles := wakeCanaryRoles{
		MayorPreset: mayor.ResolvedAgent, MayorProvider: mayor.Provider,
		PolecatPreset: polecat.ResolvedAgent, PolecatProvider: polecat.Provider,
	}
	if mayor.Provider != "codex" || polecat.Provider != "codex" {
		return roles, fmt.Errorf("wake canary requires Codex-backed Mayor and polecat presets")
	}
	canaryMayor := *mayor
	canaryMayor.Args = append([]string(nil), mayor.Args...)
	if !slices.Contains(canaryMayor.Args, "--dangerously-bypass-hook-trust") {
		canaryMayor.Args = append(canaryMayor.Args, "--dangerously-bypass-hook-trust")
	}
	settings := config.NewTownSettings()
	settings.Agents[polecat.ResolvedAgent] = polecat
	settings.Agents[mayor.ResolvedAgent] = &canaryMayor
	settings.RoleAgents = map[string]string{
		constants.RoleMayor: mayor.ResolvedAgent, constants.RolePolecat: polecat.ResolvedAgent,
	}
	settings.Operational = &config.OperationalConfig{Mail: &config.MailThresholds{ReplyReminderDelay: "0s"}}
	return roles, config.SaveTownSettings(config.TownSettingsPath(sandbox.TownRoot), settings)
}

type wakeCanarySandbox struct {
	TownRoot         string
	WorkDir          string
	RuntimeConfigDir string
	Socket           string
	Session          string
	tmux             *tmux.Tmux
}

type wakeCanaryIdleWaiter interface {
	WaitForIdle(session string, timeout time.Duration) error
}

type wakeCanaryIdleObserver interface {
	ObserveIdle(session string) (tmux.IdleObservation, error)
}

type wakeCanaryTurnWaiter interface {
	wakeCanaryIdleWaiter
	WaitForResponse(sessionName, baseline, response string, timeout time.Duration) error
}

type tmuxWakeCanaryTurnWaiter struct {
	tmux *tmux.Tmux
}

func (w tmuxWakeCanaryTurnWaiter) WaitForIdle(session string, timeout time.Duration) error {
	return w.tmux.WaitForIdle(session, timeout)
}

func (w tmuxWakeCanaryTurnWaiter) WaitForResponse(sessionName, baseline, response string, timeout time.Duration) error {
	return waitForCanaryResponse(w.tmux, sessionName, baseline, response, timeout)
}

func waitForWakeCanaryIdle(waiter wakeCanaryIdleWaiter, sessionName string) error {
	return waiter.WaitForIdle(sessionName, constants.ClaudeStartTimeout)
}

func confirmWakeCanaryTurn(waiter wakeCanaryTurnWaiter, sessionName, baseline, response string) (string, error) {
	if err := waiter.WaitForResponse(sessionName, baseline, response, 2*constants.ClaudeStartTimeout); err != nil {
		return "model-turn-unconfirmed", err
	}
	if err := waitForWakeCanaryIdle(waiter, sessionName); err != nil {
		return "model-turn-not-idle", fmt.Errorf("waiting for completed model turn steady-state idle: %w", err)
	}
	return "", nil
}

func annotateWakeCanaryIdleFailure(observer wakeCanaryIdleObserver, sessionName string, cause error) error {
	observation, err := observer.ObserveIdle(sessionName)
	if err != nil {
		return fmt.Errorf("%w; idle_observation=unavailable", cause)
	}
	return fmt.Errorf("%w; idle_observation={%s}", cause, observation.String())
}

func confirmWakeCanaryDelivery(outcome witness.MayorNotificationOutcome, confirm func() (string, error)) (string, error) {
	switch outcome {
	case witness.MayorNotificationSubmitted:
		return confirm()
	case witness.MayorNotificationQueued:
		return "notification-queued", errors.New("Mayor notification was queued")
	case witness.MayorNotificationWakeFailed:
		return "notification-failed", errors.New("Mayor notification wake failed")
	default:
		return "notification-failed", fmt.Errorf("unexpected Mayor notification outcome %q", outcome)
	}
}

func newWakeCanarySandbox(parent, gtBin string) (*wakeCanarySandbox, error) {
	townRoot, err := os.MkdirTemp(parent, "gt-wake-canary-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(townRoot) }
	if err := os.Chmod(townRoot, 0700); err != nil {
		cleanup()
		return nil, err
	}
	workDir := filepath.Join(townRoot, "mayor", "rig")
	runtimeConfigDir := filepath.Join(townRoot, ".codex")
	for _, dir := range []string{workDir, runtimeConfigDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			cleanup()
			return nil, err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			cleanup()
			return nil, err
		}
	}
	cmd := exec.Command("bd", "init", "--non-interactive", "--quiet", "--prefix", "canary", "--skip-agents", "--skip-hooks")
	cmd.Dir = townRoot
	cmd.Env = isolatedCanaryEnv()
	if err := cmd.Run(); err != nil {
		cleanup()
		return nil, fmt.Errorf("initializing isolated Dolt database: %w", err)
	}
	if err := hooks.InstallForRoleWithGTBinary("codex", townRoot, townRoot, "mayor", ".codex", "hooks.json", false, gtBin); err != nil {
		cleanup()
		return nil, fmt.Errorf("installing temporary Codex hooks: %w", err)
	}
	if err := atomicfile.WriteFile(filepath.Join(runtimeConfigDir, "config.toml"), []byte("[notice]\nhide_rate_limit_model_nudge = true\n\n[features]\nhooks = true\n"), 0600); err != nil {
		cleanup()
		return nil, fmt.Errorf("writing temporary Codex settings: %w", err)
	}
	id := nudge.NewDeliveryID()
	socket := "gt-wake-canary-" + id
	return &wakeCanarySandbox{
		TownRoot: townRoot, WorkDir: workDir, RuntimeConfigDir: runtimeConfigDir,
		Socket: socket, Session: session.MayorSessionName(),
		tmux: tmux.NewTmuxWithSocketAndEnv(socket, isolatedCanaryEnv()),
	}, nil
}

func isolatedCanaryEnv() []string {
	return append(filterIsolatedCanaryEnv(os.Environ()), "BD_NON_INTERACTIVE=1", "CI=1")

}

func filterIsolatedCanaryEnv(environ []string) []string {
	env := make([]string, 0, len(environ))
	for _, entry := range environ {
		if strings.HasPrefix(entry, "GT_") || strings.HasPrefix(entry, "BD_") ||
			strings.HasPrefix(entry, "BEADS_") || strings.HasPrefix(entry, "DOLT_") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func (s *wakeCanarySandbox) linkCodexAuth() error {
	sourceRoot := os.Getenv("CODEX_HOME")
	if sourceRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		sourceRoot = filepath.Join(home, ".codex")
	}
	source := filepath.Join(sourceRoot, "auth.json")
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("Codex authentication unavailable: %w", err)
	}
	if err := os.Symlink(source, filepath.Join(s.RuntimeConfigDir, "auth.json")); err != nil {
		return fmt.Errorf("linking temporary Codex authentication: %w", err)
	}
	return nil
}

func (s *wakeCanarySandbox) Cleanup() error {
	cleanupErr := nudge.StopPoller(s.TownRoot, s.Session)
	if s.tmux != nil {
		cleanupErr = errors.Join(cleanupErr, s.tmux.KillSessionWithProcesses(s.Session))
		cleanupErr = errors.Join(cleanupErr, s.tmux.KillServer())
	}
	if s.Socket != "" {
		cleanupErr = errors.Join(cleanupErr, removeWakeCanarySocketPath(s.Socket))
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	return os.RemoveAll(s.TownRoot)
}

func runWakeCanaryWithCleanup(sandbox *wakeCanarySandbox, run func() error) (err error) {
	defer func() {
		err = errors.Join(err, sandbox.Cleanup())
	}()
	return run()
}

func init() {
	rootCmd.AddCommand(nudgeCanaryCmd)
	nudgeCanaryCmd.Flags().BoolVar(&nudgeCanaryConfirm, "confirm-live", false, "Confirm 20 wake turns in an isolated Codex Mayor session")
}

var nudgeCanaryCmd = &cobra.Command{
	Use:         "nudge-canary",
	Short:       "Run an opt-in receipt-verified Mayor wake canary",
	GroupID:     GroupComm,
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Args:        cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if !nudgeCanaryConfirm {
			return fmt.Errorf("isolated Mayor canary requires --confirm-live")
		}
		evidenceRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		gtBin, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolving gt executable for canary hooks: %w", err)
		}
		sandbox, err := newWakeCanarySandbox("", gtBin)
		if err != nil {
			return err
		}
		return runWakeCanaryWithCleanup(sandbox, func() error {
			roles, err := configureWakeCanarySandboxRoles(sandbox, evidenceRoot, "")
			if err != nil {
				return err
			}
			if err := sandbox.linkCodexAuth(); err != nil {
				return err
			}
			startupInstruction, startupResponse := wakeCanaryStartupChallenge(strings.TrimPrefix(nudge.NewDeliveryID(), "ndg-"))
			sessionConfig, err := wakeCanarySessionConfig(sandbox, startupInstruction)
			if err != nil {
				return err
			}
			if _, err := session.StartSession(sandbox.tmux, sessionConfig); err != nil {
				return fmt.Errorf("starting isolated Codex Mayor: %w", err)
			}
			if err := waitForCanaryResponse(sandbox.tmux, sandbox.Session, "", startupResponse, constants.ClaudeStartTimeout); err != nil {
				return fmt.Errorf("confirming isolated Mayor startup turn: %w", err)
			}
			result, statePath, err := runWakeCanary(sandbox.tmux, sandbox.TownRoot, evidenceRoot, sandbox.Session, wakeCanaryTurns, roles)
			fmt.Printf("Mayor wake canary: %d/%d submitted, %d queued, %d failed\nState: %s\n", result.Submitted, result.Turns, result.Queued, result.Failed, statePath)
			return err
		})
	},
}

func wakeCanarySessionConfig(sandbox *wakeCanarySandbox, startupInstruction string) (session.SessionConfig, error) {
	runtimeConfig := config.ResolveRoleAgentConfig(constants.RoleMayor, sandbox.TownRoot, sandbox.WorkDir)
	if runtimeConfig.Provider != "codex" {
		return session.SessionConfig{}, fmt.Errorf("wake canary Mayor preset must use the Codex provider")
	}
	cfg := session.SessionConfig{
		SessionID: sandbox.Session, WorkDir: sandbox.WorkDir, Role: "mayor",
		TownRoot: sandbox.TownRoot, AgentOverride: runtimeConfig.ResolvedAgent, RuntimeConfigDir: sandbox.RuntimeConfigDir,
		ExtraEnv:         map[string]string{"GT_TOWN_ROOT": sandbox.TownRoot, "CODEX_HOME": sandbox.RuntimeConfigDir},
		StripEnvPrefixes: []string{"GT_DOLT_", "BD_", "BEADS_", "DOLT_"},
		Beacon:           session.BeaconConfig{Recipient: "isolated wake-canary mayor", Sender: "self", Topic: "canary"},
		Instructions:     startupInstruction,
		WaitForAgent:     true, WaitFatal: true, AcceptBypass: true, ReadyDelay: true, VerifySurvived: true,
	}
	return cfg, nil
}

func wakeCanaryStartupChallenge(nonce string) (string, string) {
	return fmt.Sprintf("Reply with exactly the reverse of nonce %s.", nonce), reverseString(nonce)
}

func runWakeCanary(t *tmux.Tmux, runtimeTownRoot, evidenceRoot, sessionName string, turns int, roles wakeCanaryRoles) (wakeCanaryResult, string, error) {
	result := wakeCanaryResult{
		ID: "wake-" + nudge.NewDeliveryID(), Session: sessionName, Turns: turns, StartedAt: time.Now(),
	}
	state := wakeCanaryState{
		SchemaVersion: 3, InstalledBinaryCommit: resolveCommitHash(),
		MayorPreset: roles.MayorPreset, MayorProvider: roles.MayorProvider,
		PolecatPreset: roles.PolecatPreset, PolecatProvider: roles.PolecatProvider,
		Session: sessionName, Runtime: "codex", Turns: turns,
		AttemptedAt: result.StartedAt, Result: "running", Receipts: []wakeCanaryReceipt{},
	}
	statePath, err := writeWakeCanaryState(evidenceRoot, state)
	if err != nil {
		return result, statePath, err
	}
	fail := func(code string, cause error) (wakeCanaryResult, string, error) {
		state.Result = "failed"
		state.FailureCode = code
		state.Submitted = result.Submitted
		state.Queued = result.Queued
		state.Failed = result.Failed
		state.ReceiptCount = len(state.Receipts)
		state.ReceiptDigest = wakeCanaryDigest(state.Receipts)
		state.LatencyMS = time.Since(result.StartedAt).Milliseconds()
		_ = writeWakeCanaryStateAt(statePath, state)
		return result, statePath, cause
	}
	if sessionName != session.MayorSessionName() {
		return fail("identity-not-isolated", fmt.Errorf("wake canary requires an isolated Mayor identity"))
	}
	if t == nil || !t.IsIsolated() {
		return fail("tmux-not-isolated", fmt.Errorf("wake canary requires an isolated tmux socket"))
	}
	if state.InstalledBinaryCommit == "" {
		return fail("binary-commit-unknown", fmt.Errorf("wake canary requires an installed binary commit"))
	}
	if err := waitForWakeCanaryIdle(t, sessionName); err != nil {
		return fail("session-not-idle", fmt.Errorf("waiting for isolated Mayor steady-state idle: %w", err))
	}
	if err := t.ArmClientAttachmentLatch(sessionName); err != nil {
		return fail("client-latch-failed", fmt.Errorf("arming client attachment latch: %w", err))
	}
	generation, err := t.CaptureSessionGeneration(sessionName)
	if err != nil {
		return fail("session-authority-failed", fmt.Errorf("capturing isolated Mayor authority: %w", err))
	}
	state.SessionAuthority = wakeCanaryDigest(generation)
	router := mail.NewRouterWithTownRootAndTmux(runtimeTownRoot, runtimeTownRoot, t)

	for turn := 1; turn <= turns; turn++ {
		observed, authorityErr := t.CaptureSessionGeneration(sessionName)
		if authorityErr != nil || !generation.Equal(observed) {
			result.Failed++
			return fail("session-authority-changed", fmt.Errorf("isolated Mayor session authority changed before turn %d", turn))
		}
		attached, attachmentErr := t.ClientAttachmentObserved(sessionName)
		if attachmentErr != nil {
			result.Failed++
			return fail("client-proof-failed", attachmentErr)
		}
		if attached {
			result.Failed++
			return fail("human-client-attached", fmt.Errorf("wake canary tmux session has an attached client"))
		}
		nonce := strings.TrimPrefix(nudge.NewDeliveryID(), "ndg-")
		response := reverseString(nonce)
		before, captureErr := t.CapturePaneAll(sessionName)
		if captureErr != nil {
			result.Failed++
			return fail("transcript-baseline-failed", captureErr)
		}
		releaseLease, leaseErr := t.AcquireNudgeLease(runtimeTownRoot, sessionName)
		if leaseErr != nil {
			result.Failed++
			return fail("nudge-lock-contended", fmt.Errorf("acquiring exclusive canary nudge lease: %w", leaseErr))
		}
		windowStartedAt := time.Now().UTC()
		outcome, sendErr := witness.DeliverMayorNotification(router,
			fmt.Sprintf("Wake canary %d/%d: reverse nonce %s", turn, turns, nonce),
			"Reply in the active model turn with the nonce reversed.")
		releaseLease()
		if sendErr != nil {
			result.Failed++
			return fail("mail-send-failed", sendErr)
		}
		failureCode, confirmErr := confirmWakeCanaryDelivery(outcome, func() (string, error) {
			return confirmWakeCanaryTurn(tmuxWakeCanaryTurnWaiter{tmux: t}, sessionName, before, response)
		})
		if confirmErr != nil {
			if failureCode == "model-turn-not-idle" {
				confirmErr = annotateWakeCanaryIdleFailure(t, sessionName, confirmErr)
			}
			if outcome == witness.MayorNotificationQueued {
				result.Queued++
			}
			result.Failed++
			return fail(failureCode, confirmErr)
		}
		attached, attachmentErr = t.ClientAttachmentObserved(sessionName)
		if attachmentErr != nil {
			result.Failed++
			return fail("client-proof-failed", attachmentErr)
		}
		if attached {
			result.Failed++
			return fail("human-client-attached", fmt.Errorf("canary tmux client attached during model turn"))
		}
		windowCompletedAt := time.Now().UTC()
		observed, authorityErr = t.CaptureSessionGeneration(sessionName)
		if authorityErr != nil || !generation.Equal(observed) {
			result.Failed++
			return fail("session-authority-changed", fmt.Errorf("isolated Mayor session authority changed during turn %d", turn))
		}
		events, receiptErr := readWakeCanaryReceiptEvents(runtimeTownRoot, sessionName)
		if receiptErr != nil {
			result.Failed++
			return fail("receipt-read-failed", receiptErr)
		}
		state.Receipts, receiptErr = validateWakeCanaryReceiptEvents(events, state.Receipts, sessionName, turn, nonce, windowStartedAt, windowCompletedAt)
		if receiptErr != nil {
			result.Failed++
			return fail("receipt-proof-failed", receiptErr)
		}
		result.Submitted++
		state.Submitted = result.Submitted
		state.Queued = result.Queued
		state.Failed = result.Failed
		state.ReceiptCount = len(state.Receipts)
		state.ReceiptDigest = wakeCanaryDigest(state.Receipts)
		if err := writeWakeCanaryStateAt(statePath, state); err != nil {
			return result, statePath, err
		}
	}

	result.CompletedAt = time.Now().UTC()
	if result.Submitted != turns || result.Queued != 0 || result.Failed != 0 {
		return fail("count-mismatch", fmt.Errorf("wake canary failed: %d/%d submitted, %d queued, %d failed", result.Submitted, turns, result.Queued, result.Failed))
	}
	state.Result = "passed"
	state.FailureCode = ""
	state.CompletedAt = result.CompletedAt
	state.LatencyMS = time.Since(result.StartedAt).Milliseconds()
	if err := writeWakeCanaryStateAt(statePath, state); err != nil {
		return result, statePath, err
	}
	return result, statePath, nil
}

func readWakeCanaryReceiptEvents(townRoot, sessionName string) ([]wakeCanaryReceiptEvent, error) {
	f, err := os.Open(delivery.ReceiptPath(townRoot, sessionName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading private canary receipts: %w", err)
	}
	defer f.Close()

	var events []wakeCanaryReceiptEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event wakeCanaryReceiptEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("invalid private canary receipt")
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading private canary receipts: %w", err)
	}
	return events, nil
}

func validateWakeCanaryReceiptEvents(events []wakeCanaryReceiptEvent, previous []wakeCanaryReceipt, sessionName string, turn int, nonce string, startedAt, completedAt time.Time) ([]wakeCanaryReceipt, error) {
	if len(events) != len(previous)+1 || turn != len(previous)+1 {
		return nil, fmt.Errorf("turn %d requires exactly one new receipt", turn)
	}
	seen := make(map[string]struct{}, len(events))
	for index, event := range events {
		if event.SchemaVersion != 1 || event.Event != "prompt_submitted" || event.Session != sessionName || event.Runtime != "codex" || event.DeliveryID == "" || event.SubmittedAt.IsZero() {
			return nil, fmt.Errorf("turn %d has invalid receipt authority", turn)
		}
		if _, duplicate := seen[event.DeliveryID]; duplicate {
			return nil, fmt.Errorf("turn %d has a duplicate receipt", turn)
		}
		seen[event.DeliveryID] = struct{}{}
		if index < len(previous) && (event.DeliveryID != previous[index].DeliveryID || !event.SubmittedAt.Equal(previous[index].SubmittedAt)) {
			return nil, fmt.Errorf("turn %d changed prior receipt evidence", turn)
		}
	}
	current := events[len(events)-1]
	if !current.SubmittedAt.After(startedAt) || current.SubmittedAt.After(completedAt) {
		return nil, fmt.Errorf("turn %d receipt is outside its authority window", turn)
	}
	nonceDigest := wakeCanaryDigest(nonce)
	for _, receipt := range previous {
		if receipt.NonceDigest == nonceDigest {
			return nil, fmt.Errorf("turn %d reused a nonce", turn)
		}
	}
	receipts := append([]wakeCanaryReceipt(nil), previous...)
	receipts = append(receipts, wakeCanaryReceipt{
		Turn: turn, DeliveryID: current.DeliveryID, NonceDigest: nonceDigest,
		SubmittedAt: current.SubmittedAt, WindowStartedAt: startedAt, WindowCompletedAt: completedAt,
	})
	return receipts, nil
}

func wakeCanaryDigest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func reverseString(value string) string {
	runes := []rune(value)
	for left, right := 0, len(runes)-1; left < right; left, right = left+1, right-1 {
		runes[left], runes[right] = runes[right], runes[left]
	}
	return string(runes)
}

func waitForCanaryResponse(t *tmux.Tmux, sessionName, baseline, response string, timeout time.Duration) error {
	if strings.Contains(baseline, response) {
		return fmt.Errorf("canary response was present before notification")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pane, err := t.CapturePaneAll(sessionName)
		if err != nil {
			return err
		}
		if strings.Contains(pane, response) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("model turn did not contain the nonce response within %s", timeout)
}

func writeWakeCanaryState(townRoot string, state wakeCanaryState) (string, error) {
	path := filepath.Join(townRoot, constants.DirRuntime, "canary", "control-plane.json")
	return path, writeWakeCanaryStateAt(path, state)
}

func writeWakeCanaryStateAt(path string, state wakeCanaryState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0600)
}
