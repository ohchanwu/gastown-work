package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/formula"
)

type formulaMutationAttemptKey struct {
	Kind    string `json:"kind"`
	Formula string `json:"formula"`
	Owner   string `json:"owner"`
}

const (
	formulaMutationAttemptVersion       = 2
	formulaMutationLegacyAttemptVersion = 1
)

type formulaMutationAttempt struct {
	Version             int                       `json:"version"`
	Key                 formulaMutationAttemptKey `json:"key"`
	Scope               string                    `json:"scope"`
	RequestFingerprint  string                    `json:"request_fingerprint"`
	OperationNonce      string                    `json:"operation_nonce"`
	BeforeIDs           []string                  `json:"before_ids"`
	RootID              string                    `json:"root_id,omitempty"`
	RootGeneration      string                    `json:"root_generation,omitempty"`
	AuthorizationCommit string                    `json:"authorization_commit,omitempty"`
	legacy              bool
}

type formulaMoleculeIssue struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	IssueType   string    `json:"issue_type"`
	CreatedAt   time.Time `json:"created_at"`
	Ephemeral   bool      `json:"ephemeral"`
}

type formulaMoleculeDependency struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	CreatedBy   string `json:"created_by"`
}

type formulaMoleculeGraph struct {
	Issues       []formulaMoleculeIssue      `json:"issues"`
	Dependencies []formulaMoleculeDependency `json:"dependencies"`
	Variables    map[string]string           `json:"variables"`
}

func formulaMutationRequestFingerprint(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func formulaMutationRigName(townRoot, workDir string) string {
	rigName := ""
	if rel, err := filepath.Rel(townRoot, workDir); err == nil {
		if first := strings.Split(filepath.ToSlash(rel), "/")[0]; first != "" && first != "." && first != "mayor" && first != "deacon" {
			rigName = first
		}
	}
	return rigName
}

func formulaMutationFormulaGeneration(formulaName, townRoot, workDir string) (string, error) {
	rigName := formulaMutationRigName(townRoot, workDir)
	content, err := formula.ResolveFormulaContent(formulaName, townRoot, rigName)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

func formulaMutationRootGeneration(workDir, townRoot, rootID string) (string, error) {
	return formulaMutationRootGenerationContext(context.Background(), workDir, townRoot, rootID)
}

func formulaMutationRootGenerationContext(ctx context.Context, workDir, townRoot, rootID string) (string, error) {
	graph, err := loadFormulaMoleculeGraphContext(ctx, workDir, townRoot, rootID)
	if err != nil {
		return "", err
	}
	return formulaMutationGraphGeneration(graph)
}

func formulaMutationGraphGeneration(graph *formulaMoleculeGraph) (string, error) {
	if graph == nil {
		return "", fmt.Errorf("formula molecule generation has no graph")
	}
	issues := append([]formulaMoleculeIssue(nil), graph.Issues...)
	dependencies := append([]formulaMoleculeDependency(nil), graph.Dependencies...)
	sort.Slice(issues, func(i, j int) bool { return issues[i].ID < issues[j].ID })
	sort.Slice(dependencies, func(i, j int) bool {
		left, right := dependencies[i], dependencies[j]
		if left.IssueID != right.IssueID {
			return left.IssueID < right.IssueID
		}
		if left.DependsOnID != right.DependsOnID {
			return left.DependsOnID < right.DependsOnID
		}
		return left.Type < right.Type
	})
	encoded, err := json.Marshal(struct {
		Issues       []formulaMoleculeIssue      `json:"issues"`
		Dependencies []formulaMoleculeDependency `json:"dependencies"`
		Variables    map[string]string           `json:"variables,omitempty"`
	}{issues, dependencies, graph.Variables})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func loadFormulaMoleculeGraph(workDir, townRoot, rootID string) (*formulaMoleculeGraph, error) {
	return loadFormulaMoleculeGraphContext(context.Background(), workDir, townRoot, rootID)
}

func loadFormulaMoleculeGraphContext(ctx context.Context, workDir, townRoot, rootID string) (*formulaMoleculeGraph, error) {
	beadsDir := beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), rootID)
	out, err := BdCmd("mol", "show", rootID, "--json").
		Dir(workDir).
		WithBeadsDir(beadsDir).
		WithGTRoot(townRoot).
		OutputContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading formula molecule %s: %w", rootID, err)
	}
	var graph formulaMoleculeGraph
	if err := json.Unmarshal(out, &graph); err != nil {
		return nil, fmt.Errorf("decoding formula molecule %s: %w", rootID, err)
	}
	rootCount := 0
	for _, issue := range graph.Issues {
		if issue.ID == rootID {
			rootCount++
		}
	}
	if rootCount != 1 {
		return nil, fmt.Errorf("formula molecule %s contains %d exact root rows", rootID, rootCount)
	}
	issueIDs := make([]string, 0, len(graph.Issues))
	for _, issue := range graph.Issues {
		if issue.ID == "" {
			return nil, fmt.Errorf("formula molecule %s contains an empty issue ID", rootID)
		}
		issueIDs = append(issueIDs, issue.ID)
	}
	edges := make(map[string]formulaMoleculeDependency)
	for _, direction := range []string{"down", "up"} {
		args := append([]string{"dep", "list"}, issueIDs...)
		args = append(args, "--direction="+direction, "--json")
		dependencyJSON, err := BdCmd(args...).
			Dir(workDir).
			WithBeadsDir(beadsDir).
			WithGTRoot(townRoot).
			OutputContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading %s formula dependencies for %s: %w", direction, rootID, err)
		}
		var dependencies []formulaMoleculeDependency
		if err := json.Unmarshal(dependencyJSON, &dependencies); err != nil {
			return nil, fmt.Errorf("decoding %s formula dependencies for %s: %w", direction, rootID, err)
		}
		for _, dependency := range dependencies {
			if dependency.IssueID == "" && dependency.DependsOnID == "" && dependency.Type == "" {
				continue
			}
			if dependency.IssueID == "" || dependency.DependsOnID == "" || dependency.Type == "" {
				return nil, fmt.Errorf("formula molecule %s has an incomplete dependency record", rootID)
			}
			key := dependency.IssueID + "\x00" + dependency.DependsOnID + "\x00" + dependency.Type
			if existing, ok := edges[key]; ok && existing.CreatedBy != dependency.CreatedBy {
				return nil, fmt.Errorf("formula molecule %s has conflicting dependency provenance", rootID)
			}
			edges[key] = dependency
		}
	}
	graph.Dependencies = graph.Dependencies[:0]
	for _, dependency := range edges {
		graph.Dependencies = append(graph.Dependencies, dependency)
	}
	return &graph, nil
}

func captureFormulaMoleculeGraphContext(ctx context.Context, workDir, townRoot, rootID string) (*formulaMoleculeGraph, error) {
	beadsDir := beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), rootID)
	snapshot, err := beads.NewWithBeadsDir(workDir, beadsDir).CaptureFormulaMoleculeGraphContext(ctx, rootID)
	if err != nil {
		return nil, fmt.Errorf("capturing formula molecule %s: %w", rootID, err)
	}
	graph := &formulaMoleculeGraph{
		Issues:       make([]formulaMoleculeIssue, 0, len(snapshot.Issues)),
		Dependencies: make([]formulaMoleculeDependency, 0, len(snapshot.Dependencies)),
	}
	for _, issue := range snapshot.Issues {
		graph.Issues = append(graph.Issues, formulaMoleculeIssue{
			ID: issue.ID, Title: issue.Title, Description: issue.Description, IssueType: issue.IssueType,
			CreatedAt: issue.CreatedAt, Ephemeral: issue.Ephemeral,
		})
	}
	for _, dependency := range snapshot.Dependencies {
		graph.Dependencies = append(graph.Dependencies, formulaMoleculeDependency{
			IssueID: dependency.IssueID, DependsOnID: dependency.DependsOnID,
			Type: dependency.Type, CreatedBy: dependency.CreatedBy,
		})
	}
	return graph, nil
}

func verifyFormulaMoleculeIdentity(formulaName, rootID string, vars []string, workDir, townRoot string) error {
	return verifyFormulaMoleculeIdentityContext(context.Background(), formulaName, rootID, vars, "", "", workDir, townRoot)
}

func verifyFormulaMoleculeIdentityContext(ctx context.Context, formulaName, rootID string, vars []string, owner, actor, workDir, townRoot string) error {
	_, err := verifyFormulaMoleculeIdentityAndGenerationContext(ctx, formulaName, rootID, vars, owner, actor, workDir, townRoot)
	return err
}

func verifyFormulaMoleculeIdentityAndGenerationContext(ctx context.Context, formulaName, rootID string, vars []string, owner, actor, workDir, townRoot string) (string, error) {
	content, err := formula.ResolveFormulaContent(formulaName, townRoot, formulaMutationRigName(townRoot, workDir))
	if err != nil {
		return "", fmt.Errorf("resolving formula identity: %w", err)
	}
	resolved, err := formula.Parse(content)
	if err != nil {
		return "", fmt.Errorf("parsing formula identity: %w", err)
	}
	if len(resolved.Extends) > 0 || resolved.Compose != nil {
		searchPaths := []string{filepath.Join(townRoot, ".beads", "formulas")}
		if rigName := formulaMutationRigName(townRoot, workDir); rigName != "" {
			searchPaths = append([]string{filepath.Join(townRoot, rigName, ".beads", "formulas")}, searchPaths...)
		}
		resolved, err = formula.Resolve(resolved, searchPaths)
		if err != nil {
			return "", fmt.Errorf("resolving composed formula identity: %w", err)
		}
	}
	graph, err := captureFormulaMoleculeGraphContext(ctx, workDir, townRoot, rootID)
	if err != nil {
		return "", err
	}
	if err := verifyFormulaMoleculeGraphIdentity(graph, resolved, rootID, vars, owner, actor); err != nil {
		return "", err
	}
	return formulaMutationGraphGeneration(graph)
}

var verifyFormulaMoleculeIdentityAndGenerationFn = verifyFormulaMoleculeIdentityAndGenerationContext

func verifyFormulaMoleculeGraphIdentity(graph *formulaMoleculeGraph, resolved *formula.Formula, rootID string, vars []string, owner, actor string) error {
	if graph == nil || resolved == nil {
		return fmt.Errorf("formula molecule identity proof is incomplete")
	}
	issues := make(map[string]formulaMoleculeIssue, len(graph.Issues))
	for _, issue := range graph.Issues {
		if _, exists := issues[issue.ID]; exists {
			return fmt.Errorf("formula molecule contains duplicate issue %s", issue.ID)
		}
		issues[issue.ID] = issue
	}
	root, ok := issues[rootID]
	if !ok || root.IssueType != "molecule" || !root.Ephemeral || root.Title != resolved.Name || root.Description != resolved.Description {
		return fmt.Errorf("formula molecule root %s does not match formula %s", rootID, resolved.Name)
	}
	if len(issues) != len(resolved.Steps)+1 {
		return fmt.Errorf("formula molecule %s contains %d issues, want exact envelope of %d", rootID, len(issues), len(resolved.Steps)+1)
	}

	childIDs := make(map[string]bool)
	for _, dependency := range graph.Dependencies {
		if dependency.Type == "parent-child" && dependency.DependsOnID == rootID {
			childIDs[dependency.IssueID] = true
		}
	}
	if len(childIDs) != len(resolved.Steps) {
		return fmt.Errorf("formula molecule %s has %d materialized steps, want %d", rootID, len(childIDs), len(resolved.Steps))
	}

	values := make(map[string]interface{}, len(resolved.Vars)+len(vars))
	for name, variable := range resolved.Vars {
		if variable.Default != "" {
			values[name] = variable.Default
		}
	}
	for _, assignment := range vars {
		if name, value, found := strings.Cut(assignment, "="); found && name != "" {
			values[name] = value
		}
	}
	expectedVariables := make(map[string]string, len(values))
	for name, value := range values {
		expectedVariables[name] = fmt.Sprint(value)
	}
	// Current bd releases omit Variables from mol show. When a release exposes
	// them, require an exact match; until then the operation actor below binds
	// the full request fingerprint durably at creation time.
	if graph.Variables != nil {
		if len(graph.Variables) != len(expectedVariables) {
			return fmt.Errorf("formula molecule %s variable set differs from formula request", rootID)
		}
		for name, want := range expectedVariables {
			if got, ok := graph.Variables[name]; !ok || got != want {
				return fmt.Errorf("formula molecule %s variable %s differs from formula request", rootID, name)
			}
		}
	}
	type stepIdentity struct {
		id, title, description string
		needs                  []string
	}
	stepsByIdentity := make(map[string]stepIdentity, len(resolved.Steps))
	for _, step := range resolved.Steps {
		identity := stepIdentity{
			id: step.ID, title: substituteFormulaVars(step.Title, values),
			description: substituteFormulaVars(step.Description, values), needs: step.Needs,
		}
		key := identity.title + "\x00" + identity.description
		if _, duplicate := stepsByIdentity[key]; duplicate {
			return fmt.Errorf("formula %s has duplicate rendered step identity", resolved.Name)
		}
		stepsByIdentity[key] = identity
	}
	stepIssueIDs := make(map[string]string, len(childIDs))
	for childID := range childIDs {
		issue, ok := issues[childID]
		if !ok || issue.IssueType != "task" || !issue.Ephemeral {
			return fmt.Errorf("formula molecule step %s lacks exact ephemeral task identity", childID)
		}
		identity, ok := stepsByIdentity[issue.Title+"\x00"+issue.Description]
		if !ok {
			return fmt.Errorf("formula molecule step %s is not present in formula %s", childID, resolved.Name)
		}
		if _, duplicate := stepIssueIDs[identity.id]; duplicate {
			return fmt.Errorf("formula molecule duplicates step %s", identity.id)
		}
		stepIssueIDs[identity.id] = childID
	}

	expectedDependencies := make(map[string]int)
	for childID := range childIDs {
		expectedDependencies[childID+"\x00"+rootID+"\x00parent-child"]++
	}
	for _, identity := range stepsByIdentity {
		for _, need := range identity.needs {
			from, fromOK := stepIssueIDs[identity.id]
			to, toOK := stepIssueIDs[need]
			if !fromOK || !toOK {
				return fmt.Errorf("formula molecule cannot bind dependency %s -> %s", identity.id, need)
			}
			expectedDependencies[from+"\x00"+to+"\x00blocks"]++
		}
	}
	actualDependencies := make(map[string]int)
	for _, dependency := range graph.Dependencies {
		if actor != "" && dependency.CreatedBy != actor {
			return fmt.Errorf("formula molecule dependency lacks exact operation authority")
		}
		key := dependency.IssueID + "\x00" + dependency.DependsOnID + "\x00" + dependency.Type
		if owner != "" && dependency.IssueID == rootID && dependency.DependsOnID == owner &&
			(dependency.Type == "blocks" || dependency.Type == "conditional-blocks" || dependency.Type == "parent-child") {
			if actualDependencies[key] > 0 {
				return fmt.Errorf("formula molecule %s duplicates owner dependency", rootID)
			}
			expectedDependencies[key] = 1
		}
		actualDependencies[key]++
	}
	if len(actualDependencies) != len(expectedDependencies) {
		return fmt.Errorf("formula molecule %s dependency graph differs from formula %s", rootID, resolved.Name)
	}
	for edge, want := range expectedDependencies {
		if actualDependencies[edge] != want {
			return fmt.Errorf("formula molecule %s is missing a formula dependency", rootID)
		}
	}
	return nil
}

var formulaMutationFormulaGenerationFn = formulaMutationFormulaGeneration

func formulaMutationScope(scope string) string {
	resolved := beads.ResolveBeadsDir(scope)
	if resolved == "" {
		resolved = filepath.Clean(scope)
	}
	digest := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(digest[:])
}

func formulaMutationAttemptPath(townRoot string, key formulaMutationAttemptKey) string {
	digest := sha256.Sum256([]byte(key.Kind + "\x00" + key.Formula + "\x00" + key.Owner))
	return filepath.Join(townRoot, ".runtime", "sling-formula-attempts", hex.EncodeToString(digest[:])+".json")
}

func acquireFormulaMutationLock(ctx context.Context, townRoot, scope string) (func(), error) {
	dir := filepath.Join(townRoot, ".runtime", "sling-formula-attempts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// ponytail: one lock per routed Beads DB; split only when bd exposes an
	// operation-addressable creation receipt that makes inventory deltas obsolete.
	fl := flock.New(filepath.Join(dir, "scope-"+scope+".flock"))
	locked, err := fl.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, ctx.Err()
	}
	return func() { _ = fl.Unlock() }, nil
}

func rejectOtherPendingFormulaMutation(townRoot string, key formulaMutationAttemptKey, scope string) error {
	dir := filepath.Join(townRoot, ".runtime", "sling-formula-attempts")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	currentPath := formulaMutationAttemptPath(townRoot, key)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if path == currentPath {
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // bounded internal receipt directory
		if err != nil {
			return err
		}
		var other formulaMutationAttempt
		if err := json.Unmarshal(data, &other); err != nil {
			return fmt.Errorf("decoding formula mutation attempt %s: %w", entry.Name(), err)
		}
		if other.Scope == scope && other.RootID == "" {
			return fmt.Errorf("formula mutation %s/%s for %s must be reconciled before another mutation in this Beads database", other.Key.Kind, other.Key.Formula, other.Key.Owner)
		}
	}
	return nil
}

func writeFormulaMutationAttempt(townRoot string, attempt *formulaMutationAttempt) error {
	if attempt == nil {
		return fmt.Errorf("formula mutation attempt lacks exact identity")
	}
	attempt.Version = formulaMutationAttemptVersion
	attempt.legacy = false
	parsedNonce, nonceErr := uuid.Parse(attempt.OperationNonce)
	if attempt.Key.Kind == "" || attempt.Key.Formula == "" || attempt.Key.Owner == "" || attempt.Scope == "" || attempt.RequestFingerprint == "" || nonceErr != nil || parsedNonce.String() != attempt.OperationNonce {
		return fmt.Errorf("formula mutation attempt lacks exact identity")
	}
	return writeFormulaMutationAttemptFile(townRoot, attempt)
}

func writeLegacyFormulaMutationAttempt(townRoot string, attempt *formulaMutationAttempt) error {
	if attempt == nil || attempt.OperationNonce != "" || attempt.Key.Kind == "" || attempt.Key.Formula == "" || attempt.Key.Owner == "" || attempt.Scope == "" || attempt.RequestFingerprint == "" || attempt.RootID == "" || attempt.RootGeneration == "" {
		return fmt.Errorf("legacy formula mutation attempt lacks exact reconciled identity")
	}
	attempt.Version = formulaMutationLegacyAttemptVersion
	attempt.legacy = true
	return writeFormulaMutationAttemptFile(townRoot, attempt)
}

func writeFormulaMutationAttemptFile(townRoot string, attempt *formulaMutationAttempt) error {
	path := formulaMutationAttemptPath(townRoot, attempt.Key)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("formula mutation attempt is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicfile.EnsureDirAndWriteJSONWithPerm(path, attempt, 0o600)
}

func loadFormulaMutationAttempt(townRoot string, key formulaMutationAttemptKey) (*formulaMutationAttempt, error) {
	path := formulaMutationAttemptPath(townRoot, key)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("formula mutation attempt is not a regular file")
	}
	data, err := os.ReadFile(path) //nolint:gosec // deterministic internal receipt path
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("decoding formula mutation attempt: %w", err)
	}
	allowed := map[string]bool{
		"version": true, "key": true, "scope": true, "request_fingerprint": true,
		"operation_nonce": true, "before_ids": true, "root_id": true, "root_generation": true,
		"authorization_commit": true,
	}
	for field := range fields {
		if !allowed[field] {
			return nil, fmt.Errorf("formula mutation attempt has unsupported field %q", field)
		}
	}
	var attempt formulaMutationAttempt
	if err := json.Unmarshal(data, &attempt); err != nil {
		return nil, fmt.Errorf("decoding formula mutation attempt: %w", err)
	}
	if attempt.Key != key {
		return nil, fmt.Errorf("formula mutation attempt identity does not match its path")
	}
	if attempt.Key.Kind == "" || attempt.Key.Formula == "" || attempt.Key.Owner == "" || attempt.Scope == "" || attempt.RequestFingerprint == "" || !validFormulaMutationBeforeIDs(attempt.BeforeIDs) || (attempt.RootID == "") != (attempt.RootGeneration == "") {
		return nil, fmt.Errorf("formula mutation attempt lacks exact identity")
	}
	for _, id := range attempt.BeforeIDs {
		if id == attempt.RootID {
			return nil, fmt.Errorf("formula mutation root was already present in its baseline")
		}
	}
	switch attempt.Version {
	case 0, formulaMutationLegacyAttemptVersion:
		if attempt.OperationNonce == "" {
			attempt.Version = formulaMutationLegacyAttemptVersion
			attempt.legacy = true
			break
		}
		if attempt.Version != 0 {
			return nil, fmt.Errorf("legacy formula mutation attempt has unexpected operation identity")
		}
		attempt.Version = formulaMutationAttemptVersion
		fallthrough
	case formulaMutationAttemptVersion:
		parsedNonce, nonceErr := uuid.Parse(attempt.OperationNonce)
		if nonceErr != nil || parsedNonce.String() != attempt.OperationNonce {
			return nil, fmt.Errorf("formula mutation attempt lacks operation identity")
		}
	default:
		return nil, fmt.Errorf("formula mutation attempt has unsupported version %d", attempt.Version)
	}
	return &attempt, nil
}

func validFormulaMutationBeforeIDs(ids []string) bool {
	for i, id := range ids {
		if id == "" || (i > 0 && ids[i-1] >= id) {
			return false
		}
	}
	return true
}

func formulaMutationActor(townRoot string, key formulaMutationAttemptKey) (string, error) {
	attempt, err := loadFormulaMutationAttempt(townRoot, key)
	if err != nil {
		return "", err
	}
	if attempt == nil || attempt.Key != key || attempt.RequestFingerprint == "" {
		return "", fmt.Errorf("formula mutation attempt lacks durable operation/owner identity")
	}
	if attempt.legacy {
		return "", nil
	}
	if attempt.OperationNonce == "" {
		return "", fmt.Errorf("formula mutation attempt lacks durable operation/owner identity")
	}
	ownerHash := sha256.Sum256([]byte(key.Owner))
	return fmt.Sprintf("gt-sling-v1:%s:%x:%s", attempt.OperationNonce, ownerHash[:16], attempt.RequestFingerprint), nil
}

func clearFormulaMutationAttempt(townRoot string, key formulaMutationAttemptKey) error {
	path := formulaMutationAttemptPath(townRoot, key)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("formula mutation attempt is not a regular file")
	}
	return os.Remove(path)
}

func executeFormulaMutationAttempt(
	ctx context.Context,
	townRoot string,
	scopePath string,
	key formulaMutationAttemptKey,
	requestFingerprint string,
	list func() (map[string]bool, error),
	mutate func() ([]byte, error),
	parse func([]byte) (string, bool),
	proof func(string) (string, error),
) (string, error) {
	if list == nil || mutate == nil || proof == nil || requestFingerprint == "" {
		return "", fmt.Errorf("formula mutation attempt is missing inventory, mutation, request, identity, or generation proof")
	}
	scope := formulaMutationScope(scopePath)
	release, err := acquireFormulaMutationLock(ctx, townRoot, scope)
	if err != nil {
		return "", fmt.Errorf("serializing formula mutation: %w", err)
	}
	defer release()

	attempt, err := loadFormulaMutationAttempt(townRoot, key)
	if err != nil {
		return "", err
	}
	if attempt != nil && (attempt.Scope != scope || attempt.RequestFingerprint != requestFingerprint) {
		if attempt.RootID == "" {
			return "", fmt.Errorf("pending formula mutation request differs from the current request")
		}
		current, listErr := list()
		if listErr != nil {
			return "", fmt.Errorf("checking superseded formula mutation root: %w", listErr)
		}
		if current[attempt.RootID] {
			return "", fmt.Errorf("pending formula mutation request differs from the current request")
		}
		if err := clearFormulaMutationAttempt(townRoot, key); err != nil {
			return "", fmt.Errorf("clearing superseded formula mutation: %w", err)
		}
		attempt = nil
	}
	if attempt != nil && attempt.legacy {
		current, listErr := list()
		if listErr != nil {
			return "", fmt.Errorf("reconciling legacy formula mutation inventory: %w", listErr)
		}
		before := make(map[string]bool, len(attempt.BeforeIDs))
		for _, id := range attempt.BeforeIDs {
			before[id] = true
		}
		root, found, reconcileErr := formulaMutationDelta(before, current)
		if reconcileErr != nil {
			return "", reconcileErr
		}
		if attempt.RootID != "" && current[attempt.RootID] {
			if !found || root != attempt.RootID {
				return "", fmt.Errorf("legacy formula mutation root %s is not the exact inventory delta", attempt.RootID)
			}
			return persistLegacyReconciledFormulaMutation(townRoot, attempt, root, proof)
		}
		if attempt.RootID == "" && found {
			return "", fmt.Errorf("rootless legacy formula mutation has no durable provenance for inventory delta %s", root)
		}
		if attempt.RootID == "" {
			attempt.Version = formulaMutationAttemptVersion
			attempt.OperationNonce = uuid.NewString()
			attempt.legacy = false
			if err := writeFormulaMutationAttempt(townRoot, attempt); err != nil {
				return "", fmt.Errorf("migrating legacy formula mutation intent: %w", err)
			}
		}
	}
	newAttempt := attempt == nil
	if newAttempt {
		if err := rejectOtherPendingFormulaMutation(townRoot, key, scope); err != nil {
			return "", err
		}
		before, err := list()
		if err != nil {
			return "", fmt.Errorf("capturing formula mutation inventory: %w", err)
		}
		attempt = &formulaMutationAttempt{
			Key: key, Scope: scope, RequestFingerprint: requestFingerprint,
			OperationNonce: uuid.NewString(),
			BeforeIDs:      sortedFormulaMutationIDs(before),
		}
		if err := writeFormulaMutationAttempt(townRoot, attempt); err != nil {
			return "", fmt.Errorf("journaling formula mutation intent: %w", err)
		}
	}

	if !newAttempt {
		current, err := list()
		if err != nil {
			return "", fmt.Errorf("reconciling formula mutation inventory: %w", err)
		}
		if attempt.RootID != "" && current[attempt.RootID] {
			generation, err := proof(attempt.RootID)
			if err != nil {
				return "", fmt.Errorf("reading formula mutation root generation: %w", err)
			}
			if attempt.RootGeneration == "" || generation != attempt.RootGeneration {
				return "", fmt.Errorf("formula mutation root %s generation changed", attempt.RootID)
			}
			return attempt.RootID, nil
		}
		if attempt.RootID != "" {
			if err := clearFormulaMutationAttempt(townRoot, key); err != nil {
				return "", fmt.Errorf("clearing vanished formula mutation: %w", err)
			}
			if err := rejectOtherPendingFormulaMutation(townRoot, key, scope); err != nil {
				return "", err
			}
			before, err := list()
			if err != nil {
				return "", fmt.Errorf("capturing replacement formula mutation inventory: %w", err)
			}
			attempt = &formulaMutationAttempt{
				Key: key, Scope: scope, RequestFingerprint: requestFingerprint,
				OperationNonce: uuid.NewString(),
				BeforeIDs:      sortedFormulaMutationIDs(before),
			}
			if err := writeFormulaMutationAttempt(townRoot, attempt); err != nil {
				return "", fmt.Errorf("journaling replacement formula mutation intent: %w", err)
			}
		}
		before := make(map[string]bool, len(attempt.BeforeIDs))
		for _, id := range attempt.BeforeIDs {
			before[id] = true
		}
		if root, found, err := formulaMutationDelta(before, current); err != nil {
			return "", err
		} else if found {
			return persistReconciledFormulaMutation(townRoot, attempt, root, proof)
		}
	}

	before := make(map[string]bool, len(attempt.BeforeIDs))
	for _, id := range attempt.BeforeIDs {
		before[id] = true
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, mutateErr := mutate()
	after, listErr := list()
	if listErr != nil {
		if mutateErr != nil {
			return "", fmt.Errorf("formula mutation failed (%v) and inventory reconciliation failed: %w", mutateErr, listErr)
		}
		return "", fmt.Errorf("reconciling formula mutation inventory: %w", listErr)
	}
	root, found, reconcileErr := formulaMutationDelta(before, after)
	if reconcileErr != nil {
		return "", reconcileErr
	}
	if !found {
		if mutateErr != nil {
			return "", mutateErr
		}
		parsed := ""
		if parse != nil {
			parsed, _ = parse(out)
		}
		return "", fmt.Errorf("formula mutation produced no new inventory root (reported %q)", parsed)
	}
	// Inventory is authoritative after an auto-committed mutation. Parsed output
	// is only an acknowledgement and may be missing, malformed, or conflicting.
	return persistReconciledFormulaMutation(townRoot, attempt, root, proof)
}

func formulaMutationDelta(before, after map[string]bool) (string, bool, error) {
	root, err := reconcileCreatedFormulaWisp(before, after)
	if err == nil {
		return root, true, nil
	}
	if errors.Is(err, errFormulaWispNoNewRoot) {
		return "", false, nil
	}
	return "", false, err
}

func persistReconciledFormulaMutation(
	townRoot string,
	attempt *formulaMutationAttempt,
	root string,
	proof func(string) (string, error),
) (string, error) {
	generation, err := proof(root)
	if err != nil {
		return "", fmt.Errorf("capturing formula mutation root generation: %w", err)
	}
	if generation == "" {
		return "", fmt.Errorf("formula mutation root %s has no generation", root)
	}
	attempt.RootID = root
	attempt.RootGeneration = generation
	if err := writeFormulaMutationAttempt(townRoot, attempt); err != nil {
		return "", fmt.Errorf("journaling reconciled formula root: %w", err)
	}
	return root, nil
}

func persistLegacyReconciledFormulaMutation(
	townRoot string,
	attempt *formulaMutationAttempt,
	root string,
	proof func(string) (string, error),
) (string, error) {
	generation, err := proof(root)
	if err != nil {
		return "", fmt.Errorf("capturing legacy formula mutation root generation: %w", err)
	}
	if generation == "" || (attempt.RootGeneration != "" && attempt.RootGeneration != generation) {
		return "", fmt.Errorf("legacy formula mutation root %s generation changed", root)
	}
	attempt.RootID = root
	attempt.RootGeneration = generation
	if err := writeLegacyFormulaMutationAttempt(townRoot, attempt); err != nil {
		return "", fmt.Errorf("journaling reconciled legacy formula root: %w", err)
	}
	return root, nil
}

var prepareFormulaMoleculeAuthorizationFn = func(townRoot, workDir, rootID string, authorization beads.FormulaMoleculeAuthorization) (string, error) {
	beadsDir := beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), rootID)
	return beads.NewWithBeadsDir(workDir, beadsDir).PrepareFormulaMoleculeAuthorization(authorization)
}

func formulaMutationPublication(townRoot, workDir string, key formulaMutationAttemptKey, rootID, requestFingerprint, actor string) (beads.FormulaMoleculeAuthorization, string, error) {
	attempt, err := loadFormulaMutationAttempt(townRoot, key)
	if err != nil {
		return beads.FormulaMoleculeAuthorization{}, "", err
	}
	if attempt == nil || attempt.legacy || attempt.OperationNonce == "" || attempt.RootID != rootID || attempt.RootGeneration == "" || attempt.RequestFingerprint != requestFingerprint {
		return beads.FormulaMoleculeAuthorization{}, "", fmt.Errorf("formula mutation %s has no exact publication receipt", rootID)
	}
	authorization := beads.FormulaMoleculeAuthorization{
		PublicationID:      attempt.OperationNonce,
		RootID:             rootID,
		RootGeneration:     attempt.RootGeneration,
		Formula:            key.Formula,
		Owner:              key.Owner,
		RequestFingerprint: requestFingerprint,
		Actor:              actor,
	}
	if !slingReceiptDatabaseConfigured(townRoot, rootID) {
		return authorization, "", nil
	}
	commit, err := prepareFormulaMoleculeAuthorizationFn(townRoot, workDir, rootID, authorization)
	if err != nil {
		return beads.FormulaMoleculeAuthorization{}, "", err
	}
	if attempt.AuthorizationCommit != "" && attempt.AuthorizationCommit != commit {
		return beads.FormulaMoleculeAuthorization{}, "", fmt.Errorf("formula mutation %s authorization commit changed", rootID)
	}
	if attempt.AuthorizationCommit == "" {
		attempt.AuthorizationCommit = commit
		if err := writeFormulaMutationAttempt(townRoot, attempt); err != nil {
			return beads.FormulaMoleculeAuthorization{}, "", fmt.Errorf("journaling formula publication authorization: %w", err)
		}
	}
	return authorization, commit, nil
}

func formulaMutationAlreadyPublished(townRoot, workDir string, key formulaMutationAttemptKey, rootID, assignee string) (bool, error) {
	attempt, err := loadFormulaMutationAttempt(townRoot, key)
	if err != nil || attempt == nil || attempt.OperationNonce == "" || attempt.RootID != rootID {
		return false, err
	}
	beadsDir := beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), rootID)
	publication, err := beads.NewWithBeadsDir(workDir, beadsDir).FormulaMoleculePublicationByID(attempt.OperationNonce)
	if err != nil || publication == nil {
		return false, err
	}
	return publication.RootID == rootID && publication.WorkID == rootID &&
		publication.NewAssignee == assignee && publication.Authorization.Formula == key.Formula &&
		publication.Authorization.Owner == key.Owner, nil
}

func sortedFormulaMutationIDs(ids map[string]bool) []string {
	result := make([]string, 0, len(ids))
	for id := range ids {
		if id != "" {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}
