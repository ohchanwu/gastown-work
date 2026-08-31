package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func formulaMutationTestProof(id string) (string, error) {
	return "generation-" + id, nil
}

func formulaMutationTestIdentity(string) error { return nil }

func TestFormulaMutationPublicationRejectsMutableJournalRequestSubstitution(t *testing.T) {
	townRoot := t.TempDir()
	key := formulaMutationAttemptKey{Kind: "wisp", Formula: "mol-test", Owner: "gastown/polecats/test"}
	if err := writeFormulaMutationAttempt(townRoot, &formulaMutationAttempt{
		Version: formulaMutationAttemptVersion, Key: key, Scope: formulaMutationScope(townRoot),
		RequestFingerprint: "attacker-request", OperationNonce: "56565656-5656-4656-8656-565656565656",
		RootID: "gt-root", RootGeneration: "generation-gt-root",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := formulaMutationPublication(townRoot, townRoot, key, "gt-root", "trusted-request", "trusted-actor"); err == nil {
		t.Fatal("mutable formula journal substituted a different authorized request")
	}
}

func TestFormulaMutationAlreadyPublishedRecoversStandaloneRootWithoutReproof(t *testing.T) {
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}
	townRoot := t.TempDir()
	bd := beads.NewIsolatedWithPort(townRoot, port)
	if err := bd.Init("gt"); err != nil {
		t.Fatal(err)
	}
	root, err := bd.Create(beads.CreateOptions{Title: "formula", Type: "molecule", Priority: 2, Description: "before", Actor: "operation-actor", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := bd.CaptureFormulaMoleculeGraph(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := beads.FormulaMoleculeGraphGeneration(graph)
	if err != nil {
		t.Fatal(err)
	}
	key := formulaMutationAttemptKey{Kind: "wisp", Formula: "mol-test", Owner: "gastown/polecats/test"}
	attempt := &formulaMutationAttempt{
		Version: formulaMutationAttemptVersion, Key: key, Scope: formulaMutationScope(townRoot),
		RequestFingerprint: "request-fingerprint", OperationNonce: "45454545-4545-4545-8545-454545454545",
		RootID: root.ID, RootGeneration: generation,
	}
	authorization := beads.FormulaMoleculeAuthorization{
		PublicationID: attempt.OperationNonce, RootID: root.ID, RootGeneration: generation,
		Formula: key.Formula, Owner: key.Owner, RequestFingerprint: attempt.RequestFingerprint, Actor: "operation-actor",
	}
	commit, err := bd.PrepareFormulaMoleculeAuthorization(authorization)
	if err != nil {
		t.Fatal(err)
	}
	attempt.AuthorizationCommit = commit
	if err := writeFormulaMutationAttempt(townRoot, attempt); err != nil {
		t.Fatal(err)
	}
	publication := beads.FormulaMoleculePublication{
		PublicationID: attempt.OperationNonce, RootID: root.ID, RootGeneration: generation, WorkID: root.ID,
		ExpectedStatus: root.Status, ExpectedAssignee: root.Assignee, ExpectedDescription: root.Description,
		NewStatus: "hooked", NewAssignee: key.Owner, NewDescription: "published metadata",
		Authorization: authorization, AuthorizationCommit: commit,
	}
	if err := bd.PublishFormulaMoleculeAssignment(publication); err != nil {
		t.Fatal(err)
	}
	published, err := formulaMutationAlreadyPublished(townRoot, townRoot, key, root.ID, key.Owner)
	if err != nil || !published {
		t.Fatalf("standalone publication recovery = (%v, %v)", published, err)
	}
}

func writeLegacyFormulaMutationAttemptFixture(t *testing.T, townRoot string, attempt formulaMutationAttempt) {
	t.Helper()
	legacy := struct {
		Key                formulaMutationAttemptKey `json:"key"`
		Scope              string                    `json:"scope"`
		RequestFingerprint string                    `json:"request_fingerprint"`
		BeforeIDs          []string                  `json:"before_ids"`
		RootID             string                    `json:"root_id,omitempty"`
		RootGeneration     string                    `json:"root_generation,omitempty"`
	}{
		Key: attempt.Key, Scope: attempt.Scope, RequestFingerprint: attempt.RequestFingerprint,
		BeforeIDs: attempt.BeforeIDs, RootID: attempt.RootID, RootGeneration: attempt.RootGeneration,
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path := formulaMutationAttemptPath(townRoot, attempt.Key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFormulaMutationTestInventory(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func TestFormulaMutationAttemptRecoversCrashAfterCommittedCreate(t *testing.T) {
	const childEnv = "GT_TEST_FORMULA_MUTATION_CRASH_CHILD"
	if os.Getenv(childEnv) == "1" {
		townRoot := os.Getenv("GT_TEST_FORMULA_MUTATION_TOWN")
		inventoryPath := os.Getenv("GT_TEST_FORMULA_MUTATION_INVENTORY")
		markerPath := os.Getenv("GT_TEST_FORMULA_MUTATION_MARKER")
		key := formulaMutationAttemptKey{Kind: "wisp", Formula: "mol-test", Owner: "gastown/polecats/test"}
		_, _ = executeFormulaMutationAttempt(
			context.Background(),
			townRoot,
			townRoot,
			key,
			"request-v1",
			func() (map[string]bool, error) { return readFormulaMutationTestInventory(inventoryPath) },
			func() ([]byte, error) {
				if _, err := os.Stat(formulaMutationAttemptPath(townRoot, key)); err != nil {
					return nil, err
				}
				if err := os.WriteFile(inventoryPath, []byte(`["gt-old","gt-created"]`), 0o600); err != nil {
					return nil, err
				}
				if err := os.WriteFile(markerPath, []byte("mutated\n"), 0o600); err != nil {
					return nil, err
				}
				os.Exit(86)
				return nil, nil
			},
			func([]byte) (string, bool) { return "", false },
			formulaMutationTestProof,
		)
		os.Exit(87)
	}

	townRoot := t.TempDir()
	inventoryPath := filepath.Join(townRoot, "inventory.json")
	markerPath := filepath.Join(townRoot, "mutations.log")
	if err := os.WriteFile(inventoryPath, []byte(`["gt-old"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestFormulaMutationAttemptRecoversCrashAfterCommittedCreate$")
	child.Env = append(os.Environ(),
		childEnv+"=1",
		"GT_TEST_FORMULA_MUTATION_TOWN="+townRoot,
		"GT_TEST_FORMULA_MUTATION_INVENTORY="+inventoryPath,
		"GT_TEST_FORMULA_MUTATION_MARKER="+markerPath,
	)
	if err := child.Run(); err == nil {
		t.Fatal("crash helper exited successfully")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 86 {
		t.Fatalf("crash helper = %v, want exit 86", err)
	}

	key := formulaMutationAttemptKey{Kind: "wisp", Formula: "mol-test", Owner: "gastown/polecats/test"}
	root, err := executeFormulaMutationAttempt(
		context.Background(),
		townRoot,
		townRoot,
		key,
		"request-v1",
		func() (map[string]bool, error) { return readFormulaMutationTestInventory(inventoryPath) },
		func() ([]byte, error) {
			t.Fatal("recovery repeated an already committed formula creation")
			return nil, nil
		},
		func([]byte) (string, bool) { return "", false },
		formulaMutationTestProof,
	)
	if err != nil || root != "gt-created" {
		t.Fatalf("crash recovery = (%q, %v), want gt-created", root, err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil || strings.Count(string(marker), "mutated") != 1 {
		t.Fatalf("mutation marker = (%q, %v), want one mutation", marker, err)
	}
}

func TestFormulaMutationAttemptReconcilesLostBondAcknowledgement(t *testing.T) {
	townRoot := t.TempDir()
	inventory := map[string]bool{"gt-old": true}
	mutations := 0
	verified := ""
	key := formulaMutationAttemptKey{Kind: "bond", Formula: "mol-test", Owner: "gt-work"}
	root, err := executeFormulaMutationAttempt(
		context.Background(),
		townRoot,
		townRoot,
		key,
		"request-v1",
		func() (map[string]bool, error) {
			copy := make(map[string]bool, len(inventory))
			for id := range inventory {
				copy[id] = true
			}
			return copy, nil
		},
		func() ([]byte, error) {
			mutations++
			inventory["gt-bonded"] = true
			return nil, errors.New("bond acknowledgement lost")
		},
		func([]byte) (string, bool) { return "", false },
		func(candidate string) (string, error) {
			verified = candidate
			return formulaMutationTestProof(candidate)
		},
	)
	if err != nil || root != "gt-bonded" || mutations != 1 || verified != root {
		t.Fatalf("lost bond reconciliation = root %q err %v mutations %d verified %q", root, err, mutations, verified)
	}
	replayed, err := executeFormulaMutationAttempt(
		context.Background(),
		townRoot,
		townRoot,
		key,
		"request-v1",
		func() (map[string]bool, error) { return inventory, nil },
		func() ([]byte, error) {
			t.Fatal("existing-target retry repeated the committed bond")
			return nil, nil
		},
		nil,
		func(candidate string) (string, error) {
			if candidate != "gt-bonded" {
				return "", errors.New("replayed candidate lost exact ownership")
			}
			return formulaMutationTestProof(candidate)
		},
	)
	if err != nil || replayed != root || mutations != 1 {
		t.Fatalf("existing-target bond retry = root %q err %v mutations %d", replayed, err, mutations)
	}
}

func TestFormulaMutationAttemptPersistsOperationActorBeforeMutation(t *testing.T) {
	townRoot := t.TempDir()
	key := formulaMutationAttemptKey{Kind: "wisp", Formula: "mol-test", Owner: "gastown/polecats/test"}
	inventory := map[string]bool{}
	mutationActor := ""
	root, err := executeFormulaMutationAttempt(
		context.Background(), townRoot, townRoot, key, "request-v1",
		func() (map[string]bool, error) { return inventory, nil },
		func() ([]byte, error) {
			actor, actorErr := formulaMutationActor(townRoot, key)
			if actorErr != nil {
				return nil, actorErr
			}
			mutationActor = actor
			inventory["gt-root"] = true
			return nil, nil
		},
		nil,
		func(candidate string) (string, error) {
			actor, actorErr := formulaMutationActor(townRoot, key)
			if actorErr != nil || actor != mutationActor {
				return "", fmt.Errorf("durable actor for %s = %q, %v", candidate, actor, actorErr)
			}
			return formulaMutationTestProof(candidate)
		},
	)
	if err != nil || root != "gt-root" {
		t.Fatalf("operation-bound mutation = (%q, %v)", root, err)
	}
	attempt, err := loadFormulaMutationAttempt(townRoot, key)
	if err != nil || attempt == nil || attempt.OperationNonce == "" {
		t.Fatalf("durable operation nonce = (%+v, %v)", attempt, err)
	}
	if !strings.Contains(mutationActor, attempt.OperationNonce) || !strings.HasSuffix(mutationActor, attempt.RequestFingerprint) {
		t.Fatalf("operation actor %q does not bind nonce and request", mutationActor)
	}
}

func TestFormulaMutationAttemptMigratesLegacyRootlessJournalBeforeMutation(t *testing.T) {
	townRoot := t.TempDir()
	key := formulaMutationAttemptKey{Kind: "wisp", Formula: "mol-test", Owner: "gastown/polecats/test"}
	writeLegacyFormulaMutationAttemptFixture(t, townRoot, formulaMutationAttempt{
		Key: key, Scope: formulaMutationScope(townRoot), RequestFingerprint: "request-v1", BeforeIDs: []string{"gt-old"},
	})
	inventory := map[string]bool{"gt-old": true}
	mutationActor := ""
	root, err := executeFormulaMutationAttempt(
		context.Background(), townRoot, townRoot, key, "request-v1",
		func() (map[string]bool, error) { return inventory, nil },
		func() ([]byte, error) {
			var actorErr error
			mutationActor, actorErr = formulaMutationActor(townRoot, key)
			if actorErr != nil {
				return nil, actorErr
			}
			inventory["gt-created"] = true
			return nil, nil
		},
		nil, formulaMutationTestProof,
	)
	if err != nil || root != "gt-created" || mutationActor == "" {
		t.Fatalf("legacy pre-mutation migration = root %q actor %q err %v", root, mutationActor, err)
	}
	attempt, err := loadFormulaMutationAttempt(townRoot, key)
	if err != nil || attempt == nil || attempt.OperationNonce == "" {
		t.Fatalf("migrated legacy attempt = (%+v, %v), want operation nonce", attempt, err)
	}
}

func TestFormulaMutationAttemptReconcilesLegacyLostAcknowledgement(t *testing.T) {
	for _, rooted := range []bool{false, true} {
		name := "rootless"
		if rooted {
			name = "rooted"
		}
		t.Run(name, func(t *testing.T) {
			townRoot := t.TempDir()
			key := formulaMutationAttemptKey{Kind: "bond", Formula: "mol-test", Owner: "gt-work"}
			legacy := formulaMutationAttempt{
				Key: key, Scope: formulaMutationScope(townRoot), RequestFingerprint: "request-v1", BeforeIDs: []string{"gt-old"},
			}
			if rooted {
				legacy.RootID = "gt-created"
				legacy.RootGeneration = "generation-gt-created"
			}
			writeLegacyFormulaMutationAttemptFixture(t, townRoot, legacy)
			inventory := map[string]bool{"gt-old": true, "gt-created": true}
			verified := false
			root, err := executeFormulaMutationAttempt(
				context.Background(), townRoot, townRoot, key, "request-v1",
				func() (map[string]bool, error) { return inventory, nil },
				func() ([]byte, error) {
					t.Fatal("legacy lost-ack recovery repeated the committed mutation")
					return nil, nil
				},
				nil,
				func(candidate string) (string, error) {
					actor, actorErr := formulaMutationActor(townRoot, key)
					if actorErr != nil || actor != "" {
						return "", fmt.Errorf("legacy recovery actor = %q, %v", actor, actorErr)
					}
					verified = candidate == "gt-created"
					return formulaMutationTestProof(candidate)
				},
			)
			if !rooted {
				if err == nil || root != "" || verified || !strings.Contains(err.Error(), "rootless legacy formula mutation has no durable provenance") {
					t.Fatalf("rootless legacy delta = root %q verified %v err %v, want provenance rejection", root, verified, err)
				}
				return
			}
			if err != nil || root != "gt-created" || !verified {
				t.Fatalf("legacy lost-ack recovery = root %q verified %v err %v", root, verified, err)
			}
			attempt, err := loadFormulaMutationAttempt(townRoot, key)
			if err != nil || attempt == nil || attempt.RootID != root || attempt.RootGeneration != "generation-gt-created" || attempt.OperationNonce != "" {
				t.Fatalf("versioned legacy root = (%+v, %v)", attempt, err)
			}
		})
	}
}

func TestFormulaMutationAttemptUsesOwnedInventoryRootInsteadOfConflictingOutput(t *testing.T) {
	townRoot := t.TempDir()
	inventory := map[string]bool{"gt-old": true}
	root, err := executeFormulaMutationAttempt(
		context.Background(),
		townRoot,
		townRoot,
		formulaMutationAttemptKey{Kind: "bond", Formula: "mol-test", Owner: "gt-work"},
		"request-v1",
		func() (map[string]bool, error) {
			copy := make(map[string]bool, len(inventory))
			for id := range inventory {
				copy[id] = true
			}
			return copy, nil
		},
		func() ([]byte, error) {
			inventory["gt-owned"] = true
			return []byte(`{"root_id":"gt-foreign"}`), nil
		},
		func([]byte) (string, bool) { return "gt-foreign", true },
		func(candidate string) (string, error) {
			if candidate != "gt-owned" {
				return "", errors.New("candidate lacks the exact work bond")
			}
			return formulaMutationTestProof(candidate)
		},
	)
	if err != nil || root != "gt-owned" {
		t.Fatalf("owned inventory reconciliation = (%q, %v), want gt-owned", root, err)
	}
}

func TestFormulaMutationAttemptRejectsForeignDeltaWithoutExactIdentity(t *testing.T) {
	townRoot := t.TempDir()
	inventory := map[string]bool{}
	root, err := executeFormulaMutationAttempt(
		context.Background(), townRoot, townRoot,
		formulaMutationAttemptKey{Kind: "wisp", Formula: "mol-test", Owner: "gastown/polecats/test"},
		"request-v1",
		func() (map[string]bool, error) { return inventory, nil },
		func() ([]byte, error) {
			inventory["gt-foreign"] = true
			return nil, errors.New("mutation acknowledgement lost")
		},
		nil,
		func(candidate string) (string, error) {
			return "", fmt.Errorf("candidate %s does not match the requested formula graph", candidate)
		},
	)
	if err == nil || root != "" || !strings.Contains(err.Error(), "does not match the requested formula graph") {
		t.Fatalf("foreign delta reconciliation = (%q, %v)", root, err)
	}
}

func TestFormulaMutationAttemptRequiresIdentityProof(t *testing.T) {
	mutated := false
	_, err := executeFormulaMutationAttempt(
		context.Background(), t.TempDir(), t.TempDir(),
		formulaMutationAttemptKey{Kind: "wisp", Formula: "mol-test", Owner: "gastown/polecats/test"},
		"request-v1",
		func() (map[string]bool, error) { return map[string]bool{}, nil },
		func() ([]byte, error) { mutated = true; return nil, nil },
		nil, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "identity") || mutated {
		t.Fatalf("missing identity proof = err %v mutated %v", err, mutated)
	}
}

func TestFormulaMutationAttemptRejectsChangedRequestWhileRootExists(t *testing.T) {
	townRoot := t.TempDir()
	inventory := map[string]bool{}
	key := formulaMutationAttemptKey{Kind: "bond", Formula: "mol-test", Owner: "gt-work"}
	root, err := executeFormulaMutationAttempt(
		context.Background(), townRoot, townRoot, key,
		formulaMutationRequestFingerprint("feature=old"),
		func() (map[string]bool, error) { return inventory, nil },
		func() ([]byte, error) { inventory["gt-root"] = true; return nil, nil },
		nil, formulaMutationTestProof,
	)
	if err != nil || root != "gt-root" {
		t.Fatalf("initial mutation = (%q, %v)", root, err)
	}
	mutated := false
	_, err = executeFormulaMutationAttempt(
		context.Background(), townRoot, townRoot, key,
		formulaMutationRequestFingerprint("feature=changed"),
		func() (map[string]bool, error) { return inventory, nil },
		func() ([]byte, error) { mutated = true; return nil, nil },
		nil, formulaMutationTestProof,
	)
	if err == nil || !strings.Contains(err.Error(), "request differs") || mutated {
		t.Fatalf("changed request = err %v mutated %v", err, mutated)
	}
}

func TestFormulaMutationAttemptRejectsRecreatedRootGeneration(t *testing.T) {
	townRoot := t.TempDir()
	inventory := map[string]bool{}
	generation := "generation-1"
	snapshot := func(string) (string, error) { return generation, nil }
	key := formulaMutationAttemptKey{Kind: "bond", Formula: "mol-test", Owner: "gt-work"}
	root, err := executeFormulaMutationAttempt(
		context.Background(), townRoot, townRoot, key, "request-v1",
		func() (map[string]bool, error) { return inventory, nil },
		func() ([]byte, error) { inventory["gt-root"] = true; return nil, nil },
		nil, snapshot,
	)
	if err != nil || root != "gt-root" {
		t.Fatalf("initial mutation = (%q, %v)", root, err)
	}
	generation = "generation-2"
	_, err = executeFormulaMutationAttempt(
		context.Background(), townRoot, townRoot, key, "request-v1",
		func() (map[string]bool, error) { return inventory, nil },
		func() ([]byte, error) { t.Fatal("generation mismatch repeated mutation"); return nil, nil },
		nil, snapshot,
	)
	if err == nil || !strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("recreated root error = %v", err)
	}
}

func TestFormulaMutationAttemptSerializesDifferentOwnersInOneDatabase(t *testing.T) {
	townRoot := t.TempDir()
	inventory := map[string]bool{}
	var inventoryMu sync.Mutex
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	list := func() (map[string]bool, error) {
		inventoryMu.Lock()
		defer inventoryMu.Unlock()
		copy := make(map[string]bool, len(inventory))
		for id := range inventory {
			copy[id] = true
		}
		return copy, nil
	}
	mutate := func(root string, entered, peerEntered chan struct{}) func() ([]byte, error) {
		return func() ([]byte, error) {
			close(entered)
			select {
			case <-peerEntered:
			case <-time.After(100 * time.Millisecond):
			}
			inventoryMu.Lock()
			inventory[root] = true
			inventoryMu.Unlock()
			return nil, nil
		}
	}
	type result struct {
		root string
		err  error
	}
	results := make(chan result, 2)
	run := func(owner, root string, entered, peerEntered chan struct{}) {
		got, err := executeFormulaMutationAttempt(
			context.Background(), townRoot, townRoot,
			formulaMutationAttemptKey{Kind: "bond", Formula: "mol-test", Owner: owner}, owner,
			list, mutate(root, entered, peerEntered), nil, formulaMutationTestProof,
		)
		results <- result{got, err}
	}
	go run("gt-a", "gt-root-a", firstEntered, secondEntered)
	<-firstEntered
	go run("gt-b", "gt-root-b", secondEntered, firstEntered)
	for range 2 {
		got := <-results
		if got.err != nil || (got.root != "gt-root-a" && got.root != "gt-root-b") {
			t.Fatalf("serialized mutation = (%q, %v)", got.root, got.err)
		}
	}
}

func TestFormulaMutationAttemptBlocksAnotherOwnerUntilPendingMutationReconciles(t *testing.T) {
	townRoot := t.TempDir()
	inventory := map[string]bool{}
	list := func() (map[string]bool, error) { return inventory, nil }
	first := formulaMutationAttemptKey{Kind: "bond", Formula: "mol-test", Owner: "gt-a"}
	_, err := executeFormulaMutationAttempt(
		context.Background(), townRoot, townRoot, first, "request-a", list,
		func() ([]byte, error) { return nil, errors.New("unknown mutation outcome") },
		nil, formulaMutationTestProof,
	)
	if err == nil {
		t.Fatal("unknown first outcome unexpectedly succeeded")
	}
	mutated := false
	_, err = executeFormulaMutationAttempt(
		context.Background(), townRoot, townRoot,
		formulaMutationAttemptKey{Kind: "bond", Formula: "mol-test", Owner: "gt-b"}, "request-b", list,
		func() ([]byte, error) { mutated = true; return nil, nil },
		nil, formulaMutationTestProof,
	)
	if err == nil || !strings.Contains(err.Error(), "must be reconciled") || mutated {
		t.Fatalf("second owner = err %v mutated %v", err, mutated)
	}
}
