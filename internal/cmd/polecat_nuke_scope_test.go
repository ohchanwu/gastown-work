package cmd

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"testing"

	polecatpkg "github.com/steveyegge/gastown/internal/polecat"
	rigpkg "github.com/steveyegge/gastown/internal/rig"
	tmuxpkg "github.com/steveyegge/gastown/internal/tmux"
)

func TestRunPolecatNukeDoesNotSweepTownWideOrphans(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "polecat.go", nil, 0)
	if err != nil {
		t.Fatalf("parse polecat.go: %v", err)
	}

	if calls := callsTo(t, file, "runPolecatNuke", "cleanupOrphanedProcesses"); calls != 0 {
		t.Fatalf("runPolecatNuke called town-wide cleanup %d time(s); want target-only cleanup", calls)
	}
	if calls := callsTo(t, file, "runPolecatStale", "cleanupOrphanedProcesses"); calls == 0 {
		t.Fatal("runPolecatStale no longer exposes the independently gated orphan cleanup path")
	}
}

func TestPolecatNukeUsesOnlyLocalRemovalPrimitives(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "polecat.go", nil, 0)
	if err != nil {
		t.Fatalf("parse polecat.go: %v", err)
	}

	if calls := callsTo(t, file, "nukePolecatFullWithOptions", "Push"); calls != 0 {
		t.Fatalf("nukePolecatFullWithOptions called Push %d time(s), want 0", calls)
	}
	if calls := callsTo(t, file, "nukePolecatFullWithOptions", "RemoveWithOptions"); calls != 0 {
		t.Fatalf("nukePolecatFullWithOptions called publishing RemoveWithOptions %d time(s), want 0", calls)
	}
	if calls := callsTo(t, file, "nukePolecatFullWithOptions", "RemoveWithOptionsLocalOnlyIfIncarnation"); calls != 1 {
		t.Fatalf("nukePolecatFullWithOptions incarnation-bound local-only removal calls = %d, want 1", calls)
	}
	if calls := callsTo(t, file, "nukePolecatFullWithOptions", "resetPolecatAgentBeadForReuse"); calls != 0 {
		t.Fatalf("nukePolecatFullWithOptions stale-name bead resets = %d, want 0", calls)
	}
	if !callNestedWithinCallArgument(t, file, "nukePolecatFullWithOptions", "RemoveWithOptionsLocalOnlyIfIncarnation", "deletePreservedLocalPolecatBranch") {
		t.Fatal("local branch deletion is not contained by the exact polecat lifecycle transaction")
	}
}

func TestPolecatNukeDryRunAndRealNukeShareCustodyProof(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "polecat.go", nil, 0)
	if err != nil {
		t.Fatalf("parse polecat.go: %v", err)
	}

	if refs := referencesIdentifier(t, file, "runPolecatNuke", "provePolecatNukeCustody"); refs != 1 {
		t.Fatalf("runPolecatNuke custody proof references = %d, want 1", refs)
	}
	if calls := callsTo(t, file, "nukePolecatFullWithOptions", "provePolecatNukeCustody"); calls != 2 {
		t.Fatalf("real nuke custody proof calls = %d, want initial and lifecycle-locked recheck", calls)
	}
	if calls := callsTo(t, file, "provePolecatNukeCustody", "checkPolecatSafety"); calls != 1 {
		t.Fatalf("custody proof safety rechecks = %d, want 1 inside each proof", calls)
	}
}

func TestRunPolecatNukeLockedTeardownFailsClosedOnSessionError(t *testing.T) {
	stopErr := errors.New("exact session custody changed")
	mutations := 0
	err := runPolecatNukeLockedTeardown(
		func() error { return stopErr },
		func() { mutations++ },
	)
	if !errors.Is(err, stopErr) {
		t.Fatalf("error = %v, want %v", err, stopErr)
	}
	if mutations != 0 {
		t.Fatalf("post-stop mutations = %d, want 0", mutations)
	}
}

func TestRunPolecatNukeLockedTeardownMutatesOnlyAfterExactStop(t *testing.T) {
	mutations := 0
	err := runPolecatNukeLockedTeardown(
		func() error { return nil },
		func() { mutations++ },
	)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if mutations != 1 {
		t.Fatalf("post-stop mutations = %d, want 1", mutations)
	}
}

func TestPolecatNukeDryRunRefusesWhenCustodyProofFails(t *testing.T) {
	oldDryRun, oldForce := polecatNukeDryRun, polecatNukeForce
	polecatNukeDryRun, polecatNukeForce = true, true
	t.Cleanup(func() {
		polecatNukeDryRun, polecatNukeForce = oldDryRun, oldForce
	})

	proofCalls := 0
	r := &rigpkg.Rig{Name: "rig", Path: t.TempDir()}
	mgr := polecatpkg.NewManager(r, nil, nil)
	targets := []polecatTarget{{rigName: "rig", polecatName: "nitro", mgr: mgr, r: r}}
	output := capturePolecatNukeStdout(t, func() {
		err := runResolvedPolecatNuke(targets, func(string, string, *polecatpkg.Manager, *rigpkg.Rig, nukePolecatOptions) (polecatNukeCustody, error) {
			proofCalls++
			return polecatNukeCustody{}, errors.New("branch custody is ambiguous")
		})
		if err != nil {
			t.Fatalf("runResolvedPolecatNuke: %v", err)
		}
	})

	if proofCalls != 1 {
		t.Fatalf("custody proof calls = %d, want 1", proofCalls)
	}
	if !strings.Contains(output, "Would refuse to nuke rig/nitro") {
		t.Fatalf("output did not report refusal:\n%s", output)
	}
	if !strings.Contains(output, "branch custody is ambiguous") {
		t.Fatalf("output did not explain custody failure:\n%s", output)
	}
	if strings.Contains(output, "Would nuke rig/nitro") {
		t.Fatalf("output falsely advertised nuke:\n%s", output)
	}
	for _, action := range []string{"Kill session", "Delete worktree", "Delete branch", "Reset agent bead"} {
		if strings.Contains(output, action) {
			t.Fatalf("output advertised blocked action %q:\n%s", action, output)
		}
	}
}

func TestWriteDryRunNukePlanReportsOnlyExecutableActions(t *testing.T) {
	target := polecatTarget{rigName: "rig", polecatName: "nitro", r: &rigpkg.Rig{Path: "/town/rig"}}
	tests := []struct {
		name       string
		result     *SafetyCheckResult
		custodyErr error
		force      bool
		wantBlock  bool
		wantAction bool
	}{
		{name: "safe", result: &SafetyCheckResult{}, wantAction: true},
		{name: "safety blocked", result: &SafetyCheckResult{Blocked: true, Reasons: []string{"has active work"}}, wantBlock: true},
		{name: "forced safety", result: &SafetyCheckResult{Blocked: true, Reasons: []string{"has active work"}}, force: true, wantAction: true},
		{name: "custody blocked", result: &SafetyCheckResult{}, custodyErr: errors.New("generation changed"), force: true, wantBlock: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			blocked := writeDryRunNukePlan(&out, target, tt.result, tt.custodyErr, tt.force)
			if blocked != tt.wantBlock {
				t.Fatalf("blocked = %v, want %v; output=%q", blocked, tt.wantBlock, out.String())
			}
			hasAction := strings.Contains(out.String(), "Kill session")
			if hasAction != tt.wantAction {
				t.Fatalf("action output = %v, want %v; output=%q", hasAction, tt.wantAction, out.String())
			}
		})
	}
}

func TestTargetSessionNukePreservesUnrelatedSessionsOnDedicatedSocket(t *testing.T) {
	socket := fmt.Sprintf("gt-nuke-scope-%d", os.Getpid())
	tm := tmuxpkg.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = tm.KillServer() })

	for _, name := range []string{"gt-target", "gt-unrelated-a", "gt-unrelated-b"} {
		if err := tm.NewSessionWithCommand(name, t.TempDir(), "sleep 300"); err != nil {
			t.Fatalf("create session %s: %v", name, err)
		}
	}

	sessions := polecatpkg.NewSessionManager(tm, &rigpkg.Rig{Name: "scope"})
	if err := sessions.Stop("target", true); err != nil {
		t.Fatalf("nuke target session: %v", err)
	}

	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: "gt-target", want: false},
		{name: "gt-unrelated-a", want: true},
		{name: "gt-unrelated-b", want: true},
	} {
		got, err := tm.HasSession(tc.name)
		if err != nil {
			t.Fatalf("check session %s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("session %s alive = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func callsTo(t *testing.T, file *ast.File, function, callee string) int {
	t.Helper()

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == function {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("function %s not found", function)
	}

	calls := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if name == callee {
			calls++
		}
		return true
	})
	return calls
}

func callNestedWithinCallArgument(t *testing.T, file *ast.File, function, outer, inner string) bool {
	t.Helper()

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == function {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("function %s not found", function)
	}

	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || calledFunctionName(call) != outer {
			return true
		}
		for _, arg := range call.Args {
			ast.Inspect(arg, func(nested ast.Node) bool {
				innerCall, ok := nested.(*ast.CallExpr)
				if ok && calledFunctionName(innerCall) == inner {
					found = true
				}
				return !found
			})
		}
		return !found
	})
	return found
}

func calledFunctionName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	default:
		return ""
	}
}

func referencesIdentifier(t *testing.T, file *ast.File, function, identifier string) int {
	t.Helper()

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == function {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("function %s not found", function)
	}

	references := 0
	ast.Inspect(body, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if ok && ident.Name == identifier {
			references++
		}
		return true
	})
	return references
}

func capturePolecatNukeStdout(t *testing.T, run func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })

	output := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		output <- data
	}()

	run()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	os.Stdout = original
	data := <-output
	if err := reader.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	return string(data)
}
