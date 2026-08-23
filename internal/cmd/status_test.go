package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	os.Stderr = w

	fn()

	_ = w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	_ = r.Close()

	return buf.String()
}

func TestDiscoverRigAgents_UsesRigPrefix(t *testing.T) {
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "bd-", Path: "beads/mayor/rig"},
	})

	r := &rig.Rig{
		Name:       "beads",
		Path:       filepath.Join(townRoot, "beads"),
		HasWitness: true,
	}

	allAgentBeads := map[string]*beads.Issue{
		"bd-beads-witness": {
			ID:         "bd-beads-witness",
			AgentState: "running",
			HookBead:   "bd-hook",
		},
	}
	allHookBeads := map[string]*beads.Issue{
		"bd-hook": {ID: "bd-hook", Title: "Pinned"},
	}

	agents := discoverRigAgents(map[string]bool{}, r, nil, allAgentBeads, allHookBeads, nil, true)
	if len(agents) != 1 {
		t.Fatalf("discoverRigAgents() returned %d agents, want 1", len(agents))
	}

	if agents[0].State != "running" {
		t.Fatalf("agent state = %q, want %q", agents[0].State, "running")
	}
	if !agents[0].HasWork {
		t.Fatalf("agent HasWork = false, want true")
	}
	if agents[0].WorkTitle != "Pinned" {
		t.Fatalf("agent WorkTitle = %q, want %q", agents[0].WorkTitle, "Pinned")
	}
}

func TestAgentRuntimeMailWorkIsAdditiveToPrimaryWork(t *testing.T) {
	agent := AgentRuntime{Address: "gastown/Toast", HasWork: false}
	messages := statusMailWorkMessages(t)
	populateMailWorkInfo(&agent, messages, func(generation mail.WorkGeneration) bool {
		return generation.Name == "gt-gastown-Toast"
	})

	if agent.HasWork {
		t.Fatal("secondary mail changed has_work")
	}
	if !agent.HasMailWork || !agent.HasAnyWork {
		t.Fatalf("mail work flags = %v/%v", agent.HasMailWork, agent.HasAnyWork)
	}
	if agent.PendingMailWork != 1 || len(agent.MailWork) != 2 {
		t.Fatalf("pending/owned = %d/%d", agent.PendingMailWork, len(agent.MailWork))
	}
	if agent.MailWork[0].Status != mail.WorkStateInProgress || agent.MailWork[1].Status != mail.WorkStateBlocked {
		t.Fatalf("mail work = %#v", agent.MailWork)
	}

	payload, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(payload)
	for _, want := range []string{
		`"has_work":false`, `"has_mail_work":true`, `"has_any_work":true`,
		`"pending_mail_work":1`, `"mail_work":[`,
	} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("status JSON missing %s: %s", want, jsonText)
		}
	}

	var output bytes.Buffer
	renderAgentDetails(&output, agent, "", nil, t.TempDir())
	for _, want := range []string{"hook: (none)", "mail work: in_progress", "mail work: blocked", "pending task mail: 1"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("human status missing %q: %s", want, output.String())
		}
	}
	output.Reset()
	renderAgentCompact(&output, agent, "", nil, "")
	for _, want := range []string{"active:1", "blocked:1", "pending:1"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("compact status missing %q: %s", want, output.String())
		}
	}
}

func statusMailWorkMessages(t *testing.T) []*mail.Message {
	t.Helper()
	now := time.Date(2026, time.August, 24, 8, 0, 0, 0, time.UTC)
	generation := mail.WorkGenerationFromTmux(tmux.SessionGeneration{
		Name: "gt-gastown-Toast", SessionID: "$8", PaneID: "%12", Nonce: "generation-nonce",
		Custody: "custody-marker", ServerPID: 4242, ServerIdentity: "server-start-identity",
		Transport: tmux.SessionTransport{Bound: true, SocketName: "gastown", SocketPath: "/tmp/tmux-test/gastown"},
	})
	makeMessage := func(id string, state mail.WorkState, work *mail.WorkMetadata) *mail.Message {
		metadata, err := mail.EncodeMailWorkMetadata(nil, work)
		if err != nil {
			t.Fatal(err)
		}
		message := &mail.Message{
			ID: id, From: "mayor/", To: "gastown/Toast", Subject: id, Type: mail.TypeTask,
			Status: state, Labels: []string{"gt:message", mail.MailWorkLabel}, Metadata: metadata,
		}
		if !message.IsActionableWork() {
			t.Fatalf("%s is not actionable", id)
		}
		parsed, err := mail.ParseMailWorkMetadata(message.Metadata)
		if err != nil || parsed.Validate(state) != nil {
			t.Fatalf("%s metadata invalid: parse=%v validate=%v", id, err, parsed.Validate(state))
		}
		return message
	}
	claim := &mail.WorkClaim{Actor: "gastown/Toast", ClaimedAt: now, Generation: generation}
	return []*mail.Message{
		makeMessage("pending", mail.WorkStateOpen, &mail.WorkMetadata{Schema: mail.MailWorkSchema, Route: mail.WorkRouteDirect}),
		makeMessage("active", mail.WorkStateInProgress, &mail.WorkMetadata{Schema: mail.MailWorkSchema, Route: mail.WorkRouteDirect, Claim: claim}),
		makeMessage("blocked", mail.WorkStateBlocked, &mail.WorkMetadata{Schema: mail.MailWorkSchema, Route: mail.WorkRouteDirect, Claim: claim, Blocked: &mail.WorkBlock{Reason: "dependency", At: now.Add(time.Minute)}}),
	}
}

func TestRenderAgentDetails_UsesRigPrefix(t *testing.T) {
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "bd-", Path: "beads/mayor/rig"},
	})

	agent := AgentRuntime{
		Name:    "witness",
		Address: "beads/witness",
		Role:    "witness",
		Running: true,
	}

	var buf bytes.Buffer
	renderAgentDetails(&buf, agent, "", nil, townRoot)
	output := buf.String()

	if !strings.Contains(output, "bd-beads-witness") {
		t.Fatalf("output %q does not contain rig-prefixed bead ID", output)
	}
}

func TestDiscoverRigAgents_ZombieSessionNotRunning(t *testing.T) {
	// Verify that a session in allSessions with value=false (zombie: tmux alive,
	// agent dead) results in agent.Running=false. This is the core fix for gt-bd6i3.
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
	})

	r := &rig.Rig{
		Name:       "gastown",
		Path:       filepath.Join(townRoot, "gastown"),
		HasWitness: true,
	}

	// allSessions has the witness session but marked as zombie (false).
	// This simulates a tmux session that exists but whose agent process has died.
	allSessions := map[string]bool{
		"gt-gastown-witness": false, // zombie: tmux exists, agent dead
	}

	agents := discoverRigAgents(allSessions, r, nil, nil, nil, nil, true)
	for _, a := range agents {
		if a.Role == "witness" {
			if a.Running {
				t.Fatal("zombie witness session (allSessions=false) should show as not running")
			}
			return
		}
	}
	t.Fatal("witness agent not found in results")
}

func TestDiscoverRigAgents_MissingSessionNotRunning(t *testing.T) {
	// Verify that a session not in allSessions at all results in agent.Running=false.
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
	})

	r := &rig.Rig{
		Name:       "gastown",
		Path:       filepath.Join(townRoot, "gastown"),
		HasWitness: true,
	}

	// Empty sessions map - no tmux sessions exist at all
	allSessions := map[string]bool{}

	agents := discoverRigAgents(allSessions, r, nil, nil, nil, nil, true)
	for _, a := range agents {
		if a.Role == "witness" {
			if a.Running {
				t.Fatal("witness with no tmux session should show as not running")
			}
			return
		}
	}
	t.Fatal("witness agent not found in results")
}

func TestBuildStatusIndicator_ZombieShowsStopped(t *testing.T) {
	// Verify that a zombie agent (Running=false) shows ○ (stopped), not ● (running)
	agent := AgentRuntime{Running: false}
	indicator := buildStatusIndicator(agent)
	if strings.Contains(indicator, "●") {
		t.Fatal("zombie agent (Running=false) should not show ● indicator")
	}
}

func TestBuildStatusIndicator_AliveShowsRunning(t *testing.T) {
	// Verify that an alive agent (Running=true) shows ● (running)
	agent := AgentRuntime{Running: true}
	indicator := buildStatusIndicator(agent)
	if strings.Contains(indicator, "○") {
		t.Fatal("alive agent (Running=true) should not show ○ indicator")
	}
}

func TestBuildStatusIndicator_DNDMutedShowsBadge(t *testing.T) {
	agent := AgentRuntime{Running: true, NotificationLevel: beads.NotifyMuted}
	indicator := buildStatusIndicator(agent)
	if !strings.Contains(indicator, "🔕") {
		t.Fatalf("expected muted indicator to include 🔕, got %q", indicator)
	}
}

func TestOutputStatusText_IncludesDNDSection(t *testing.T) {
	status := TownStatus{
		Name:     "gt",
		Location: "/tmp/gt",
		DND: &DNDInfo{
			Enabled: true,
			Level:   beads.NotifyMuted,
			Agent:   "hq-mayor",
		},
	}

	var buf bytes.Buffer
	if err := outputStatusText(&buf, status); err != nil {
		t.Fatalf("outputStatusText error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "DND:") {
		t.Fatalf("expected DND section in status output, got: %q", out)
	}
	if !strings.Contains(out, "on") {
		t.Fatalf("expected DND state 'on' in status output, got: %q", out)
	}
}

func TestRunStatusWatch_RejectsZeroInterval(t *testing.T) {
	oldInterval := statusInterval
	oldWatch := statusWatch
	defer func() {
		statusInterval = oldInterval
		statusWatch = oldWatch
	}()

	statusInterval = 0
	statusWatch = true

	err := runStatusWatch(nil, nil)
	if err == nil {
		t.Fatal("expected error for zero interval, got nil")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error %q should mention 'positive'", err.Error())
	}
}

func TestRunStatusWatch_RejectsNegativeInterval(t *testing.T) {
	oldInterval := statusInterval
	oldWatch := statusWatch
	defer func() {
		statusInterval = oldInterval
		statusWatch = oldWatch
	}()

	statusInterval = -5
	statusWatch = true

	err := runStatusWatch(nil, nil)
	if err == nil {
		t.Fatal("expected error for negative interval, got nil")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error %q should mention 'positive'", err.Error())
	}
}

func TestRunStatusWatch_RejectsJSONCombo(t *testing.T) {
	oldJSON := statusJSON
	oldWatch := statusWatch
	oldInterval := statusInterval
	defer func() {
		statusJSON = oldJSON
		statusWatch = oldWatch
		statusInterval = oldInterval
	}()

	statusJSON = true
	statusWatch = true
	statusInterval = 2

	err := runStatusWatch(nil, nil)
	if err == nil {
		t.Fatal("expected error for --json + --watch, got nil")
	}
	if !strings.Contains(err.Error(), "cannot be used together") {
		t.Errorf("error %q should mention 'cannot be used together'", err.Error())
	}
}

func TestTryStatusDetailLockContention(t *testing.T) {
	townRoot := t.TempDir()

	release, ok := tryStatusDetailLock(townRoot)
	if !ok {
		t.Fatal("first status detail lock should be acquired")
	}

	if release2, ok := tryStatusDetailLock(townRoot); ok {
		release2()
		t.Fatal("second status detail lock should fail while first is held")
	}

	release()

	release3, ok := tryStatusDetailLock(townRoot)
	if !ok {
		t.Fatal("status detail lock should be reusable after release")
	}
	release3()
}

func TestIsKnownAgent(t *testing.T) {
	t.Parallel()

	// All agent presets should be recognized
	for _, name := range config.ListAgentPresets() {
		t.Run(name+"_known", func(t *testing.T) {
			if !isKnownAgent(name) {
				t.Errorf("isKnownAgent(%q) = false, want true", name)
			}
		})
	}

	// Non-agents should not be recognized
	for _, name := range []string{"bash", "node", ""} {
		t.Run(name+"_unknown", func(t *testing.T) {
			if isKnownAgent(name) {
				t.Errorf("isKnownAgent(%q) = true, want false", name)
			}
		})
	}
}

func TestIsAgentWrapper(t *testing.T) {
	t.Parallel()
	tests := []struct {
		base string
		want bool
	}{
		{"node", true},
		{"bun", true},
		{"npx", true},
		{"bunx", true},
		{"claude", false},
		{"pi", false},
		{"bash", false},
	}

	for _, tt := range tests {
		t.Run(tt.base, func(t *testing.T) {
			if got := isAgentWrapper(tt.base); got != tt.want {
				t.Errorf("isAgentWrapper(%q) = %v, want %v", tt.base, got, tt.want)
			}
		})
	}
}

func TestParseRuntimeInfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cmdline string
		want    string
	}{
		{
			name:    "claude with model",
			cmdline: "claude\x00--model\x00opus\x00--dangerously-skip-permissions",
			want:    "claude/opus",
		},
		{
			name:    "pi with model",
			cmdline: "pi\x00-e\x00gastown-hooks.js\x00--model\x00google-antigravity/gemini-3-flash",
			want:    "pi/google-antigravity/gemini-3-flash",
		},
		{
			name:    "cgroup-wrap then claude",
			cmdline: "cgroup-wrap\x00claude\x00--model\x00opus\x00--dangerously-skip-permissions",
			want:    "claude/opus",
		},
		{
			name:    "opencode with -m flag",
			cmdline: "opencode\x00-m\x00kimi-for-coding/kimi-k2.5",
			want:    "opencode/kimi-for-coding/kimi-k2.5",
		},
		{
			name:    "empty cmdline",
			cmdline: "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRuntimeInfo(tt.cmdline)
			if got != tt.want {
				t.Errorf("parseRuntimeInfo(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestParseRuntimeInfo_PiBare(t *testing.T) {
	t.Parallel()
	// Bare pi (no --model flag) calls readPiDefaults() which reads
	// ~/.pi/agent/settings.json. The result is either "pi" (if no settings)
	// or "pi/<default-model>" (if settings exist). Both are valid.
	cmdline := "pi\x00-e\x00gastown-hooks.js"
	got := parseRuntimeInfo(cmdline)
	if !strings.HasPrefix(got, "pi") {
		t.Errorf("parseRuntimeInfo(pi bare) = %q, want prefix 'pi'", got)
	}
}

func TestBuildInfoFromConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rc   *config.RuntimeConfig
		want string
	}{
		{
			name: "claude with model",
			rc:   &config.RuntimeConfig{Command: "claude", Args: []string{"--model", "opus"}},
			want: "claude/opus",
		},
		{
			name: "cgroup-wrap claude",
			rc:   &config.RuntimeConfig{Command: "cgroup-wrap", Args: []string{"claude", "--model", "opus"}},
			want: "claude/opus",
		},
		{
			name: "pi bare",
			rc:   &config.RuntimeConfig{Command: "pi", Args: []string{"-e", "hooks.js"}},
			want: "pi",
		},
		{
			name: "opencode with -m",
			rc:   &config.RuntimeConfig{Command: "opencode", Args: []string{"-m", "gpt-5"}},
			want: "opencode/gpt-5",
		},
		{
			name: "empty command",
			rc:   &config.RuntimeConfig{Command: ""},
			want: "claude",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildInfoFromConfig(tt.rc)
			if got != tt.want {
				t.Errorf("buildInfoFromConfig(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestIsAgentCmdline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{"claude direct", "claude\x00--model\x00opus", true},
		{"pi direct", "pi\x00-e\x00hooks.js", true},
		{"node wrapper with pi", "node\x00/path/to/pi\x00-e\x00hooks.js", true},
		{"bun wrapper with opencode", "bun\x00/path/to/opencode", true},
		{"bash not agent", "bash\x00-c\x00echo hi", false},
		{"node without agent", "node\x00/path/to/server.js", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAgentCmdline(tt.cmdline)
			if got != tt.want {
				t.Errorf("isAgentCmdline(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestCountRunningAgents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status TownStatus
		want   int
	}{
		{
			name:   "empty status",
			status: TownStatus{},
			want:   0,
		},
		{
			name: "global agents only",
			status: TownStatus{
				Agents: []AgentRuntime{
					{Name: "mayor", Running: true},
					{Name: "deacon", Running: false},
				},
			},
			want: 1,
		},
		{
			name: "rig agents only",
			status: TownStatus{
				Rigs: []RigStatus{
					{
						Agents: []AgentRuntime{
							{Name: "polecat-1", Running: true},
							{Name: "witness", Running: true},
						},
					},
				},
			},
			want: 2,
		},
		{
			name: "mixed global and rig agents",
			status: TownStatus{
				Agents: []AgentRuntime{
					{Name: "mayor", Running: true},
				},
				Rigs: []RigStatus{
					{
						Agents: []AgentRuntime{
							{Name: "polecat-1", Running: true},
							{Name: "witness", Running: false},
						},
					},
					{
						Agents: []AgentRuntime{
							{Name: "polecat-2", Running: true},
						},
					},
				},
			},
			want: 3,
		},
		{
			name: "all not running",
			status: TownStatus{
				Agents: []AgentRuntime{
					{Name: "mayor", Running: false},
				},
				Rigs: []RigStatus{
					{
						Agents: []AgentRuntime{
							{Name: "polecat-1", Running: false},
						},
					},
				},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countRunningAgents(tt.status)
			if got != tt.want {
				t.Errorf(
					"countRunningAgents() = %d, want %d",
					got, tt.want,
				)
			}
		})
	}
}

func TestExtractBaseName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cmdline string
		want    string
	}{
		{"claude\x00--model\x00opus", "claude"},
		{"/usr/bin/node\x00/path/pi", "node"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := extractBaseName(tt.cmdline)
			if got != tt.want {
				t.Errorf("extractBaseName(%q) = %q, want %q", tt.cmdline, got, tt.want)
			}
		})
	}
}
