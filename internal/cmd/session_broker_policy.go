package cmd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	BrokerSafeAnnotation      = "gastown.io/session-broker-safe"
	brokerSafeArgsAnnotation  = "gastown.io/session-broker-args"
	brokerSafeFlagsAnnotation = "gastown.io/session-broker-flags"
	brokerSafeArgsCobra       = "cobra"
	brokerSafeArgsNone        = "none"
	brokerSafeArgsExactOne    = "exact:1"
	brokerSafeArgsMaximumOne  = "maximum:1"
)

var brokerPolicyMu sync.Mutex

const containedWitnessRigToken = "{rig}"

type containedWitnessPrimeCommand struct {
	description string
	args        []string
}

type containedWitnessBrokerCapability struct {
	path     []string
	guidance []containedWitnessPrimeCommand
}

// containedWitnessBrokerCapabilities is the single reviewed registry for both
// broker authorization and contained Witness guidance. A capability without a
// guidance entry remains usable by a caller that already knows its operands,
// but is not presented as a generally actionable patrol command.
var containedWitnessBrokerCapabilities = []containedWitnessBrokerCapability{
	{path: []string{"prime"}},
	{path: []string{"hook"}, guidance: []containedWitnessPrimeCommand{
		{description: "Inspect the current hook", args: []string{"hook"}},
	}},
	{path: []string{"hook", "status"}},
	{path: []string{"hook", "show"}, guidance: []containedWitnessPrimeCommand{
		{description: "Show the current hooked work", args: []string{"hook", "show"}},
	}},
	{path: []string{"mol", "current"}, guidance: []containedWitnessPrimeCommand{
		{description: "Inspect the current patrol step", args: []string{"mol", "current"}},
	}},
	{path: []string{"mol", "step", "close"}, guidance: []containedWitnessPrimeCommand{
		{description: "Close the current patrol step", args: []string{"mol", "step", "close"}},
	}},
	{path: []string{"mail", "inbox"}, guidance: []containedWitnessPrimeCommand{
		{description: "Check unread mail", args: []string{"mail", "inbox", "--unread"}},
	}},
	{path: []string{"mail", "read"}, guidance: []containedWitnessPrimeCommand{
		{description: "Read one stored message", args: []string{"mail", "read", "<message-id>"}},
	}},
	{path: []string{"mail", "send"}, guidance: []containedWitnessPrimeCommand{
		{description: "Send durable mail to the Mayor", args: []string{"mail", "send", "mayor/", "--subject", "<subject>", "--message", "<body>", "--no-notify"}},
	}},
	{path: []string{"nudge"}, guidance: []containedWitnessPrimeCommand{
		{description: "Queue a routine Mayor nudge", args: []string{"nudge", "mayor", "<message>", "--mode", "queue"}},
		{description: "Send an immediate Mayor nudge", args: []string{"nudge", "mayor", "<message>", "--mode", "immediate"}},
	}},
	{path: []string{"patrol", "scan"}, guidance: []containedWitnessPrimeCommand{
		{description: "Scan this rig", args: []string{"patrol", "scan", "--rig", containedWitnessRigToken, "--json"}},
	}},
	{path: []string{"patrol", "report"}, guidance: []containedWitnessPrimeCommand{
		{description: "Report this patrol cycle", args: []string{"patrol", "report", "--summary", "<brief summary of observations>"}},
	}},
	{path: []string{"agents", "list"}, guidance: []containedWitnessPrimeCommand{
		{description: "List live agents", args: []string{"agents", "list"}},
	}},
	{path: []string{"polecat", "list"}, guidance: []containedWitnessPrimeCommand{
		{description: "List this rig's polecats", args: []string{"polecat", "list", containedWitnessRigToken}},
	}},
	{path: []string{"polecat", "status"}},
	{path: []string{"health"}},
	{path: []string{"status"}, guidance: []containedWitnessPrimeCommand{
		{description: "Read town status", args: []string{"status", "--fast"}},
	}},
	// Lifecycle handling is intentionally absent from user guidance. The exact
	// message ID is dispatched by the Witness mail loop and must match this rig.
	{path: []string{"witness", "handle-lifecycle"}},
	// Dog closeout is intentionally absent from user guidance. The only
	// accepted form carries an exact snapshot captured inside the owned dog
	// session, then runs through the trusted host worker.
	{path: []string{"dog", "done"}},
}

func containedWitnessBrokerGuidance(rigName string) []containedWitnessPrimeCommand {
	var result []containedWitnessPrimeCommand
	for _, capability := range containedWitnessBrokerCapabilities {
		for _, command := range capability.guidance {
			copy := containedWitnessPrimeCommand{description: command.description, args: append([]string(nil), command.args...)}
			for index, arg := range copy.args {
				if arg == containedWitnessRigToken {
					copy.args[index] = rigName
				}
			}
			result = append(result, copy)
		}
	}
	return result
}

func isContainedWitnessBrokerCapability(path []string) bool {
	for _, capability := range containedWitnessBrokerCapabilities {
		if len(path) != len(capability.path) {
			continue
		}
		match := true
		for index := range path {
			if path[index] != capability.path[index] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// IsBrokerSafeCommand resolves and validates one exact reviewed Cobra leaf.
// Prefixes, aliases, hidden commands, unannotated commands, malformed flags,
// and invalid positional arguments are denied before the broker starts a
// process outside the containment namespace.
func IsBrokerSafeCommand(root *cobra.Command, args []string) error {
	brokerPolicyMu.Lock()
	defer brokerPolicyMu.Unlock()

	if root == nil || len(args) == 0 {
		return errors.New("broker command is empty")
	}
	command, _, err := root.Find(args)
	if err != nil {
		return fmt.Errorf("resolving broker command: %w", err)
	}
	if command == root || command.Hidden || command.Annotations[BrokerSafeAnnotation] != "true" {
		return fmt.Errorf("command %q is not broker-safe", command.CommandPath())
	}

	path, err := exactBrokerCommandPath(root, command)
	if err != nil {
		return err
	}
	if !isContainedWitnessBrokerCapability(path) {
		return fmt.Errorf("command %q is not in the reviewed broker capability registry", strings.Join(path, " "))
	}
	if len(args) < len(path) {
		return errors.New("broker command path is truncated")
	}
	for index, segment := range path {
		if args[index] != segment {
			return fmt.Errorf("broker command path must use exact name %q", strings.Join(path, " "))
		}
	}
	if err := validateBrokerCommandArguments(command, args[len(path):]); err != nil {
		return err
	}
	if len(path) == 2 && path[0] == "dog" && path[1] == "done" {
		return validateDogDoneBrokerRequest(args, os.Getenv("GT_ROLE"), os.Getenv("GT_DOG_NAME"))
	}
	if len(path) == 2 && path[0] == "witness" && path[1] == "handle-lifecycle" {
		return validateWitnessLifecycleBrokerRequest(args, os.Getenv("GT_ROLE"), os.Getenv("GT_RIG"))
	}
	return nil
}

func validateWitnessLifecycleBrokerRequest(args []string, role, ownedRig string) error {
	if role != "witness" || ownedRig == "" {
		return errors.New("lifecycle broker request has no owned Witness rig")
	}
	if len(args) != 4 || args[0] != "witness" || args[1] != "handle-lifecycle" ||
		args[2] == "" || args[3] == "" {
		return errors.New("lifecycle broker request must carry one exact rig and message ID")
	}
	if args[2] != ownedRig {
		return errors.New("lifecycle broker request does not match the owned Witness rig")
	}
	return nil
}

func validateDogDoneBrokerRequest(args []string, role, ownedDog string) error {
	if role != "dog" || ownedDog == "" {
		return errors.New("dog closeout broker request has no owned dog identity")
	}
	if len(args) != 5 || args[0] != "dog" || args[1] != "done" || args[2] == "" || args[3] != "--finalizer" || args[4] == "" {
		return errors.New("dog closeout broker request must carry one exact finalizer snapshot")
	}
	if args[2] != ownedDog {
		return errors.New("dog closeout broker request does not match the owned dog")
	}
	snapshot, err := dogCloseoutSnapshotFromEncoded(args[4])
	if err != nil {
		return err
	}
	if snapshot.Name != ownedDog || snapshot.SessionGeneration == nil {
		return errors.New("dog closeout snapshot does not match the owned dog generation")
	}
	return nil
}

func isDetachedSessionBrokerCommand(args []string) bool {
	return len(args) == 5 && args[0] == "dog" && args[1] == "done" &&
		args[2] != "" && args[3] == "--finalizer" && args[4] != ""
}

func shouldBypassSessionBrokerForDogDone(args []string, role, ownedDog string, brokerWorker bool) bool {
	if brokerWorker || role != "dog" || ownedDog == "" {
		return false
	}
	if len(args) == 2 && args[0] == "dog" && args[1] == "done" {
		return true
	}
	return len(args) == 3 && args[0] == "dog" && args[1] == "done" && args[2] == ownedDog
}

func exactBrokerCommandPath(root, command *cobra.Command) ([]string, error) {
	var reversed []string
	current := command
	for current != nil && current != root {
		reversed = append(reversed, current.Name())
		current = current.Parent()
	}
	if current != root {
		return nil, errors.New("broker command is not attached to the supplied root")
	}
	path := make([]string, len(reversed))
	for index := range reversed {
		path[len(reversed)-1-index] = reversed[index]
	}
	return path, nil
}

type brokerFlagState struct {
	flag    *pflag.Flag
	value   string
	slice   []string
	isSlice bool
	changed bool
}

func validateBrokerCommandArguments(command *cobra.Command, args []string) (retErr error) {
	flags := command.Flags()
	states := make([]brokerFlagState, 0, flags.NFlag())
	flags.VisitAll(func(flag *pflag.Flag) {
		state := brokerFlagState{flag: flag, value: flag.Value.String(), changed: flag.Changed}
		if sliceValue, ok := flag.Value.(pflag.SliceValue); ok {
			state.slice = append([]string(nil), sliceValue.GetSlice()...)
			state.isSlice = true
		}
		states = append(states, state)
		// Cobra commands are process-global and other in-process callers may
		// leave Changed set. Broker validation must authorize only the flags in
		// this request, then restore the caller's exact prior state below.
		flag.Changed = false
	})
	defer func() {
		var restoreErrors []error
		for _, state := range states {
			var err error
			if state.isSlice {
				err = state.flag.Value.(pflag.SliceValue).Replace(state.slice)
			} else {
				err = state.flag.Value.Set(state.value)
			}
			if err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restoring --%s: %w", state.flag.Name, err))
			}
			state.flag.Changed = state.changed
		}
		retErr = errors.Join(retErr, errors.Join(restoreErrors...))
	}()

	if err := command.ParseFlags(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parsing broker command flags: %w", err)
	}
	allowedFlags := make(map[string]struct{})
	for _, name := range strings.Split(command.Annotations[brokerSafeFlagsAnnotation], ",") {
		if name = strings.TrimSpace(name); name != "" {
			allowedFlags[name] = struct{}{}
		}
	}
	var forbiddenFlag string
	// FlagSet.Visit walks pflag's process-global "actual" cache, which retains
	// flags parsed by earlier in-process Cobra executions even after Changed is
	// restored. Inspect the freshly reset Changed bits directly so authorization
	// covers only this broker request.
	flags.VisitAll(func(flag *pflag.Flag) {
		if forbiddenFlag != "" {
			return
		}
		if !flag.Changed {
			return
		}
		if _, ok := allowedFlags[flag.Name]; !ok {
			forbiddenFlag = flag.Name
		}
	})
	if forbiddenFlag != "" {
		return fmt.Errorf("broker command flag --%s is not allowed", forbiddenFlag)
	}
	positionals := command.Flags().Args()
	switch policy := command.Annotations[brokerSafeArgsAnnotation]; policy {
	case "", brokerSafeArgsCobra:
		if err := command.ValidateArgs(positionals); err != nil {
			return fmt.Errorf("validating broker command arguments: %w", err)
		}
	case brokerSafeArgsNone:
		if len(positionals) != 0 {
			return fmt.Errorf("broker command accepts no positional arguments; got %d", len(positionals))
		}
	case brokerSafeArgsExactOne:
		if len(positionals) != 1 {
			return fmt.Errorf("broker command requires exactly one positional argument; got %d", len(positionals))
		}
	case brokerSafeArgsMaximumOne:
		if len(positionals) > 1 {
			return fmt.Errorf("broker command accepts at most one positional argument; got %d", len(positionals))
		}
	default:
		if strings.HasPrefix(policy, "exact:") {
			count, err := strconv.Atoi(strings.TrimPrefix(policy, "exact:"))
			if err == nil && len(positionals) == count {
				break
			}
		}
		return fmt.Errorf("invalid broker argument policy %q", policy)
	}
	if command == mailSendCmd && os.Getenv("GT_ROLE") == "witness" &&
		(len(positionals) != 1 || positionals[0] != "mayor/") {
		return errors.New("contained Witness may send brokered mail only to mayor/")
	}
	if err := command.ValidateRequiredFlags(); err != nil {
		return fmt.Errorf("validating broker required flags: %w", err)
	}
	if err := command.ValidateFlagGroups(); err != nil {
		return fmt.Errorf("validating broker flag groups: %w", err)
	}
	return nil
}
