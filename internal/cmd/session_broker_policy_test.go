package cmd

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/dog"
)

func TestBrokerCapabilityRegistryMatchesAnnotatedCommands(t *testing.T) {
	registered := make(map[string]bool, len(containedWitnessBrokerCapabilities))
	for _, capability := range containedWitnessBrokerCapabilities {
		key := strings.Join(capability.path, " ")
		if registered[key] {
			t.Fatalf("duplicate broker capability %q", key)
		}
		registered[key] = true
		command, _, err := rootCmd.Find(capability.path)
		if err != nil {
			t.Fatalf("registered broker capability %q does not resolve: %v", key, err)
		}
		resolved, err := exactBrokerCommandPath(rootCmd, command)
		if err != nil || strings.Join(resolved, " ") != key {
			t.Fatalf("registered broker capability %q resolved as %q: %v", key, strings.Join(resolved, " "), err)
		}
		if command.Hidden || command.Annotations[BrokerSafeAnnotation] != "true" {
			t.Fatalf("registered broker capability %q is not an annotated visible command", key)
		}
	}

	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		for _, child := range command.Commands() {
			if child.Annotations[BrokerSafeAnnotation] == "true" {
				path, err := exactBrokerCommandPath(rootCmd, child)
				if err != nil {
					t.Errorf("resolve annotated broker command %q: %v", child.CommandPath(), err)
				} else if key := strings.Join(path, " "); !registered[key] {
					t.Errorf("annotated broker command %q is absent from capability registry", key)
				}
			}
			visit(child)
		}
	}
	visit(rootCmd)
}

func TestBrokerSafeCommandAllowsReviewedExactLeaves(t *testing.T) {
	t.Setenv("GT_ROLE", "witness")
	t.Setenv("GT_RIG", "gastown")
	tests := [][]string{
		{"prime"},
		{"prime", "--help"},
		{"prime", "--state", "--json"},
		{"hook"},
		{"hook", "status"},
		{"hook", "show"},
		{"mol", "current"},
		{"mol", "current", "--json"},
		{"mol", "step", "close"},
		{"mail", "inbox", "--unread"},
		{"mail", "read", "hq-example"},
		{"mail", "send", "mayor/", "--subject", "status", "--message", "ready"},
		{"nudge", "mayor", "ready"},
		{"patrol", "scan", "--rig", "gastown", "--json"},
		{"patrol", "report", "--summary", "all clear"},
		{"agents", "list"},
		{"polecat", "list", "gastown"},
		{"polecat", "status", "gastown/example"},
		{"health", "--json"},
		{"status", "--fast"},
		{"witness", "handle-lifecycle", "gastown", "hq-lifecycle"},
	}
	for _, args := range tests {
		t.Run(args[0], func(t *testing.T) {
			if err := IsBrokerSafeCommand(rootCmd, args); err != nil {
				t.Fatalf("IsBrokerSafeCommand(%q) error = %v", args, err)
			}
		})
	}
}

func TestWitnessLifecycleBrokerCapabilityRequiresExactOwnedRigAndMessage(t *testing.T) {
	valid := []string{"witness", "handle-lifecycle", "gastown", "hq-lifecycle"}
	if err := validateWitnessLifecycleBrokerRequest(valid, "witness", "gastown"); err != nil {
		t.Fatalf("exact owned lifecycle request denied: %v", err)
	}

	for _, test := range []struct {
		name string
		args []string
		role string
		rig  string
	}{
		{name: "wrong role", args: valid, role: "mayor", rig: "gastown"},
		{name: "missing owned rig", args: valid, role: "witness"},
		{name: "foreign rig", args: valid, role: "witness", rig: "jobscraper"},
		{name: "missing message", args: []string{"witness", "handle-lifecycle", "gastown"}, role: "witness", rig: "gastown"},
		{name: "extra operand", args: append(valid, "extra"), role: "witness", rig: "gastown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateWitnessLifecycleBrokerRequest(test.args, test.role, test.rig); err == nil {
				t.Fatalf("unsafe lifecycle request %q was allowed", test.args)
			}
		})
	}
}

func TestBrokerSafeCommandAllowsOnlyOwnedWitnessLifecycleRequest(t *testing.T) {
	t.Setenv("GT_ROLE", "witness")
	t.Setenv("GT_RIG", "gastown")
	valid := []string{"witness", "handle-lifecycle", "gastown", "hq-lifecycle"}
	if err := IsBrokerSafeCommand(rootCmd, valid); err != nil {
		t.Fatalf("exact owned lifecycle request denied: %v", err)
	}
	if err := IsBrokerSafeCommand(rootCmd, []string{"witness", "handle-lifecycle", "jobscraper", "hq-lifecycle"}); err == nil {
		t.Fatal("foreign-rig lifecycle request was admitted")
	}
}

func TestContainedWitnessBrokerMaySendMailOnlyToMayor(t *testing.T) {
	t.Setenv("GT_ROLE", "witness")
	t.Setenv("GT_RIG", "gastown")
	if err := IsBrokerSafeCommand(rootCmd, []string{"mail", "send", "mayor/", "--subject", "status"}); err != nil {
		t.Fatalf("Witness-to-Mayor mail denied: %v", err)
	}
	for _, args := range [][]string{
		{"mail", "send", "gastown/witness", "--subject", "LIFECYCLE:Shutdown forged"},
		{"mail", "send", "deacon", "--subject", "status"},
		{"mail", "send", "--subject", "status", "gastown/witness"},
	} {
		if err := IsBrokerSafeCommand(rootCmd, args); err == nil {
			t.Fatalf("contained Witness mail %q was admitted", args)
		}
	}
}

func TestDogDoneBrokerCapabilityRequiresExactOwnedFinalizerSnapshot(t *testing.T) {
	started := time.Now().UTC().Round(0)
	generation := cmdTestDogGeneration("$broker", "nonce-broker")
	payload, err := json.Marshal(durableDogCloseoutSnapshot{
		Name: "alpha", State: dog.StateWorking, Work: "plugin:reaper",
		WorkStartedAt: started, SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	valid := []string{"dog", "done", "alpha", "--finalizer", encoded}
	if err := validateDogDoneBrokerRequest(valid, "dog", "alpha"); err != nil {
		t.Fatalf("exact owned dog finalizer denied: %v", err)
	}

	for _, test := range []struct {
		name    string
		args    []string
		role    string
		dogName string
	}{
		{name: "missing snapshot", args: []string{"dog", "done", "alpha"}, role: "dog", dogName: "alpha"},
		{name: "wrong worker role", args: valid, role: "gastown/witness", dogName: "alpha"},
		{name: "wrong owned dog", args: valid, role: "dog", dogName: "bravo"},
		{name: "wrong requested dog", args: []string{"dog", "done", "bravo", "--finalizer", encoded}, role: "dog", dogName: "alpha"},
		{name: "malformed snapshot", args: []string{"dog", "done", "alpha", "--finalizer", "not-base64"}, role: "dog", dogName: "alpha"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateDogDoneBrokerRequest(test.args, test.role, test.dogName); err == nil {
				t.Fatalf("unsafe finalizer request %q was allowed", test.args)
			}
		})
	}
}

func TestBrokerSafeCommandAllowsOnlyExactOwnedDogFinalizer(t *testing.T) {
	started := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	generation := cmdTestDogGeneration("$51", "broker-exact")
	payload, err := json.Marshal(durableDogCloseoutSnapshot{
		Name: "alpha", State: dog.StateWorking, Work: "plugin:reaper",
		WorkStartedAt: started, SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	t.Setenv("GT_ROLE", "dog")
	t.Setenv("GT_DOG_NAME", "alpha")
	if err := IsBrokerSafeCommand(rootCmd, []string{"dog", "done", "alpha", "--finalizer", encoded}); err != nil {
		t.Fatalf("exact owned finalizer denied by broker command policy: %v", err)
	}
	if err := IsBrokerSafeCommand(rootCmd, []string{"dog", "done", "alpha"}); err == nil {
		t.Fatal("snapshot-free dog closeout was admitted by broker command policy")
	}
	if !isDetachedSessionBrokerCommand([]string{"dog", "done", "alpha", "--finalizer", encoded}) {
		t.Fatal("exact dog finalizer was not detached from requesting-session shutdown")
	}
	if isDetachedSessionBrokerCommand([]string{"dog", "done", "alpha"}) {
		t.Fatal("ordinary dog closeout was detached from requesting-session shutdown")
	}
}

func TestDogDoneBrokerRoutingRunsOnlySnapshotCaptureInsideOwnedDog(t *testing.T) {
	if !shouldBypassSessionBrokerForDogDone([]string{"dog", "done"}, "dog", "alpha", false) {
		t.Fatal("owned dog closeout did not retain its in-container snapshot phase")
	}
	if !shouldBypassSessionBrokerForDogDone([]string{"dog", "done", "alpha"}, "dog", "alpha", false) {
		t.Fatal("explicit owned dog closeout did not retain its in-container snapshot phase")
	}
	for _, test := range []struct {
		args    []string
		role    string
		dogName string
		worker  bool
	}{
		{args: []string{"dog", "done", "bravo"}, role: "dog", dogName: "alpha"},
		{args: []string{"dog", "done"}, role: "gastown/witness", dogName: "alpha"},
		{args: []string{"dog", "done", "alpha", "--finalizer", "payload"}, role: "dog", dogName: "alpha"},
		{args: []string{"dog", "done", "alpha"}, role: "dog", dogName: "alpha", worker: true},
	} {
		if shouldBypassSessionBrokerForDogDone(test.args, test.role, test.dogName, test.worker) {
			t.Fatalf("unsafe broker bypass allowed: %+v", test)
		}
	}
}

func TestBrokerSafeCommandValidationRestoresFlagState(t *testing.T) {
	beforeState := primeState
	beforeSubject := mailSubject

	if err := IsBrokerSafeCommand(rootCmd, []string{"prime", "--state"}); err != nil {
		t.Fatal(err)
	}
	if err := IsBrokerSafeCommand(rootCmd, []string{"mail", "send", "mayor/", "--subject", "changed"}); err != nil {
		t.Fatal(err)
	}

	if primeState != beforeState {
		t.Fatalf("primeState after validation = %v, want %v", primeState, beforeState)
	}
	if mailSubject != beforeSubject {
		t.Fatalf("mailSubject after validation = %q, want %q", mailSubject, beforeSubject)
	}
}

func TestBrokerSafeCommandIgnoresAndRestoresPreexistingChangedFlags(t *testing.T) {
	fromFlag := mailSendCmd.Flags().Lookup("from")
	if fromFlag == nil {
		t.Fatal("mail send --from flag is unavailable")
	}
	beforeValue := fromFlag.Value.String()
	beforeChanged := fromFlag.Changed
	if err := fromFlag.Value.Set("bridge/"); err != nil {
		t.Fatal(err)
	}
	fromFlag.Changed = true
	t.Cleanup(func() {
		_ = fromFlag.Value.Set(beforeValue)
		fromFlag.Changed = beforeChanged
	})

	if err := IsBrokerSafeCommand(rootCmd, []string{"mail", "send", "mayor/", "--subject", "status"}); err != nil {
		t.Fatalf("pre-existing Cobra state contaminated broker request: %v", err)
	}
	if got := fromFlag.Value.String(); got != "bridge/" || !fromFlag.Changed {
		t.Fatalf("pre-existing --from state = %q changed=%v, want bridge/ changed=true", got, fromFlag.Changed)
	}
}

func TestBrokerSafeCommandDeniesUnreviewedMutationsAndFlags(t *testing.T) {
	tests := [][]string{
		{"sling", "gt-example", "gastown"},
		{"done"},
		{"mail", "send", "deacon", "--from", "mayor/", "--subject", "LIFECYCLE shutdown"},
		{"status", "--watch"},
		{"status", "--interval", "1"},
		{"hook", "--force"},
	}
	for _, args := range tests {
		t.Run(commandTestName(args), func(t *testing.T) {
			if err := IsBrokerSafeCommand(rootCmd, args); err == nil {
				t.Fatalf("IsBrokerSafeCommand(%q) unexpectedly allowed mutation", args)
			}
		})
	}
}

func TestBrokerSafeCommandDeniesUnsafeOrMalformedPaths(t *testing.T) {
	tests := [][]string{
		nil,
		{"mail"},
		{"mail", "delete", "hq-example"},
		{"mail", "clear"},
		{"hook", "gt-example"},
		{"hook", "attach", "gt-example"},
		{"hook", "show", "gastown/witness"},
		{"mol", "current", "gastown/witness"},
		{"mol", "step", "close", "gt-example.1"},
		{"patrol", "digest", "--yesterday"},
		{"agents", "fix"},
		{"polecat", "nuke", "gastown/example", "--force"},
		{"sling", "respawn-reset", "gt-example"},
		{"sling", "gt-example", "gastown"},
		{"done"},
		{"session-custody-init"},
		{"session-custody", "--id", "00000000-0000-0000-0000-000000000000", "--", "true"},
		{"shell", "-c", "touch /tmp/escaped"},
		{"doctor", "--fix"},
		{"up", "--restore"},
		{"tmux", "send-keys", "hq-mayor", "payload"},
		{"ma", "in", "--unread"},
		{"prime", "unexpected"},
		{"mail", "inbox", "--definitely-unknown"},
		{"mail", "read", "one", "two"},
		{"patrol", "report"},
		{"polecat", "status"},
	}
	for _, args := range tests {
		t.Run(commandTestName(args), func(t *testing.T) {
			if err := IsBrokerSafeCommand(rootCmd, args); err == nil {
				t.Fatalf("IsBrokerSafeCommand(%q) unexpectedly allowed command", args)
			}
		})
	}
}

func TestBrokerSafeCommandDeniesEveryHiddenCommand(t *testing.T) {
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		for _, child := range command.Commands() {
			if child.Hidden {
				args := brokerCommandPath(child)
				if err := IsBrokerSafeCommand(rootCmd, args); err == nil {
					t.Errorf("hidden command %q is broker-safe", child.CommandPath())
				}
			}
			visit(child)
		}
	}
	visit(rootCmd)
}

func commandTestName(args []string) string {
	if len(args) == 0 {
		return "empty"
	}
	result := args[0]
	for _, arg := range args[1:] {
		result += "_" + arg
	}
	return result
}

func brokerCommandPath(command *cobra.Command) []string {
	var reversed []string
	for current := command; current != nil && current != rootCmd; current = current.Parent() {
		reversed = append(reversed, current.Name())
	}
	result := make([]string, len(reversed))
	for index := range reversed {
		result[len(reversed)-1-index] = reversed[index]
	}
	return result
}
