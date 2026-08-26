package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/witness"
)

type wakeCanaryIdleWaiterStub struct {
	session string
	timeout time.Duration
	err     error
}

type wakeCanaryTurnWaiterStub struct {
	events          []string
	responseTimeout time.Duration
	idleTimeout     time.Duration
	responseErr     error
	idleErr         error
}

type wakeCanaryIdleObserverStub struct {
	observation tmux.IdleObservation
	err         error
}

func (s wakeCanaryIdleObserverStub) ObserveIdle(string) (tmux.IdleObservation, error) {
	return s.observation, s.err
}

func (s *wakeCanaryTurnWaiterStub) WaitForResponse(_, _, _ string, timeout time.Duration) error {
	s.events = append(s.events, "response")
	s.responseTimeout = timeout
	return s.responseErr
}

func (s *wakeCanaryTurnWaiterStub) WaitForIdle(_ string, timeout time.Duration) error {
	s.events = append(s.events, "idle")
	s.idleTimeout = timeout
	return s.idleErr
}

func buildWakeCanaryCandidateGT(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	candidate := filepath.Join(buildDir, "gt")
	cmd := exec.Command("make", "build", "BUILD_DIR="+buildDir)
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build candidate gt: %v: %s", err, output)
	}
	if output, err := exec.Command(candidate, "version").CombinedOutput(); err != nil {
		t.Fatalf("run candidate gt: %v: %s", err, output)
	}
	return candidate
}

func (s *wakeCanaryIdleWaiterStub) WaitForIdle(session string, timeout time.Duration) error {
	s.session = session
	s.timeout = timeout
	return s.err
}

func TestWaitForWakeCanaryIdleUsesStartupTurnBound(t *testing.T) {
	cause := errors.New("session busy")
	waiter := &wakeCanaryIdleWaiterStub{err: cause}

	err := waitForWakeCanaryIdle(waiter, session.MayorSessionName())
	if !errors.Is(err, cause) {
		t.Fatalf("waitForWakeCanaryIdle error = %v, want %v", err, cause)
	}
	if waiter.session != session.MayorSessionName() {
		t.Fatalf("WaitForIdle session = %q, want %q", waiter.session, session.MayorSessionName())
	}
	if waiter.timeout != constants.ClaudeStartTimeout {
		t.Fatalf("WaitForIdle timeout = %s, want %s", waiter.timeout, constants.ClaudeStartTimeout)
	}
}

func TestConfirmWakeCanaryTurnWaitsForSteadyIdleBeforeNextDelivery(t *testing.T) {
	waiter := &wakeCanaryTurnWaiterStub{}

	code, err := confirmWakeCanaryTurn(waiter, session.MayorSessionName(), "before", "response")
	waiter.events = append(waiter.events, "next-delivery")

	if err != nil || code != "" {
		t.Fatalf("confirmWakeCanaryTurn = (%q, %v), want success", code, err)
	}
	if got, want := strings.Join(waiter.events, ","), "response,idle,next-delivery"; got != want {
		t.Fatalf("turn ordering = %q, want %q", got, want)
	}
	if waiter.responseTimeout != 2*constants.ClaudeStartTimeout || waiter.idleTimeout != constants.ClaudeStartTimeout {
		t.Fatalf("turn bounds = response %s idle %s", waiter.responseTimeout, waiter.idleTimeout)
	}
}

func TestAnnotateWakeCanaryIdleFailureAddsOnlyStructuralObservation(t *testing.T) {
	t.Parallel()

	cause := errors.New("steady idle timeout")
	observer := wakeCanaryIdleObserverStub{observation: tmux.IdleObservation{
		Idle: true, PromptRows: 1, PromptOnCursor: true, CursorX: 1, CursorY: 7,
	}}
	err := annotateWakeCanaryIdleFailure(observer, session.MayorSessionName(), cause)

	if !errors.Is(err, cause) {
		t.Fatalf("annotated error = %v, want wrapped cause %v", err, cause)
	}
	for _, want := range []string{"idle=true", "prompt_rows=1", "prompt_on_cursor=true", "cursor_x=1", "cursor_y=7"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("annotated error missing %q: %v", want, err)
		}
	}
}

func TestConfirmWakeCanaryTurnDistinguishesResponseAndIdleFailures(t *testing.T) {
	responseErr := errors.New("response missing")
	idleErr := errors.New("turn still active")
	for _, tt := range []struct {
		name       string
		waiter     *wakeCanaryTurnWaiterStub
		wantCode   string
		wantErr    error
		wantEvents string
	}{
		{name: "response unconfirmed", waiter: &wakeCanaryTurnWaiterStub{responseErr: responseErr}, wantCode: "model-turn-unconfirmed", wantErr: responseErr, wantEvents: "response"},
		{name: "response observed but not idle", waiter: &wakeCanaryTurnWaiterStub{idleErr: idleErr}, wantCode: "model-turn-not-idle", wantErr: idleErr, wantEvents: "response,idle"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, err := confirmWakeCanaryTurn(tt.waiter, session.MayorSessionName(), "before", "response")
			if code != tt.wantCode || !errors.Is(err, tt.wantErr) {
				t.Fatalf("confirmWakeCanaryTurn = (%q, %v), want (%q, %v)", code, err, tt.wantCode, tt.wantErr)
			}
			if got := strings.Join(tt.waiter.events, ","); got != tt.wantEvents {
				t.Fatalf("turn events = %q, want %q", got, tt.wantEvents)
			}
		})
	}
}

func TestConfirmWakeCanaryDeliveryRejectsQueuedOutcome(t *testing.T) {
	confirmed := false
	code, err := confirmWakeCanaryDelivery(witness.MayorNotificationQueued, func() (string, error) {
		confirmed = true
		return "", nil
	})
	if err == nil || code != "notification-queued" || confirmed {
		t.Fatalf("queued confirmation = (%q, %v, called=%v), want fail-closed without turn confirmation", code, err, confirmed)
	}
}

func TestWakeCanaryStartupChallengeRequiresFiniteReply(t *testing.T) {
	instruction, response := wakeCanaryStartupChallenge("abc123")
	if response != "321cba" {
		t.Fatalf("startup response = %q, want reversed nonce", response)
	}
	if want := "Reply with exactly the reverse of nonce abc123."; instruction != want {
		t.Fatalf("startup instruction = %q, want one finite request %q", instruction, want)
	}
	if strings.Contains(instruction, response) {
		t.Fatal("startup instruction contains the expected response before the model turn")
	}
}

func TestValidateWakeCanaryReceiptEventsRejectsWrongDuplicateAndLateEvidence(t *testing.T) {
	startedAt := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(time.Second)
	valid := wakeCanaryReceiptEvent{
		SchemaVersion: 1, Event: "prompt_submitted", DeliveryID: "ndg-first",
		Session: session.MayorSessionName(), Runtime: "codex", SubmittedAt: startedAt.Add(time.Millisecond),
	}
	receipts, err := validateWakeCanaryReceiptEvents(
		[]wakeCanaryReceiptEvent{valid}, nil, session.MayorSessionName(), 1, "private-nonce", startedAt, completedAt,
	)
	if err != nil || len(receipts) != 1 || receipts[0].NonceDigest != wakeCanaryDigest("private-nonce") {
		t.Fatalf("valid receipt proof = %#v, %v", receipts, err)
	}

	tests := []struct {
		name     string
		events   []wakeCanaryReceiptEvent
		previous []wakeCanaryReceipt
		turn     int
		nonce    string
	}{
		{name: "duplicate", events: []wakeCanaryReceiptEvent{valid, valid}, previous: receipts, turn: 2},
		{name: "duplicate nonce", events: []wakeCanaryReceiptEvent{valid, {SchemaVersion: 1, Event: "prompt_submitted", DeliveryID: "ndg-second", Session: session.MayorSessionName(), Runtime: "codex", SubmittedAt: startedAt.Add(2 * time.Millisecond)}}, previous: receipts, turn: 2, nonce: "private-nonce"},
		{name: "wrong session", events: []wakeCanaryReceiptEvent{{SchemaVersion: 1, Event: "prompt_submitted", DeliveryID: "ndg-wrong-session", Session: "live", Runtime: "codex", SubmittedAt: valid.SubmittedAt}}, turn: 1},
		{name: "wrong runtime", events: []wakeCanaryReceiptEvent{{SchemaVersion: 1, Event: "prompt_submitted", DeliveryID: "ndg-wrong-runtime", Session: session.MayorSessionName(), Runtime: "claude", SubmittedAt: valid.SubmittedAt}}, turn: 1},
		{name: "late", events: []wakeCanaryReceiptEvent{{SchemaVersion: 1, Event: "prompt_submitted", DeliveryID: "ndg-late", Session: session.MayorSessionName(), Runtime: "codex", SubmittedAt: completedAt.Add(time.Nanosecond)}}, turn: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nonce := tt.nonce
			if nonce == "" {
				nonce = "next-private-nonce"
			}
			if got, err := validateWakeCanaryReceiptEvents(tt.events, tt.previous, session.MayorSessionName(), tt.turn, nonce, startedAt, completedAt); err == nil {
				t.Fatalf("invalid receipt proof accepted: %#v", got)
			}
		})
	}
}

func TestRunWakeCanaryPersistsSessionNotIdleBeforeLease(t *testing.T) {
	previousCommit := Commit
	Commit = "test-commit"
	t.Cleanup(func() { Commit = previousCommit })

	socket := "gt-wake-canary-" + nudge.NewDeliveryID()
	tm := tmux.NewTmuxWithSocket(socket)
	socketPath := filepath.Join(tmux.SocketDir(), socket)
	runtimeTownRoot := t.TempDir()
	sessionName := session.MayorSessionName()
	t.Cleanup(func() {
		if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
			t.Errorf("test tmux socket still exists after cleanup: %v", err)
		}
	})
	t.Cleanup(func() {
		sandbox := &wakeCanarySandbox{TownRoot: runtimeTownRoot, Socket: socket, Session: sessionName, tmux: tm}
		if err := sandbox.Cleanup(); err != nil {
			t.Errorf("cleanup test wake canary sandbox: %v", err)
		}
	})

	evidenceRoot := t.TempDir()
	if err := tm.NewSessionWithCommand(sessionName, t.TempDir(), "sleep 60"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}

	release, err := tm.AcquireNudgeLease(runtimeTownRoot, sessionName)
	if err != nil {
		t.Fatalf("AcquireNudgeLease: %v", err)
	}
	t.Cleanup(release)

	type canaryResult struct {
		result    wakeCanaryResult
		statePath string
		err       error
	}
	done := make(chan canaryResult, 1)
	go func() {
		result, statePath, err := runWakeCanary(tm, runtimeTownRoot, evidenceRoot, sessionName, 1, wakeCanaryRoles{})
		done <- canaryResult{result: result, statePath: statePath, err: err}
	}()

	statePath := filepath.Join(evidenceRoot, constants.DirRuntime, "canary", "control-plane.json")
	deadline := time.NewTimer(5 * time.Second)
	t.Cleanup(func() { deadline.Stop() })
	poll := time.NewTicker(10 * time.Millisecond)
	t.Cleanup(poll.Stop)
	for {
		select {
		case <-poll.C:
			data, readErr := os.ReadFile(statePath)
			if readErr != nil {
				continue
			}
			var state wakeCanaryState
			if json.Unmarshal(data, &state) == nil && state.Result == "running" {
				goto waiting
			}
		case <-deadline.C:
			t.Fatal("runWakeCanary did not persist running state")
		}
	}

waiting:
	if err := tm.KillSession(sessionName); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	select {
	case got := <-done:
		if got.err == nil || !strings.Contains(got.err.Error(), "steady-state idle") {
			t.Fatalf("runWakeCanary error = %v, want steady-state idle failure", got.err)
		}
		if got.result.Submitted != 0 || got.result.Queued != 0 || got.result.Failed != 0 {
			t.Fatalf("runWakeCanary result = %+v, want zero delivery attempts", got.result)
		}
		data, readErr := os.ReadFile(got.statePath)
		if readErr != nil {
			t.Fatalf("read failed canary state: %v", readErr)
		}
		var state wakeCanaryState
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatalf("decode failed canary state: %v", err)
		}
		if state.Result != "failed" || state.FailureCode != "session-not-idle" {
			t.Fatalf("failed canary state = %+v, want session-not-idle", state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runWakeCanary did not stop after the session disappeared")
	}
}

func TestRunWakeCanaryRejectsLiveMayorIdentity(t *testing.T) {
	evidenceRoot := t.TempDir()
	_, statePath, err := runWakeCanary(nil, t.TempDir(), evidenceRoot, "not-the-isolated-mayor", wakeCanaryTurns, wakeCanaryRoles{})
	if err == nil || !strings.Contains(err.Error(), "isolated") {
		t.Fatalf("runWakeCanary live Mayor error = %v, want isolated identity rejection", err)
	}
	data, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read failed canary state: %v", readErr)
	}
	if !strings.Contains(string(data), `"result": "failed"`) || !strings.Contains(string(data), `"failure_code": "identity-not-isolated"`) {
		t.Fatalf("failed canary state = %s", data)
	}
}

func TestRunWakeCanaryPersistsConfiguredRoleSnapshot(t *testing.T) {
	previousCommit := Commit
	Commit = "test-commit"
	t.Cleanup(func() { Commit = previousCommit })

	sourceTownRoot := t.TempDir()
	settings := config.NewTownSettings()
	settings.Agents["preset-a-mayor"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.Agents["preset-a-polecat"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.RoleAgents = map[string]string{
		constants.RoleMayor: "preset-a-mayor", constants.RolePolecat: "preset-a-polecat",
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(sourceTownRoot), settings); err != nil {
		t.Fatalf("save preset A: %v", err)
	}
	sandbox := &wakeCanarySandbox{TownRoot: t.TempDir(), WorkDir: t.TempDir()}
	roles, err := configureWakeCanarySandboxRoles(sandbox, sourceTownRoot, "")
	if err != nil {
		t.Fatalf("configure preset A: %v", err)
	}

	settings.Agents["preset-b-mayor"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.Agents["preset-b-polecat"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.RoleAgents[constants.RoleMayor] = "preset-b-mayor"
	settings.RoleAgents[constants.RolePolecat] = "preset-b-polecat"
	if err := config.SaveTownSettings(config.TownSettingsPath(sourceTownRoot), settings); err != nil {
		t.Fatalf("save preset B: %v", err)
	}

	_, statePath, err := runWakeCanary(nil, t.TempDir(), sourceTownRoot, "not-isolated", 1, roles)
	if err == nil {
		t.Fatal("runWakeCanary accepted non-isolated identity")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read canary state: %v", err)
	}
	var state wakeCanaryState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode canary state: %v", err)
	}
	if state.MayorPreset != "preset-a-mayor" || state.PolecatPreset != "preset-a-polecat" {
		t.Fatalf("state certified mutable roles: %+v, want preset A snapshot", state)
	}
}

func TestIsolatedCanaryEnvStripsLiveRouting(t *testing.T) {
	got := filterIsolatedCanaryEnv([]string{
		"PATH=/usr/bin",
		"GT_DOLT_HOST=live",
		"BD_DB=live",
		"BEADS_DOLT_SERVER_PORT=3306",
		"DOLT_ROOT_PATH=/live",
	})
	if strings.Join(got, "\n") != "PATH=/usr/bin" {
		t.Fatalf("isolated environment retained live routing: %v", got)
	}
}

func TestNewWakeCanarySandboxIsPrivateAndIsolated(t *testing.T) {
	parent := t.TempDir()
	candidateGT := filepath.Join(parent, "candidate", "gt")
	sandbox, err := newWakeCanarySandbox(parent, candidateGT)
	if err != nil {
		t.Fatalf("newWakeCanarySandbox: %v", err)
	}
	root := sandbox.TownRoot
	defer sandbox.Cleanup()

	if sandbox.Session != session.MayorSessionName() {
		t.Fatalf("canary session = %q, want dedicated mayor identity", sandbox.Session)
	}
	if _, err := os.Stat(filepath.Join(sandbox.TownRoot, ".beads")); err != nil {
		t.Fatalf("isolated Dolt database missing: %v", err)
	}
	hooksPath := filepath.Join(sandbox.RuntimeConfigDir, "hooks.json")
	hooksData, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("temporary Codex hooks missing: %v", err)
	}
	if !strings.Contains(string(hooksData), "UserPromptSubmit") || !strings.Contains(string(hooksData), "mail check --inject") {
		t.Fatalf("temporary Codex hooks lack receipt dispatcher: %s", hooksData)
	}
	var installed struct {
		Hooks struct {
			SessionStart []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"SessionStart"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(hooksData, &installed); err != nil {
		t.Fatalf("parse temporary Codex hooks: %v", err)
	}
	if len(installed.Hooks.SessionStart) != 1 || len(installed.Hooks.SessionStart[0].Hooks) != 1 {
		t.Fatal("temporary Codex hooks lack one SessionStart command")
	}
	if got, want := installed.Hooks.SessionStart[0].Hooks[0].Command, candidateGT+" prime --hook"; got != want {
		t.Fatalf("SessionStart command = %q, want exact candidate command %q", got, want)
	}
	if strings.Contains(string(hooksData), "cmd.test") {
		t.Fatal("temporary Codex hooks resolve to the Go test binary")
	}
	configData, err := os.ReadFile(filepath.Join(sandbox.RuntimeConfigDir, "config.toml"))
	if err != nil {
		t.Fatalf("temporary Codex config missing: %v", err)
	}
	var codexConfig struct {
		Notice struct {
			HideRateLimitModelNudge bool `toml:"hide_rate_limit_model_nudge"`
		} `toml:"notice"`
		Features struct {
			Hooks bool `toml:"hooks"`
		} `toml:"features"`
	}
	if _, err := toml.Decode(string(configData), &codexConfig); err != nil {
		t.Fatalf("parse temporary Codex config: %v", err)
	}
	if !codexConfig.Notice.HideRateLimitModelNudge {
		t.Fatal("temporary Codex config permits a model-switch reminder to block idle detection")
	}
	if !codexConfig.Features.Hooks {
		t.Fatal("temporary Codex config disables hooks")
	}
	if sandbox.Socket == "" || sandbox.Socket == "gastown" {
		t.Fatalf("canary socket is not isolated: %q", sandbox.Socket)
	}
	for _, path := range []string{sandbox.TownRoot, sandbox.WorkDir, sandbox.RuntimeConfigDir} {
		if filepath.Clean(path) == filepath.Clean(parent) || !filepath.IsAbs(path) {
			t.Fatalf("sandbox path is not a private child: %q", path)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("Stat(%s): %v", path, statErr)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("mode(%s) = %o, want 0700", path, info.Mode().Perm())
		}
	}

	if err := sandbox.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("sandbox still exists after cleanup: %v", err)
	}
}

func TestWakeCanaryLaunchBypassesHookTrustOnlyForCanary(t *testing.T) {
	sourceTownRoot := t.TempDir()
	sourceSettings := config.NewTownSettings()
	sourceSettings.Agents["night-mayor"] = &config.RuntimeConfig{
		Provider: "codex", Command: "configured-codex",
		Args: []string{"--model", "mayor"}, Env: map[string]string{"CANARY_MARKER": "enabled"},
		ExecWrapper: []string{"role-wrapper", "--"},
	}
	sourceSettings.Agents["night-polecat"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	sourceSettings.RoleAgents = map[string]string{
		constants.RoleMayor: "night-mayor", constants.RolePolecat: "night-polecat",
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(sourceTownRoot), sourceSettings); err != nil {
		t.Fatalf("save source town settings: %v", err)
	}
	sandbox := &wakeCanarySandbox{
		TownRoot:         t.TempDir(),
		WorkDir:          t.TempDir(),
		RuntimeConfigDir: t.TempDir(),
		Session:          session.MayorSessionName(),
	}
	roles, err := configureWakeCanarySandboxRoles(sandbox, sourceTownRoot, "")
	if err != nil {
		t.Fatalf("configure canary roles: %v", err)
	}
	if roles.MayorPreset != "night-mayor" || roles.PolecatPreset != "night-polecat" {
		t.Fatalf("configured roles = %+v", roles)
	}
	const instruction = "complete the finite startup challenge"
	cfg, err := wakeCanarySessionConfig(sandbox, instruction)
	if err != nil {
		t.Fatalf("build canary session config: %v", err)
	}
	if cfg.Command != "" {
		t.Fatalf("canary prebuilt command = %q, want standard startup builder", cfg.Command)
	}
	prompt := session.BuildStartupPrompt(cfg.Beacon, cfg.Instructions)
	command, err := config.BuildAgentStartupCommandWithAgentOverride(
		cfg.Role, cfg.RigName, cfg.TownRoot, cfg.RigPath, prompt, cfg.AgentOverride,
	)
	if err != nil {
		t.Fatalf("build standard canary command: %v", err)
	}
	parts := []string{"CANARY_MARKER=enabled", "role-wrapper", "configured-codex", "--model mayor", "--dangerously-bypass-hook-trust", instruction}
	last := -1
	for _, part := range parts {
		index := strings.Index(command[last+1:], part)
		if index < 0 {
			t.Fatalf("command ordering lacks %q after index %d: %q", part, last, command)
		}
		last += index + 1
	}
	ordinary, err := config.BuildAgentStartupCommandWithAgentOverride(
		constants.RoleMayor, "", sourceTownRoot, "", prompt, "night-mayor",
	)
	if err != nil {
		t.Fatalf("build ordinary configured command: %v", err)
	}
	if strings.Contains(ordinary, "--dangerously-bypass-hook-trust") {
		t.Fatalf("ordinary startup command contains canary flag: %q", ordinary)
	}
}

func TestWakeCanarySessionConfigUsesIsolatedConfiguredMayorPreset(t *testing.T) {
	callerDir := t.TempDir()
	callerSettings := config.NewRigSettings()
	callerSettings.Agents = map[string]*config.RuntimeConfig{
		"codex": {Provider: "codex", Command: "caller-codex-wrapper", Args: []string{"--caller"}},
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(callerDir), callerSettings); err != nil {
		t.Fatalf("save caller rig settings: %v", err)
	}
	t.Chdir(callerDir)

	townRoot := t.TempDir()
	workDir := filepath.Join(townRoot, "mayor", "rig")
	if err := os.MkdirAll(workDir, 0700); err != nil {
		t.Fatalf("create isolated workdir: %v", err)
	}
	townSettings := config.NewTownSettings()
	townSettings.Agents["night-mayor"] = &config.RuntimeConfig{
		Provider: "codex", Command: "configured-codex", Args: []string{"--model", "mayor"},
	}
	townSettings.RoleAgents = map[string]string{constants.RoleMayor: "night-mayor"}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatalf("save isolated town settings: %v", err)
	}
	sandbox := &wakeCanarySandbox{
		TownRoot: townRoot, WorkDir: workDir, RuntimeConfigDir: filepath.Join(townRoot, ".codex"),
		Session: session.MayorSessionName(),
	}
	cfg, err := wakeCanarySessionConfig(sandbox, "complete the finite startup challenge")
	if err != nil {
		t.Fatalf("build canary session config: %v", err)
	}

	runtimeConfig, _, err := config.ResolveAgentConfigWithOverride(cfg.TownRoot, cfg.RigPath, cfg.AgentOverride)
	if err != nil {
		t.Fatalf("resolve StartSession runtime: %v", err)
	}
	if cfg.AgentOverride != "night-mayor" || runtimeConfig.Provider != "codex" {
		t.Fatalf("StartSession runtime = %q/%q, want night-mayor/codex", cfg.AgentOverride, runtimeConfig.Provider)
	}
	if cfg.Command != "" {
		t.Fatalf("canary prebuilt command = %q, want standard configured startup", cfg.Command)
	}
	if cfg.RigPath != "" {
		t.Fatalf("canary rig path = %q, want town-level configured startup", cfg.RigPath)
	}
	command, err := config.BuildAgentStartupCommandWithAgentOverride(
		cfg.Role, cfg.RigName, cfg.TownRoot, cfg.RigPath,
		session.BuildStartupPrompt(cfg.Beacon, cfg.Instructions), cfg.AgentOverride,
	)
	if err != nil {
		t.Fatalf("build StartSession command: %v", err)
	}
	if strings.Contains(command, "caller-codex-wrapper") || !strings.Contains(command, "configured-codex") {
		t.Fatalf("canary command used wrong config: %q", command)
	}
}

func TestWriteWakeCanaryStateIsSanitizedAtomicAndPrivate(t *testing.T) {
	townRoot := t.TempDir()
	state := wakeCanaryState{
		SchemaVersion:         3,
		InstalledBinaryCommit: "abc123",
		MayorPreset:           "codex-mayor",
		MayorProvider:         "codex",
		PolecatPreset:         "codex-polecat",
		PolecatProvider:       "codex",
		Session:               session.MayorSessionName(),
		Runtime:               "codex",
		SessionAuthority:      strings.Repeat("a", 64),
		Turns:                 1,
		Submitted:             1,
		Receipts: []wakeCanaryReceipt{{
			Turn: 1, DeliveryID: "ndg-opaque", NonceDigest: strings.Repeat("b", 64),
			SubmittedAt: time.Now(), WindowStartedAt: time.Now().Add(-time.Second), WindowCompletedAt: time.Now().Add(time.Second),
		}},
		ReceiptCount:  1,
		ReceiptDigest: strings.Repeat("c", 64),
		AttemptedAt:   time.Now(),
		CompletedAt:   time.Now(),
		Result:        "passed",
		LatencyMS:     42,
	}
	path, err := writeWakeCanaryState(townRoot, state)
	if err != nil {
		t.Fatalf("writeWakeCanaryState: %v", err)
	}
	wantPath := filepath.Join(townRoot, ".runtime", "canary", "control-plane.json")
	if path != wantPath {
		t.Fatalf("state path = %q, want %q", path, wantPath)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["schema_version"] != float64(3) || got["result"] != "passed" || got["receipt_count"] != float64(1) ||
		got["mayor_preset"] != "codex-mayor" || got["polecat_preset"] != "codex-polecat" {
		t.Fatalf("state schema = %#v", got)
	}
	for _, forbidden := range []string{"nonce", "message"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("state leaked %s: %#v", forbidden, got)
		}
	}
}

func TestConfigureWakeCanarySandboxRolesUsesConfiguredPresetsAndProviders(t *testing.T) {
	townRoot := t.TempDir()
	settings := config.NewTownSettings()
	settings.Agents["codex-mayor"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.Agents["codex-polecat"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.RoleAgents = map[string]string{
		constants.RoleMayor:   "codex-mayor",
		constants.RolePolecat: "codex-polecat",
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), settings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}

	sandboxRoot := t.TempDir()
	got, err := configureWakeCanarySandboxRoles(&wakeCanarySandbox{TownRoot: sandboxRoot}, townRoot, "")
	if err != nil {
		t.Fatalf("configureWakeCanarySandboxRoles: %v", err)
	}
	if got.MayorPreset != "codex-mayor" || got.MayorProvider != "codex" {
		t.Fatalf("Mayor role evidence = %+v, want codex-mayor/codex", got)
	}
	if got.PolecatPreset != "codex-polecat" || got.PolecatProvider != "codex" {
		t.Fatalf("polecat role evidence = %+v, want codex-polecat/codex", got)
	}
	if delay := config.LoadOperationalConfig(sandboxRoot).GetMailConfig().ReplyReminderDelayD(); delay != 0 {
		t.Fatalf("sandbox reply reminder delay = %v, want disabled", delay)
	}
}

func TestConfigureWakeCanarySandboxRolesUsesCodexDefaultFallback(t *testing.T) {
	t.Setenv("GT_COST_TIER", "")
	sourceTownRoot := t.TempDir()
	settings := config.NewTownSettings()
	settings.DefaultAgent = "codex"
	settings.RoleAgents = map[string]string{
		constants.RoleMayor: "missing-mayor", constants.RolePolecat: "missing-polecat",
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(sourceTownRoot), settings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	sandbox := &wakeCanarySandbox{TownRoot: t.TempDir(), WorkDir: t.TempDir()}
	roles, err := configureWakeCanarySandboxRoles(sandbox, sourceTownRoot, "")
	if err != nil {
		t.Fatalf("configure default fallback: %v", err)
	}
	if roles.MayorPreset != "codex" || roles.MayorProvider != "codex" ||
		roles.PolecatPreset != "codex" || roles.PolecatProvider != "codex" {
		t.Fatalf("fallback roles = %+v, want base codex", roles)
	}
}

func TestConfigureWakeCanarySandboxRolesRejectsValidNonCodexPreset(t *testing.T) {
	t.Setenv("GT_COST_TIER", "")
	sourceTownRoot := t.TempDir()
	settings := config.NewTownSettings()
	settings.Agents["codex-mayor"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.Agents["claude-polecat"] = &config.RuntimeConfig{Provider: "claude", Command: "claude"}
	settings.RoleAgents = map[string]string{
		constants.RoleMayor: "codex-mayor", constants.RolePolecat: "claude-polecat",
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(sourceTownRoot), settings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	sandbox := &wakeCanarySandbox{TownRoot: t.TempDir(), WorkDir: t.TempDir()}
	if _, err := configureWakeCanarySandboxRoles(sandbox, sourceTownRoot, ""); err == nil || !strings.Contains(err.Error(), "Codex-backed") {
		t.Fatalf("configure non-Codex preset error = %v", err)
	}
}
