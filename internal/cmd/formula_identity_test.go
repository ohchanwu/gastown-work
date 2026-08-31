package cmd

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/formula"
)

func TestVerifyFormulaMoleculeIdentityAgainstIsolatedWisp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real Dolt fixture is not supported on Windows")
	}
	port, err := strconv.Atoi(os.Getenv("GT_DOLT_PORT"))
	if err != nil || os.Getenv("GT_TEST_ISOLATED") != "1" {
		t.Skip("isolated Dolt listener is required")
	}
	townRoot := t.TempDir()
	if err := beads.NewIsolatedWithPort(townRoot, port).Init("gt"); err != nil {
		t.Fatal(err)
	}
	if _, err := formula.ProvisionFormulas(townRoot); err != nil {
		t.Fatal(err)
	}
	if err := BdCmd("cook", "shiny").Dir(townRoot).WithAutoCommit().WithGTRoot(townRoot).Run(); err != nil {
		t.Fatal(err)
	}
	const actor = "gt-sling-v1:test-operation"
	out, err := BdCmd("--actor", actor, "mol", "wisp", "shiny", "--var", "feature=identity-probe", "--json").
		Dir(townRoot).WithAutoCommit().WithActor(actor).WithGTRoot(townRoot).Output()
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := parseWispIDFromJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	vars := []string{"feature=identity-probe"}
	if err := verifyFormulaMoleculeIdentity("shiny", rootID, vars, townRoot, townRoot); err != nil {
		t.Fatalf("exact formula identity rejected: %v", err)
	}
	if err := verifyFormulaMoleculeIdentityContext(context.Background(), "shiny", rootID, vars, "", actor, townRoot, townRoot); err != nil {
		t.Fatalf("operation-bound formula identity rejected: %v", err)
	}
	if err := verifyFormulaMoleculeIdentityContext(context.Background(), "shiny", rootID, vars, "", actor+"-foreign", townRoot, townRoot); err == nil {
		t.Fatal("formula identity accepted a foreign operation actor")
	}
	const foreignID = "gt-foreign-formula-dependency"
	if err := BdCmd("create", "foreign formula dependency", "--id", foreignID, "--json").
		Dir(townRoot).WithAutoCommit().WithGTRoot(townRoot).Run(); err != nil {
		t.Fatal(err)
	}
	if err := BdCmd("dep", "add", rootID, foreignID, "--type", "blocks").
		Dir(townRoot).WithAutoCommit().WithActor(actor).WithGTRoot(townRoot).Run(); err != nil {
		t.Fatal(err)
	}
	if err := verifyFormulaMoleculeIdentityContext(context.Background(), "shiny", rootID, vars, "", actor, townRoot, townRoot); err == nil {
		t.Fatal("formula identity accepted an extra graph dependency")
	}
	if err := BdCmd("dep", "remove", rootID, foreignID).
		Dir(townRoot).WithAutoCommit().WithGTRoot(townRoot).Run(); err != nil {
		t.Fatal(err)
	}
	if err := verifyFormulaMoleculeIdentity("shiny", rootID, []string{"feature=foreign"}, townRoot, townRoot); err == nil {
		t.Fatal("formula identity accepted a different rendered request")
	}

	before, err := formulaMutationRootGeneration(townRoot, townRoot, rootID)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := loadFormulaMoleculeGraph(townRoot, townRoot, rootID)
	if err != nil {
		t.Fatal(err)
	}
	childID := ""
	for _, issue := range graph.Issues {
		if issue.ID != rootID {
			childID = issue.ID
			break
		}
	}
	if childID == "" {
		t.Fatal("isolated formula wisp did not materialize a step")
	}
	if err := BdCmd("update", childID, "--description=foreign graph").
		Dir(townRoot).WithAutoCommit().WithGTRoot(townRoot).Run(); err != nil {
		t.Fatal(err)
	}
	after, err := formulaMutationRootGeneration(townRoot, townRoot, rootID)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("formula graph substitution did not change the root generation")
	}
	if err := verifyFormulaMoleculeIdentity("shiny", rootID, vars, townRoot, townRoot); err == nil {
		t.Fatal("formula identity accepted a substituted step graph")
	}
}
