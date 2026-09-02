// Package doltserver manages the Dolt SQL server for Gas Town.
//
// The Dolt server provides multi-client access to beads databases,
// avoiding the single-writer limitation of embedded Dolt mode.
//
// Server configuration:
//   - Port: 3307 (avoids conflict with MySQL on 3306)
//   - User: root (default Dolt user, no password for localhost)
//   - Data directory: ~/gt/.dolt-data/ (contains all rig databases)
//
// Each rig (hq, gastown, beads) has its own database subdirectory:
//
//	~/gt/.dolt-data/
//	├── hq/        # Town beads (hq-*)
//	├── gastown/   # Gastown rig (gt-*)
//	├── beads/     # Beads rig (bd-*)
//	└── ...        # Other rigs
//
// Usage:
//
//	gt dolt start           # Start the server
//	gt dolt stop            # Stop the server
//	gt dolt status          # Check server status
//	gt dolt logs            # View server logs
//	gt dolt sql             # Open SQL shell
//	gt dolt init-rig <name> # Initialize a new rig database
package doltserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gofrs/flock"
	beadssdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	configpkg "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/doltlock"
	"github.com/steveyegge/gastown/internal/style"
)

// EnsureDoltIdentity configures dolt global identity (user.name, user.email)
// if not already set. Copies values from git config as a sensible default.
// This must run before InitRig and Start, since dolt init requires identity.
func EnsureDoltIdentity() error {
	// Check each field independently to avoid creating duplicates with --add.
	// Distinguish "key not found" (exit code 1, empty output) from dolt crashes.
	needName, err := doltConfigMissing("user.name")
	if err != nil {
		return fmt.Errorf("probing dolt user.name: %w", err)
	}
	needEmail, err := doltConfigMissing("user.email")
	if err != nil {
		return fmt.Errorf("probing dolt user.email: %w", err)
	}

	if !needName && !needEmail {
		return nil // already configured
	}

	// Copy missing fields from git global config.
	// We read --global only (not repo-local) to avoid silently persisting
	// a repo-scoped override into dolt's permanent global config.
	if needName {
		nameCmd := exec.Command("git", "config", "--global", "user.name")
		setProcessGroup(nameCmd)
		gitName, err := nameCmd.Output()
		if err != nil || len(bytes.TrimSpace(gitName)) == 0 {
			return fmt.Errorf("dolt identity not configured and git user.name not available; run: dolt config --global --add user.name \"Your Name\"")
		}
		if err := setDoltGlobalConfig("user.name", strings.TrimSpace(string(gitName))); err != nil {
			return fmt.Errorf("failed to set dolt user.name: %w", err)
		}
	}

	if needEmail {
		emailCmd := exec.Command("git", "config", "--global", "user.email")
		setProcessGroup(emailCmd)
		gitEmail, err := emailCmd.Output()
		if err != nil || len(bytes.TrimSpace(gitEmail)) == 0 {
			return fmt.Errorf("dolt identity not configured and git user.email not available; run: dolt config --global --add user.email \"you@example.com\"")
		}
		if err := setDoltGlobalConfig("user.email", strings.TrimSpace(string(gitEmail))); err != nil {
			return fmt.Errorf("failed to set dolt user.email: %w", err)
		}
	}

	return nil
}

// doltConfigMissing checks whether a dolt global config key is unset.
// Returns (true, nil) for missing keys, (false, nil) for present keys,
// and (false, error) when dolt itself fails unexpectedly.
func doltConfigMissing(key string) (bool, error) {
	cmd := exec.Command("dolt", "config", "--global", "--get", key)
	setProcessGroup(cmd)
	out, err := cmd.Output()
	if err == nil {
		// Command succeeded — key exists if output is non-empty
		return len(bytes.TrimSpace(out)) == 0, nil
	}
	// dolt config --get exits 1 for missing keys with no stderr.
	// Any other failure (crash, permission error) is unexpected.
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return true, nil // key not found — expected
	}
	return false, fmt.Errorf("dolt config --global --get %s: %w", key, err)
}

// setDoltGlobalConfig idempotently sets a dolt global config key.
// Uses --unset then --add to avoid duplicate entries from repeated calls.
func setDoltGlobalConfig(key, value string) error {
	// Remove existing value (ignore error — key may not exist yet)
	unsetCmd := exec.Command("dolt", "config", "--global", "--unset", key)
	setProcessGroup(unsetCmd)
	_ = unsetCmd.Run()
	addCmd := exec.Command("dolt", "config", "--global", "--add", key, value)
	setProcessGroup(addCmd)
	return addCmd.Run()
}

// Default configuration
const (
	DefaultPort           = 3307
	DefaultUser           = "root" // Default Dolt user (no password for local access)
	DefaultMaxConnections = 1000   // Dolt default; no reason to limit below (Tim Sehn confirmed 1k is fine)

	defaultDoltSQLServerGoMemLimit = "16GiB"
	defaultDoltSQLServerGOGC       = "50"

	// DefaultReadTimeoutMs is the server-side timeout for reading a complete request from a client.
	// Controls how long Dolt waits for a client to send a query on an idle connection.
	// Prevents CLOSE_WAIT accumulation from abandoned connections: when a client times out
	// and closes its end, Dolt will detect the dead connection within this window.
	// 5 minutes matches the compactor GC timeout (compactorGCTimeout) so GC ops complete
	// before the connection is considered stale.
	DefaultReadTimeoutMs = 5 * 60 * 1000 // 5 minutes in milliseconds

	// DefaultWriteTimeoutMs is the server-side timeout for writing a response back to a client.
	// When a client closes its TCP connection while a query is running (e.g. compactor GC),
	// Dolt detects the dead connection within this timeout rather than holding CLOSE_WAIT
	// for Dolt's default 8 hours. Set to match compactor GC timeout.
	DefaultWriteTimeoutMs = 5 * 60 * 1000 // 5 minutes in milliseconds

	// DefaultWaitTimeoutSec is how long Dolt keeps an idle session alive before
	// closing it. Dolt's MySQL-compat default is 28800s (8 hours). Under Gas
	// Town load (mayor + deacon + witness + refinery + N polecats + dashboard
	// polling), short-lived `bd` processes leak connections faster than the
	// default timeout reclaims them, leading to a death spiral at the 1000-
	// connection cap. 30s is aggressive but matches the documented workaround
	// in gh-3623 and is far longer than any healthy bd query takes.
	// Override with GT_DOLT_WAIT_TIMEOUT.
	DefaultWaitTimeoutSec = 30

	// DefaultTimeZone is the MySQL `time_zone` server variable, set after
	// startup. UTC keeps server-side `NOW()`/`CURRENT_TIMESTAMP` consistent
	// with Go-side `time.Now().UTC()` writes. Without this, columns populated
	// by SQL (e.g. `closed_at = NOW()` in reaper.go) and columns populated
	// by Go drivers disagree by the host TZ offset, breaking age comparisons
	// in ad-hoc queries. The reaper itself binds Go-side UTC values so its
	// math is unaffected, but humans triaging via `dolt sql` get misled.
	// See hq-57jr8.
	// Override with GT_DOLT_TIME_ZONE; set to empty to skip the override.
	DefaultTimeZone = "+00:00"
)

// metadataMu provides per-path mutexes for EnsureMetadata goroutine synchronization.
// flock is inter-process only and cannot reliably synchronize goroutines within the
// same process (the same process may acquire the same flock twice without blocking).
var metadataMu sync.Map // map[string]*sync.Mutex

// doltLifecycleMu complements daemon/dolt.lock for same-process callers.
// flock is inter-process and is not a reliable goroutine mutex on every OS.
var doltLifecycleMu sync.Mutex

type doltServerIncarnation struct {
	PID               int
	ProcessStartToken string
}

// getMetadataMu returns a mutex for the given metadata file path, creating one if needed.
func getMetadataMu(path string) *sync.Mutex {
	mu, _ := metadataMu.LoadOrStore(path, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// Config holds Dolt server configuration.
type Config struct {
	// TownRoot is the Gas Town workspace root.
	TownRoot string

	// Host is the Dolt server hostname or IP.
	// Empty means localhost (backward-compatible default).
	Host string

	// Port is the MySQL protocol port.
	Port int

	// User is the MySQL user name.
	User string

	// Password is the MySQL password.
	// Empty means no password (backward-compatible default for local access).
	Password string

	// DataDir is the root directory containing all rig databases.
	// Each subdirectory is a separate database that will be served.
	DataDir string

	// LogFile is the path to the server log file.
	LogFile string

	// PidFile is the path to the PID file.
	PidFile string

	// MaxConnections is the maximum number of simultaneous connections the server will accept.
	// Set to 0 to use the Dolt default (1000). Gas Town defaults to 50 to prevent
	// connection storms during mass polecat slings.
	MaxConnections int

	// ReadTimeoutMs is the server-side read timeout in milliseconds.
	// Controls how long Dolt waits for a client to send a request on an idle connection.
	// Prevents abandoned connections from staying in CLOSE_WAIT indefinitely.
	// Set to 0 to use Dolt's default (28800000 = 8 hours — strongly discouraged).
	ReadTimeoutMs int

	// WriteTimeoutMs is the server-side write timeout in milliseconds.
	// Controls how long Dolt waits to write response data back to a client.
	// When a client closes its TCP connection while a query is running, Dolt
	// detects the dead connection within WriteTimeoutMs instead of holding it
	// open for up to 8 hours (Dolt default).
	// Must be >= the longest expected query (e.g., compactor GC at 5 minutes).
	// Set to 0 to use Dolt's default (28800000 = 8 hours — strongly discouraged).
	WriteTimeoutMs int

	// WaitTimeoutSec is the idle-session timeout (MySQL `wait_timeout` server
	// variable) in seconds. Set after the server is reachable via
	// `SET GLOBAL wait_timeout = N`. See gh-3623.
	// Set to 0 to skip the override and let Dolt use its 8-hour default.
	WaitTimeoutSec int

	// TimeZone is the MySQL `time_zone` server variable, set via
	// `SET GLOBAL time_zone = '...'` after the server is reachable. Empty
	// string skips the override and lets Dolt inherit the host TZ.
	// See hq-57jr8 and DefaultTimeZone.
	TimeZone string

	// LogLevel is the Dolt server log level (trace, debug, info, warning, error, fatal).
	// Default is "warning" to suppress connection open/close noise. Override with
	// GT_DOLT_LOGLEVEL=info (or debug) for diagnostics.
	LogLevel string

	// EventScheduler controls Dolt's MySQL event scheduler in managed config.
	// Default is OFF for Gas Town: background SQL events are not part of normal
	// beads operation and add hidden work during outage recovery.
	EventScheduler string

	// DoltStatsEnabled controls the dolt_stats_enabled system variable in managed
	// config. Default is "0" to avoid background stats workers during high-churn
	// agent workloads. Set to "omit" to leave the variable out of config.yaml.
	DoltStatsEnabled string

	// AutoGC controls Dolt's non-blocking storage GC (auto_gc_behavior) in managed
	// config. Default "on" keeps the sql-server's RSS bounded (hq-excy9g). Set to
	// "off" (or false/0/disabled), typically via GT_DOLT_AUTO_GC, to disable it
	// without a source revert+rebuild — a runtime escape hatch.
	AutoGC string
}

// DefaultConfig returns the default Dolt server configuration.
//
// Port priority is resolved by config.ResolveDoltPort so client and server
// callsites share one precedence model: explicit env, durable config, daemon
// env, then DefaultPort.
//
// Other environment variables:
//   - GT_DOLT_HOST → Host
//   - GT_DOLT_USER → User
//   - GT_DOLT_PASSWORD → Password
//   - GT_DOLT_LOGLEVEL → LogLevel (trace, debug, info, warning, error, fatal)
func DefaultConfig(townRoot string) *Config {
	daemonDir := filepath.Join(townRoot, "daemon")
	config := &Config{
		TownRoot:         townRoot,
		Port:             DefaultPort,
		User:             DefaultUser,
		DataDir:          filepath.Join(townRoot, ".dolt-data"),
		LogFile:          filepath.Join(daemonDir, "dolt.log"),
		PidFile:          filepath.Join(daemonDir, "dolt.pid"),
		MaxConnections:   DefaultMaxConnections,
		ReadTimeoutMs:    DefaultReadTimeoutMs,
		WriteTimeoutMs:   DefaultWriteTimeoutMs,
		WaitTimeoutSec:   DefaultWaitTimeoutSec,
		TimeZone:         DefaultTimeZone,
		LogLevel:         "warning",
		EventScheduler:   "OFF",
		DoltStatsEnabled: "0",
		AutoGC:           "on",
	}

	// Optional override for the idle-session timeout. Negative values disable
	// the override entirely (use Dolt's 8-hour default).
	if v := os.Getenv("GT_DOLT_WAIT_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 0 {
				config.WaitTimeoutSec = 0
			} else {
				config.WaitTimeoutSec = n
			}
		}
	}

	// Optional override for the server timezone. Empty value disables the
	// post-start `SET GLOBAL time_zone` and lets Dolt inherit the host TZ.
	if v, ok := os.LookupEnv("GT_DOLT_TIME_ZONE"); ok {
		config.TimeZone = v
	}

	if h := configpkg.ResolveDoltHost(townRoot); h != "" {
		config.Host = h
	}

	if port := configpkg.ResolveDoltPort(townRoot); port > 0 {
		config.Port = port
	}
	if scheduler, ok := os.LookupEnv("GT_DOLT_EVENT_SCHEDULER"); ok {
		config.EventScheduler = scheduler
	}
	if stats, ok := os.LookupEnv("GT_DOLT_STATS_ENABLED"); ok {
		config.DoltStatsEnabled = stats
	}
	if autoGc, ok := os.LookupEnv("GT_DOLT_AUTO_GC"); ok {
		config.AutoGC = autoGc
	}

	if u := os.Getenv("GT_DOLT_USER"); u != "" {
		config.User = u
	}
	if pw := os.Getenv("GT_DOLT_PASSWORD"); pw != "" {
		config.Password = pw
	}
	if ll := os.Getenv("GT_DOLT_LOGLEVEL"); ll != "" {
		config.LogLevel = ll
	} else if townRoot != "" {
		// Fallback: read GT_DOLT_LOGLEVEL from daemon/daemon.env so the log
		// level survives daemon-triggered Dolt restarts (gt-zb8). The daemon
		// process may not have GT_DOLT_LOGLEVEL in its own environment when it
		// was started before the manual env var was applied.
		if ll := readDaemonEnvVar(filepath.Join(townRoot, "daemon", "daemon.env"), "GT_DOLT_LOGLEVEL"); ll != "" {
			config.LogLevel = ll
		}
	}

	// Default to warning logging. Use GT_DOLT_LOGLEVEL=info or =debug for diagnostics.
	if config.LogLevel == "" {
		config.LogLevel = "warning"
	}

	return config
}

// readDaemonEnvVar reads a single key=value variable from a simple env file.
// Handles blank lines and # comments; returns "" if not found or on error.
func readDaemonEnvVar(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

// IsRemote returns true when the config points to a non-local Dolt server.
// Empty host, "127.0.0.1", "localhost", "::1", and "[::1]" are all considered local.
// Hostnames that resolve to a loopback address are also treated as local.
func (c *Config) IsRemote() bool {
	switch strings.ToLower(c.Host) {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return false
	}
	// Resolve hostname and check if it points to loopback.
	addrs, err := net.LookupHost(c.Host)
	if err != nil {
		return true
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && ip.IsLoopback() {
			return false
		}
	}
	return true
}

// SQLArgs returns the dolt CLI flags needed to connect to a remote server.
// Returns nil for local servers (dolt auto-detects the running local server).
func (c *Config) SQLArgs() []string {
	if !c.IsRemote() {
		return nil
	}
	return []string{
		"--host", c.Host,
		"--port", strconv.Itoa(c.Port),
		"--user", c.User,
		"--no-tls",
	}
}

// userDSN returns the user[:password] portion of a MySQL DSN.
func (c *Config) userDSN() string {
	if c.Password != "" {
		return c.User + ":" + c.Password
	}
	return c.User
}

// EffectiveHost returns the configured host, defaulting to "127.0.0.1" when empty.
func (c *Config) EffectiveHost() string {
	if c.Host == "" {
		return "127.0.0.1"
	}
	return c.Host
}

// HostPort returns "host:port", defaulting host to "127.0.0.1" when empty.
func (c *Config) HostPort() string {
	host := c.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, c.Port)
}

// buildDoltSQLCmd constructs a non-interactive dolt sql command that always
// talks to the running SQL server over TCP.
//
// For local servers, this avoids embedded-mode auto-discovery, which can load
// databases relative to cmd.Dir instead of querying the live shared server.
func buildDoltSQLCmd(ctx context.Context, config *Config, args ...string) *exec.Cmd {
	fullArgs := make([]string, 0, 8+len(args))
	fullArgs = append(fullArgs,
		"--host", config.EffectiveHost(),
		"--port", strconv.Itoa(config.Port),
		"--user", config.User,
		"--no-tls",
		"sql",
	)
	fullArgs = append(fullArgs, args...)

	cmd := exec.CommandContext(ctx, "dolt", fullArgs...)

	// GH#2537: Always set cmd.Dir to prevent dolt from creating stray
	// .doltcfg/privileges.db files in the caller's CWD. Even TCP client
	// connections can trigger .doltcfg creation if CWD is uncontrolled.
	cmd.Dir = config.DataDir
	setProcessGroup(cmd)

	// Always set DOLT_CLI_PASSWORD to suppress interactive prompts.
	// When empty, dolt connects without a password, which is the local default.
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD="+config.Password)

	return cmd
}

// NewSQLServerCommand constructs the managed dolt sql-server process command.
func NewSQLServerCommand(doltPath, dataDir, configPath string) *exec.Cmd {
	cmd := exec.Command(doltPath, "sql-server", "--config", configPath)
	cmd.Dir = dataDir
	cmd.Env = doltSQLServerEnv(cmd.Environ())
	return cmd
}

func doltSQLServerEnv(env []string) []string {
	return doltSQLServerEnvForGOOS(env, runtime.GOOS)
}

func doltSQLServerEnvForGOOS(env []string, goos string) []string {
	out := append([]string(nil), env...)
	out = appendEnvDefault(out, "GOMEMLIMIT", defaultDoltSQLServerGoMemLimit, goos)
	out = appendEnvDefault(out, "GOGC", defaultDoltSQLServerGOGC, goos)
	return out
}

func appendEnvDefault(env []string, key, value, goos string) []string {
	for _, entry := range env {
		entryKey, _, ok := strings.Cut(entry, "=")
		if ok && envKeyMatches(entryKey, key, goos) {
			return env
		}
	}
	return append(env, key+"="+value)
}

func envKeyMatches(entryKey, key, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(entryKey, key)
	}
	return entryKey == key
}

// RigDatabaseDir returns the database directory for a specific rig.
func RigDatabaseDir(townRoot, rigName string) string {
	config := DefaultConfig(townRoot)
	return filepath.Join(config.DataDir, rigName)
}

// DatabasePath returns a validated direct child of the shared Dolt data dir.
func DatabasePath(townRoot, name string) (string, error) {
	if !validSQLName(name) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid database name %q: must match [a-zA-Z0-9_.-]+ and name one direct child", name)
	}
	dataDir := filepath.Clean(DefaultConfig(townRoot).DataDir)
	path := filepath.Join(dataDir, name)
	if filepath.Dir(path) != dataDir || filepath.Base(path) != name {
		return "", fmt.Errorf("invalid database name %q: resolved path is not a direct child", name)
	}
	return path, nil
}

// State represents the Dolt server's runtime state.
type State struct {
	// Running indicates if the server is running.
	Running bool `json:"running"`

	// PID is the process ID of the server.
	PID int `json:"pid"`

	// Port is the port the server is listening on.
	Port int `json:"port"`

	// StartedAt is when the server started.
	StartedAt time.Time `json:"started_at"`

	// DataDir is the data directory containing all rig databases.
	DataDir string `json:"data_dir"`

	// Databases is the list of available databases (rig names).
	Databases []string `json:"databases,omitempty"`
}

// SQLServerInfo is Dolt's own runtime metadata from .dolt/sql-server.info.
type SQLServerInfo struct {
	PID      int
	Port     int
	ServerID string
	Path     string
}

// StateFile returns the path to the state file.
func StateFile(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "dolt-state.json")
}

func sqlServerInfoPath(config *Config) string {
	return filepath.Join(config.DataDir, ".dolt", "sql-server.info")
}

// SQLServerInfoPath returns the Dolt-managed sql-server.info path for a town.
func SQLServerInfoPath(townRoot string) string {
	return sqlServerInfoPath(DefaultConfig(townRoot))
}

// ReadSQLServerInfo reads Dolt's own sql-server.info metadata for a town.
func ReadSQLServerInfo(townRoot string) (*SQLServerInfo, error) {
	return readSQLServerInfo(DefaultConfig(townRoot))
}

func readSQLServerInfo(config *Config) (*SQLServerInfo, error) {
	path := sqlServerInfoPath(config)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed sql-server.info at %s", path)
	}
	pid, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, fmt.Errorf("malformed sql-server.info PID at %s: %w", path, err)
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("malformed sql-server.info port at %s: %w", path, err)
	}
	info := &SQLServerInfo{
		PID:  pid,
		Port: port,
		Path: path,
	}
	if len(parts) > 2 {
		info.ServerID = parts[2]
	}
	return info, nil
}

// LoadState loads Dolt server state from disk.
func LoadState(townRoot string) (*State, error) {
	stateFile := StateFile(townRoot)
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// SaveState saves Dolt server state to disk using atomic write.
func SaveState(townRoot string, state *State) error {
	stateFile := StateFile(townRoot)

	// Ensure daemon directory exists
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		return err
	}

	return atomicfile.WriteJSON(stateFile, state)
}

func refreshPIDStateFromLiveInfo(townRoot string, config *Config, pid int) (bool, error) {
	if pid <= 0 || config == nil || config.IsRemote() {
		return false, nil
	}

	changed := false
	if data, err := os.ReadFile(config.PidFile); err != nil || strings.TrimSpace(string(data)) != strconv.Itoa(pid) {
		if err := os.MkdirAll(filepath.Dir(config.PidFile), 0755); err != nil {
			return changed, err
		}
		if err := atomicfile.WriteFile(config.PidFile, []byte(strconv.Itoa(pid)+"\n"), 0644); err != nil {
			return changed, err
		}
		changed = true
	}

	state, err := LoadState(townRoot)
	if err != nil {
		return changed, err
	}
	if state == nil {
		state = &State{}
	}
	if state.PID != pid || !state.Running || state.Port != config.Port || state.DataDir != config.DataDir {
		state.PID = pid
		state.Running = true
		state.Port = config.Port
		state.DataDir = config.DataDir
		if state.StartedAt.IsZero() {
			state.StartedAt = time.Now()
		}
		if err := SaveState(townRoot, state); err != nil {
			return changed, err
		}
		changed = true
	}

	return changed, nil
}

// countDoltDatabases counts the number of Dolt database directories in dataDir.
// Each subdirectory containing a .dolt directory is considered a database.
// Returns at least 1 so the caller never divides by zero.
func countDoltDatabases(dataDir string) int {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return 1
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// A Dolt database directory contains a .dolt subdirectory.
		if _, statErr := os.Stat(filepath.Join(dataDir, e.Name(), ".dolt")); statErr == nil {
			count++
		}
	}
	if count < 1 {
		return 1
	}
	return count
}

// IsRunning checks if a Dolt server is running for the given town.

// Returns (running, pid, error).
// Checks both PID file AND port to detect externally-started servers.
// For remote servers, skips PID/port scan and just does TCP reachability.
func IsRunning(townRoot string) (bool, int, error) {
	config := DefaultConfig(townRoot)

	// Remote server: no local PID/process to check — just TCP reachability.
	if config.IsRemote() {
		conn, err := net.DialTimeout("tcp", config.HostPort(), 2*time.Second)
		if err != nil {
			return false, 0, nil
		}
		_ = conn.Close()
		return true, 0, nil
	}

	// Prefer Dolt's own runtime metadata when present. During daemon restarts,
	// daemon/dolt-state.json and daemon/dolt.pid can lag behind the live
	// sql-server process, but .dolt/sql-server.info is written by Dolt itself.
	if info, err := readSQLServerInfo(config); err == nil && info.Port == config.Port && info.PID > 0 {
		if processIsAlive(info.PID) && isDoltServerOnPort(config.Port) && doltProcessMatchesTown(townRoot, info.PID, config) {
			_, _ = refreshPIDStateFromLiveInfo(townRoot, config, info.PID)
			return true, info.PID, nil
		}
	}

	// First check PID file
	data, err := os.ReadFile(config.PidFile)
	if err == nil {
		pidStr := strings.TrimSpace(string(data))
		pid, err := strconv.Atoi(pidStr)
		if err == nil {
			// Check if process is alive
			if processIsAlive(pid) {
				// Verify it's actually serving on the expected port.
				// More reliable than ps string matching (ZFC fix: gt-utuk).
				if isDoltServerOnPort(config.Port) {
					if doltProcessMatchesTown(townRoot, pid, config) {
						_, _ = refreshPIDStateFromLiveInfo(townRoot, config, pid)
						return true, pid, nil
					}
					// Port served by a different town's Dolt — fall through to stale cleanup
				}
			}
		}
		// PID file is stale, clean it up
		_ = os.Remove(config.PidFile)
	}

	// No valid PID file - check if port is in use by dolt anyway.
	// This catches externally-started dolt servers.
	pid := findDoltServerOnPort(config.Port)
	if pid > 0 && doltProcessMatchesTown(townRoot, pid, config) {
		_, _ = refreshPIDStateFromLiveInfo(townRoot, config, pid)
		return true, pid, nil
	}

	// Last resort: TCP reachability check. This handles Docker containers,
	// externally-restarted servers (e.g., dolt restarted outside of gt),
	// and other setups where no local dolt process is visible via lsof/ss
	// (e.g., the port is forwarded by a Docker proxy).
	// We always check, even on the default port 3307, so that gt rig add
	// succeeds when dolt is live regardless of how it was started.
	conn, err := net.DialTimeout("tcp", config.HostPort(), 2*time.Second)
	if err == nil {
		_ = conn.Close()
		return true, 0, nil
	}

	return false, 0, nil
}

// CheckServerReachable verifies the Dolt server is actually accepting TCP connections.
// This catches the case where a process exists but the server hasn't finished starting,
// or the PID file is stale and the port is not actually listening.
// Returns nil if reachable, error describing the problem otherwise.
func CheckServerReachable(townRoot string) error {
	config := DefaultConfig(townRoot)
	addr := config.HostPort()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		hint := ""
		if !config.IsRemote() {
			hint = "\n\nStart with: gt dolt start"
		}
		return fmt.Errorf("Dolt server not reachable at %s: %w%s", addr, err, hint)
	}
	_ = conn.Close()
	return nil
}

// WaitForReady polls for the Dolt server to become reachable (TCP connection
// succeeds) within the given timeout. Returns nil if the server is reachable
// or if no server-mode metadata is configured (nothing to wait for).
// Returns an error if the timeout expires before the server is reachable.
//
// This is used by gt up to ensure the Dolt server is ready before starting
// agents (witnesses, refineries) that depend on beads database access.
// Without this, agents race the Dolt server startup and get "connection refused".
func WaitForReady(townRoot string, timeout time.Duration) error {
	// Check if any rig is configured for server mode.
	// If not, there's no Dolt server to wait for.
	if len(HasServerModeMetadata(townRoot)) == 0 {
		return nil
	}

	config := DefaultConfig(townRoot)
	addr := config.HostPort()
	deadline := time.Now().Add(timeout)
	interval := 100 * time.Millisecond

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		dialTimeout := 1 * time.Second
		if remaining < dialTimeout {
			dialTimeout = remaining
		}
		conn, err := net.DialTimeout("tcp", addr, dialTimeout)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if interval > remaining {
			interval = remaining
		}
		time.Sleep(interval)
		// Exponential backoff capped at 500ms
		if interval < 500*time.Millisecond {
			interval = interval * 2
			if interval > 500*time.Millisecond {
				interval = 500 * time.Millisecond
			}
		}
	}

	return fmt.Errorf("Dolt server not ready at %s after %v", addr, timeout)
}

// HasServerModeMetadata checks whether any rig has metadata.json configured for
// Dolt server mode. Returns the list of rig names configured for server mode.
// This is used to detect the split-brain risk: if metadata says "server" but
// the server isn't running, bd commands may silently create isolated databases.
func HasServerModeMetadata(townRoot string) []string {
	var serverRigs []string

	// Check town-level beads (hq)
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if hasServerMode(townBeadsDir) {
		serverRigs = append(serverRigs, "hq")
	}

	// Check rig-level beads
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	data, err := os.ReadFile(rigsPath)
	if err != nil {
		return serverRigs
	}
	var config struct {
		Rigs map[string]interface{} `json:"rigs"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return serverRigs
	}

	for rigName := range config.Rigs {
		beadsDir := FindRigBeadsDir(townRoot, rigName)
		if beadsDir != "" && hasServerMode(beadsDir) {
			serverRigs = append(serverRigs, rigName)
		}
	}

	return serverRigs
}

// hasServerMode reads metadata.json and returns true if dolt_mode is "server".
func hasServerMode(beadsDir string) bool {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return false
	}
	var metadata struct {
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return false
	}
	return metadata.DoltMode == "server"
}

// CheckPortConflict checks if the configured port is occupied by another town's Dolt.
// Returns (conflicting PID, conflicting data-dir) if a foreign Dolt holds the port,
// or (0, "") if the port is free or used by this town's own Dolt.
func CheckPortConflict(townRoot string) (int, string) {
	cfg := DefaultConfig(townRoot)
	if cfg.IsRemote() {
		return 0, ""
	}
	pid := findDoltServerOnPort(cfg.Port)
	if pid <= 0 {
		return 0, ""
	}
	if doltProcessMatchesTown(townRoot, pid, cfg) {
		return 0, ""
	}
	return pid, doltProcessOwnerPath(townRoot, pid)
}

// findDoltServerOnPort finds a process listening on the given port.
// Returns the PID or 0 if not found.
// Does not verify process identity via ps string matching (ZFC fix: gt-utuk).
//
// Tries lsof first (macOS and most Linux), then ss (iproute2) as a fallback
// for Linux systems where lsof is not installed.
func findDoltServerOnPort(port int) int {
	// Try lsof — preferred when available (cross-platform).
	// Without -sTCP:LISTEN, lsof returns client PIDs (e.g., gt daemon) first,
	// which aren't dolt processes — causing false negatives.
	cmd := exec.Command("lsof", "-i", fmt.Sprintf(":%d", port), "-sTCP:LISTEN", "-t")
	setProcessGroup(cmd)
	if output, err := cmd.Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if len(lines) > 0 && lines[0] != "" {
			if pid, err := strconv.Atoi(lines[0]); err == nil {
				return pid
			}
		}
	}

	// Fall back to ss (iproute2) — standard on modern Linux, no extra packages needed.
	// Example output line: LISTEN 0 128 *:3307 *:* users:(("dolt",pid=12345,fd=7))
	cmd = exec.Command("ss", "-tlnp", fmt.Sprintf("sport = :%d", port))
	setProcessGroup(cmd)
	if output, err := cmd.Output(); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			if idx := strings.Index(line, "pid="); idx >= 0 {
				rest := line[idx+4:]
				if end := strings.IndexAny(rest, ",)"); end > 0 {
					if pid, err := strconv.Atoi(rest[:end]); err == nil && pid > 0 {
						return pid
					}
				}
			}
		}
	}

	return 0
}

// DoltListener represents a Dolt process listening on a TCP port.
type DoltListener struct {
	PID  int
	Port int
}

// DoltServerClass describes why a local Dolt listener is or is not safe to act on.
type DoltServerClass string

const (
	DoltServerCanonical              DoltServerClass = "canonical"
	DoltServerConfiguredPortImposter DoltServerClass = "configured-port-imposter"
	DoltServerOwnedTownLeak          DoltServerClass = "owned-town-leak"
	DoltServerOwnedTestLeak          DoltServerClass = "owned-test-leak"
	DoltServerUnknown                DoltServerClass = "unknown"
)

// LocalDoltServer is one entry in the shared local listener inventory.
type LocalDoltServer struct {
	DoltListener
	Class        DoltServerClass
	OwnerPath    string
	ProcessToken string
}

// TestLeakSelection is the private, path-free identity recorded by a preview.
type TestLeakSelection struct {
	PID            int             `json:"pid"`
	Port           int             `json:"port"`
	Class          DoltServerClass `json:"class"`
	OwnershipToken string          `json:"ownership_token"`
}

// TestProcessCustody is an opaque identity for a launcher-owned child before
// it has opened a listener.
type TestProcessCustody struct {
	PID            int
	ParentPID      int
	OwnershipToken string
}

func newTestLeakSelection(server LocalDoltServer) TestLeakSelection {
	if server.Class != DoltServerOwnedTestLeak || server.OwnerPath == "" || server.ProcessToken == "" {
		return TestLeakSelection{}
	}
	token := sha256.Sum256([]byte(filepath.Clean(server.OwnerPath) + "\x00" + server.ProcessToken))
	return TestLeakSelection{
		PID:            server.PID,
		Port:           server.Port,
		Class:          server.Class,
		OwnershipToken: fmt.Sprintf("%x", token),
	}
}

// TestLeakSelections returns path-free identities for positively test-owned listeners.
func TestLeakSelections(inventory []LocalDoltServer) []TestLeakSelection {
	var selections []TestLeakSelection
	for _, server := range inventory {
		if selection := newTestLeakSelection(server); selection.OwnershipToken != "" {
			selections = append(selections, selection)
		}
	}
	return selections
}

// Actionable reports whether Gas Town has enough evidence to remediate this listener.
func (s LocalDoltServer) Actionable() bool {
	return s.Class == DoltServerConfiguredPortImposter || s.Class == DoltServerOwnedTownLeak || s.Class == DoltServerOwnedTestLeak
}

type doltProcessEvidence struct {
	DataDir      string
	ConfigPath   string
	CWD          string
	CWDFromLsof  bool
	StateDir     string
	ProcessToken string
}

func processEvidence(townRoot string, pid int) doltProcessEvidence {
	cwd := getProcessCWD(pid)
	return doltProcessEvidence{
		DataDir:      GetDoltDataDirFromProcess(pid),
		ConfigPath:   getDoltConfigPathFromProcess(pid),
		CWD:          cwd,
		CWDFromLsof:  runtime.GOOS == "darwin" && cwd != "",
		StateDir:     getServerDataDir(townRoot, pid),
		ProcessToken: getProcessStartToken(pid),
	}
}

func (e doltProcessEvidence) ownerPath() string {
	return doltProcessOwnerPathFromEvidence(e.DataDir, e.ConfigPath, e.CWD, e.StateDir)
}

func isOwnedTestDataDir(path string) bool {
	if path == "" {
		return false
	}
	roots := []string{filepath.Clean(os.TempDir())}
	if resolved, err := filepath.EvalSymlinks(roots[0]); err == nil && resolved != roots[0] {
		roots = append(roots, resolved)
	}
	for _, tempRoot := range roots {
		rel, err := filepath.Rel(tempRoot, filepath.Clean(path))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		root := strings.Split(rel, string(filepath.Separator))[0]
		if strings.HasPrefix(root, "gastown-test-dolt.") && len(root) > len("gastown-test-dolt.") {
			return true
		}
	}
	return false
}

func isOwnedTestCWD(path string, fromLsof bool) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	for _, component := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == "." || component == ".." {
			return false
		}
	}

	tempRoot := filepath.Clean(os.TempDir())
	cleanPath := filepath.Clean(path)
	if isOwnedTestDataDir(cleanPath) {
		resolvedPath, err := filepath.EvalSymlinks(cleanPath)
		if err != nil {
			return fromLsof && os.IsNotExist(err)
		}
		return isOwnedTestDataDir(resolvedPath)
	}
	resolvedTempRoot, err := filepath.EvalSymlinks(tempRoot)
	if err != nil {
		return false
	}
	containedRel := func(root, candidate string) (string, bool) {
		rel, err := filepath.Rel(root, candidate)
		return rel, err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	lexicalRel, ok := containedRel(tempRoot, cleanPath)
	if !ok {
		lexicalRel, ok = containedRel(resolvedTempRoot, cleanPath)
	}
	if !ok {
		return false
	}
	components := strings.Split(lexicalRel, string(filepath.Separator))
	if len(components) < 5 || !strings.HasPrefix(components[0], ".ctx-mode-") || len(components[0]) == len(".ctx-mode-") ||
		!isGoTestTempComponent(components[1]) || !allDecimalDigits(components[2]) ||
		components[len(components)-2] != ".beads" || components[len(components)-1] != "dolt" {
		return false
	}
	resolvedPath, err := filepath.EvalSymlinks(cleanPath)
	if err != nil {
		// On macOS this path comes only from lsof's live, kernel-resolved cwd
		// record. A deleted test temp directory cannot be resolved again, but its
		// strict reported shape remains positive ownership evidence.
		return fromLsof && os.IsNotExist(err)
	}
	resolvedRel, ok := containedRel(resolvedTempRoot, resolvedPath)
	if !ok || resolvedRel != lexicalRel {
		return false
	}
	return true
}

func isGoTestTempComponent(component string) bool {
	if !strings.HasPrefix(component, "Test") {
		return false
	}
	nameAndDigits := component[len("Test"):]
	digitStart := len(nameAndDigits)
	for digitStart > 0 && nameAndDigits[digitStart-1] >= '0' && nameAndDigits[digitStart-1] <= '9' {
		digitStart--
	}
	if digitStart == 0 || digitStart == len(nameAndDigits) {
		return false
	}
	if first := nameAndDigits[0]; !((first >= 'A' && first <= 'Z') || first == '_') {
		return false
	}
	for i, ch := range nameAndDigits[:digitStart] {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' || (i > 0 && ch >= '0' && ch <= '9')) {
			return false
		}
	}
	return true
}

func allDecimalDigits(component string) bool {
	if component == "" {
		return false
	}
	for _, ch := range component {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func pathWithin(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isOwnedTownLeak(expectedDataDir string, evidence doltProcessEvidence) bool {
	if expectedDataDir == "" {
		return false
	}
	townRoot := filepath.Dir(filepath.Clean(expectedDataDir))
	return pathWithin(townRoot, evidence.DataDir) || pathWithin(townRoot, evidence.ConfigPath) || pathWithin(townRoot, evidence.StateDir)
}

func classifyLocalDoltServers(expectedPort int, expectedDataDir string, listeners []DoltListener, evidenceFor func(int) doltProcessEvidence) []LocalDoltServer {
	evidenceByPID := make(map[int]doltProcessEvidence, len(listeners))
	canonicalPIDs := make(map[int]bool)
	for _, listener := range listeners {
		evidence, ok := evidenceByPID[listener.PID]
		if !ok {
			evidence = evidenceFor(listener.PID)
			evidenceByPID[listener.PID] = evidence
		}
		if listener.Port == expectedPort && doltProcessMatchesTownPaths(expectedDataDir, evidence.DataDir, evidence.ConfigPath, evidence.CWD, evidence.StateDir) {
			canonicalPIDs[listener.PID] = true
		}
	}
	servers := make([]LocalDoltServer, 0, len(listeners))
	for _, listener := range listeners {
		evidence := evidenceByPID[listener.PID]
		server := LocalDoltServer{DoltListener: listener, Class: DoltServerUnknown, OwnerPath: evidence.ownerPath(), ProcessToken: evidence.ProcessToken}
		switch {
		case canonicalPIDs[listener.PID]:
			server.Class = DoltServerCanonical
		case listener.Port == expectedPort:
			server.Class = DoltServerConfiguredPortImposter
		case isOwnedTestDataDir(evidence.DataDir):
			server.Class = DoltServerOwnedTestLeak
			server.OwnerPath = evidence.DataDir
		case isOwnedTestCWD(evidence.CWD, evidence.CWDFromLsof):
			server.Class = DoltServerOwnedTestLeak
			server.OwnerPath = evidence.CWD
		case isOwnedTownLeak(expectedDataDir, evidence):
			server.Class = DoltServerOwnedTownLeak
		}
		servers = append(servers, server)
	}
	return servers
}

// InventoryLocalDoltServers discovers and classifies all local Dolt listeners once.
func inventoryLocalDoltServers(townRoot string) ([]LocalDoltServer, error) {
	config := DefaultConfig(townRoot)
	listeners, err := FindAllDoltListenersWithError()
	if err != nil {
		return nil, err
	}
	if config.IsRemote() {
		return classifyLocalDoltServers(0, "", listeners, func(pid int) doltProcessEvidence {
			return processEvidence(townRoot, pid)
		}), nil
	}
	return classifyLocalDoltServers(config.Port, config.DataDir, listeners, func(pid int) doltProcessEvidence {
		return processEvidence(townRoot, pid)
	}), nil
}

// InventoryLocalDoltServers discovers and classifies all local Dolt listeners once.
func InventoryLocalDoltServers(townRoot string) []LocalDoltServer {
	servers, _ := inventoryLocalDoltServers(townRoot)
	return servers
}

// InventoryLocalDoltServersWithError preserves listener-discovery failures for
// callers that must fail closed before signaling a process.
func InventoryLocalDoltServersWithError(townRoot string) ([]LocalDoltServer, error) {
	return inventoryLocalDoltServers(townRoot)
}

// FindAllDoltListeners discovers all Dolt processes with TCP listeners using lsof.
// Uses process binary name matching (-c dolt) instead of command-line string matching
// (pgrep -f), avoiding fragile ps/pgrep pattern coupling (ZFC fix: gt-fj87).
// The -a flag is critical: without it, lsof ORs -c and -i selections, matching ANY
// process with TCP listeners (not just dolt). With -a, selections are ANDed (fix: gt-lzdp).
func FindAllDoltListenersWithError() ([]DoltListener, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "lsof", "-a", "-c", "dolt", "-sTCP:LISTEN", "-i", "TCP", "-n", "-P", "-F", "pn")
	setProcessGroup(cmd)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(output) == 0 && len(exitErr.Stderr) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("discover Dolt listeners: %w", err)
	}

	// Parse lsof -F output. Lines are field-prefixed:
	//   p<PID>     — process ID
	//   n<addr>    — network name (e.g., "*:3307" or "127.0.0.1:3307")
	var listeners []DoltListener
	var currentPID int
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, err := strconv.Atoi(line[1:])
			if err == nil {
				currentPID = pid
			}
		case 'n':
			if currentPID == 0 {
				continue
			}
			// Extract port from address like "*:3307" or "127.0.0.1:3307"
			addr := line[1:]
			if idx := strings.LastIndex(addr, ":"); idx >= 0 {
				port, err := strconv.Atoi(addr[idx+1:])
				if err == nil {
					// Deduplicate: same PID can have multiple FDs on same port
					dup := false
					for _, l := range listeners {
						if l.PID == currentPID && l.Port == port {
							dup = true
							break
						}
					}
					if !dup {
						listeners = append(listeners, DoltListener{PID: currentPID, Port: port})
					}
				}
			}
		}
	}
	return listeners, nil
}

func FindAllDoltListeners() []DoltListener {
	listeners, _ := FindAllDoltListenersWithError()
	return listeners
}

// isDoltServerOnPort checks if a dolt server is accepting connections on the given port.
// More reliable than ps string matching for process identity verification (ZFC fix: gt-utuk).
func isDoltServerOnPort(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// getServerDataDir returns the data directory for the Dolt server associated with townRoot.
// Reads from the persisted state file instead of parsing ps command output
// (ZFC fix: gt-utuk — eliminates fragile ps string matching).
// Returns empty string if the state file is missing or the PID doesn't match.
func getServerDataDir(townRoot string, pid int) string {
	state, err := LoadState(townRoot)
	if err != nil {
		return ""
	}
	// Only trust the state if the PID matches or we don't know the PID
	if state.PID == pid || pid == 0 {
		return state.DataDir
	}
	// PID mismatch — state is stale or belongs to a different server
	return ""
}

func getDoltFlagFromArgs(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
		prefix := flag + "="
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return ""
}

func getProcessArgs(pid int) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=")
	setProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Fields(strings.TrimSpace(string(out)))
}

func getProcessStartToken(pid int) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	setProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func getProcessParentPID(pid int) int {
	if runtime.GOOS == "windows" {
		return 0
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=")
	setProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	parent, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return parent
}

func processIsZombie(pid int) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "state=")
	setProcessGroup(cmd)
	out, err := cmd.Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func currentTestProcessCustody(pid, parentPID int) TestProcessCustody {
	start := getProcessStartToken(pid)
	if pid <= 0 || parentPID <= 0 || start == "" || getProcessParentPID(pid) != parentPID || processIsZombie(pid) {
		return TestProcessCustody{}
	}
	token := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s", pid, parentPID, start)))
	return TestProcessCustody{PID: pid, ParentPID: parentPID, OwnershipToken: fmt.Sprintf("%x", token)}
}

// CaptureTestProcessCustody records a stable identity for a direct child.
func CaptureTestProcessCustody(pid, parentPID int) (TestProcessCustody, error) {
	custody := currentTestProcessCustody(pid, parentPID)
	if custody.OwnershipToken == "" {
		return TestProcessCustody{}, fmt.Errorf("PID %d is not a live direct child of PID %d", pid, parentPID)
	}
	return custody, nil
}

func getProcessCWD(pid int) string {
	switch runtime.GOOS {
	case "linux":
		cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
		if err == nil {
			return cwd
		}
	case "darwin":
		cmd := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
		setProcessGroup(cmd)
		out, err := cmd.Output()
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasPrefix(line, "n") {
					return strings.TrimPrefix(line, "n")
				}
			}
		}
	}
	return ""
}

func resolveProcessPath(pid int, path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if cwd := getProcessCWD(pid); cwd != "" {
		return filepath.Clean(filepath.Join(cwd, path))
	}
	return filepath.Clean(path)
}

// GetDoltDataDirFromProcess reads the --data-dir flag value from the running
// process's command-line arguments. This is structural (reading a well-defined
// CLI flag), not heuristic string matching. Used as a tiebreaker when the
// state-file based check is inconclusive (e.g. PID reuse across towns).
//
// Supported on macOS and Linux via POSIX ps. Returns empty string on Windows
// (not supported) or on any error.
func GetDoltDataDirFromProcess(pid int) string {
	return resolveProcessPath(pid, getDoltFlagFromArgs(getProcessArgs(pid), "--data-dir"))
}

// getDoltConfigPathFromProcess reads the --config flag value from the running
// process's command-line arguments. Gas Town starts Dolt via --config, so this
// is the primary ownership signal when --data-dir is absent.
func getDoltConfigPathFromProcess(pid int) string {
	return resolveProcessPath(pid, getDoltFlagFromArgs(getProcessArgs(pid), "--config"))
}

func doltProcessMatchesTownPaths(expectedDataDir, actualDataDir, actualConfigPath, actualCWD, stateDataDir string) bool {
	expectedDir, _ := filepath.Abs(expectedDataDir)
	if actualDataDir != "" {
		actualDir, _ := filepath.Abs(actualDataDir)
		return actualDir == expectedDir
	}
	if actualConfigPath != "" {
		expectedConfig, _ := filepath.Abs(filepath.Join(expectedDir, "config.yaml"))
		actualConfig, _ := filepath.Abs(actualConfigPath)
		return actualConfig == expectedConfig
	}
	if actualCWD != "" {
		absCWD, _ := filepath.Abs(actualCWD)
		return absCWD == expectedDir || absCWD == filepath.Dir(expectedDir)
	}
	if stateDataDir != "" {
		actualDir, _ := filepath.Abs(stateDataDir)
		return actualDir == expectedDir
	}
	return false
}

func doltProcessMatchesTown(townRoot string, pid int, config *Config) bool {
	return doltProcessMatchesTownPaths(
		config.DataDir,
		GetDoltDataDirFromProcess(pid),
		getDoltConfigPathFromProcess(pid),
		getProcessCWD(pid),
		getServerDataDir(townRoot, pid),
	)
}

func doltProcessOwnerPathFromEvidence(actualDataDir, actualConfigPath, actualCWD, stateDataDir string) string {
	switch {
	case actualDataDir != "":
		return actualDataDir
	case actualConfigPath != "":
		return actualConfigPath
	case actualCWD != "":
		return actualCWD
	default:
		return stateDataDir
	}
}

func doltProcessOwnerPath(townRoot string, pid int) string {
	return doltProcessOwnerPathFromEvidence(
		GetDoltDataDirFromProcess(pid),
		getDoltConfigPathFromProcess(pid),
		getProcessCWD(pid),
		getServerDataDir(townRoot, pid),
	)
}

// VerifyServerDataDir checks whether the running Dolt server is serving the
// expected databases from the correct data directory. Returns true if the server
// is legitimate (serving databases from config.DataDir), false if it's an imposter
// (e.g., started from a different data directory with different/empty databases).
func VerifyServerDataDir(townRoot string) (bool, error) {
	config := DefaultConfig(townRoot)

	// First check: inspect the state file for data-dir (ZFC fix: gt-utuk).
	running, pid, err := IsRunning(townRoot)
	if err != nil || !running {
		return false, fmt.Errorf("server not running")
	}

	ownerPath := doltProcessOwnerPath(townRoot, pid)
	if ownerPath != "" {
		expectedDir, _ := filepath.Abs(config.DataDir)
		if !doltProcessMatchesTown(townRoot, pid, config) {
			return false, fmt.Errorf("server ownership mismatch: expected %s, got %s (PID %d)", expectedDir, ownerPath, pid)
		}
		return true, nil
	}

	// No state file or PID mismatch — check served databases
	fsDatabases, fsErr := ListDatabases(townRoot)
	if fsErr != nil || len(fsDatabases) == 0 {
		// Can't verify if no databases expected
		return true, nil
	}

	served, _, verifyErr := VerifyDatabases(townRoot)
	if verifyErr != nil {
		return false, fmt.Errorf("could not query server databases: %w", verifyErr)
	}

	// If the server is serving none of our expected databases, it's an imposter
	servedSet := make(map[string]bool, len(served))
	for _, db := range served {
		servedSet[strings.ToLower(db)] = true
	}
	matchCount := 0
	for _, db := range fsDatabases {
		if servedSet[strings.ToLower(db)] {
			matchCount++
		}
	}
	if matchCount == 0 && len(fsDatabases) > 0 {
		return false, fmt.Errorf("server serves none of the expected %d databases — likely an imposter", len(fsDatabases))
	}

	return true, nil
}

func terminateLocalDoltServer(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding Dolt process %d: %w", pid, err)
	}
	if err := gracefulTerminate(process); err != nil {
		return fmt.Errorf("sending termination signal to Dolt PID %d: %w", pid, err)
	}
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if !processIsAlive(pid) {
			return nil
		}
	}
	if err := process.Kill(); err != nil {
		return fmt.Errorf("killing Dolt PID %d: %w", pid, err)
	}
	time.Sleep(100 * time.Millisecond)
	if processIsAlive(pid) {
		return fmt.Errorf("Dolt PID %d remained alive after kill", pid)
	}
	return nil
}

func remediateInventory(initial []LocalDoltServer, apply bool, terminate func(int) error, rescan func() []LocalDoltServer) error {
	if !apply {
		return nil
	}
	seen := make(map[int]bool)
	for _, server := range initial {
		if server.Actionable() && !seen[server.PID] {
			seen[server.PID] = true
			if err := terminate(server.PID); err != nil {
				return err
			}
		}
	}
	remaining := 0
	for _, server := range rescan() {
		if server.Actionable() {
			remaining++
		}
	}
	if remaining > 0 {
		return fmt.Errorf("%d actionable Dolt listener(s) remain", remaining)
	}
	return nil
}

func configuredPortImposters(inventory []LocalDoltServer) []LocalDoltServer {
	var imposters []LocalDoltServer
	for _, server := range inventory {
		if server.Class == DoltServerConfiguredPortImposter {
			imposters = append(imposters, server)
		}
	}
	return imposters
}

func remediateConfiguredInventory(initial []LocalDoltServer, apply bool, terminate func(int) error, rescan func() []LocalDoltServer) error {
	return remediateInventory(configuredPortImposters(initial), apply, terminate, func() []LocalDoltServer {
		return configuredPortImposters(rescan())
	})
}

func ownedLeaksSince(inventory []LocalDoltServer, baseline map[int]bool) []LocalDoltServer {
	var leaks []LocalDoltServer
	for _, server := range inventory {
		owned := server.Class == DoltServerOwnedTownLeak || server.Class == DoltServerOwnedTestLeak
		if owned && !baseline[server.PID] {
			leaks = append(leaks, server)
		}
	}
	return leaks
}

func remediateOwnedLeaksSince(initial []LocalDoltServer, baseline map[int]bool, terminate func(int) error, rescan func() []LocalDoltServer) error {
	return remediateInventory(ownedLeaksSince(initial, baseline), true, terminate, func() []LocalDoltServer {
		return ownedLeaksSince(rescan(), baseline)
	})
}

func testLeakSelectionsSince(inventory []LocalDoltServer, baseline map[DoltListener]bool) []TestLeakSelection {
	var selections []TestLeakSelection
	for _, server := range inventory {
		if baseline[server.DoltListener] {
			continue
		}
		if selection := newTestLeakSelection(server); selection.OwnershipToken != "" {
			selections = append(selections, selection)
		}
	}
	return selections
}

func remediateTestLeaksSince(initial []LocalDoltServer, baseline map[DoltListener]bool, terminate func(TestLeakSelection) error, rescan func() ([]LocalDoltServer, error)) error {
	preview := testLeakSelectionsSince(initial, baseline)
	if len(preview) == 0 {
		return nil
	}
	current, err := rescan()
	if err != nil {
		return err
	}
	return remediatePreviewedTestLeaks(current, preview, true, terminate, rescan)
}

func previewedTestLeaks(inventory []LocalDoltServer, preview []TestLeakSelection) []LocalDoltServer {
	selected := make(map[TestLeakSelection]bool, len(preview))
	for _, selection := range preview {
		selected[selection] = true
	}
	var leaks []LocalDoltServer
	for _, server := range inventory {
		if selection := newTestLeakSelection(server); selected[selection] {
			leaks = append(leaks, server)
		}
	}
	return leaks
}

func remediatePreviewedTestLeaks(initial []LocalDoltServer, preview []TestLeakSelection, apply bool, terminate func(TestLeakSelection) error, rescan func() ([]LocalDoltServer, error)) error {
	for _, wanted := range preview {
		if wanted.Class != DoltServerOwnedTestLeak || wanted.OwnershipToken == "" {
			return fmt.Errorf("invalid test-leak preview selection")
		}
		if testLeakSelectionState(initial, wanted) == revalidatedProcessChanged {
			return fmt.Errorf("previewed test-leak ownership changed for PID %d port %d", wanted.PID, wanted.Port)
		}
	}
	if !apply {
		return nil
	}
	for _, server := range previewedTestLeaks(initial, preview) {
		if err := terminate(newTestLeakSelection(server)); err != nil {
			return err
		}
	}
	current, err := rescan()
	if err != nil {
		return err
	}
	remaining := 0
	for _, wanted := range preview {
		switch testLeakSelectionState(current, wanted) {
		case revalidatedProcessChanged:
			return fmt.Errorf("previewed test-leak ownership changed for PID %d port %d on final inventory", wanted.PID, wanted.Port)
		case revalidatedProcessOwned:
			remaining++
		}
	}
	if remaining > 0 {
		return fmt.Errorf("%d previewed test-owned Dolt listener(s) remain", remaining)
	}
	return nil
}

func hasTestLeakSelection(inventory []LocalDoltServer, selection TestLeakSelection) bool {
	for _, server := range inventory {
		if newTestLeakSelection(server) == selection {
			return true
		}
	}
	return false
}

type revalidatedProcessState uint8

const (
	revalidatedProcessAbsent revalidatedProcessState = iota
	revalidatedProcessOwned
	revalidatedProcessChanged
)

func testLeakSelectionState(inventory []LocalDoltServer, selection TestLeakSelection) revalidatedProcessState {
	pidFound := false
	for _, server := range inventory {
		if server.PID != selection.PID {
			continue
		}
		pidFound = true
		if newTestLeakSelection(server) == selection {
			return revalidatedProcessOwned
		}
	}
	if pidFound || processIsAlive(selection.PID) {
		return revalidatedProcessChanged
	}
	return revalidatedProcessAbsent
}

func targetedTestLeakSelectionState(selection TestLeakSelection, expectedPort int, expectedDataDir string, listenerPID func(int) int, evidenceFor func(int) doltProcessEvidence, processAlive func(int) bool) revalidatedProcessState {
	if listenerPID(selection.Port) != selection.PID {
		if processAlive(selection.PID) {
			return revalidatedProcessChanged
		}
		return revalidatedProcessAbsent
	}
	if expectedPort != 0 && expectedPort != selection.Port && listenerPID(expectedPort) == selection.PID {
		return revalidatedProcessChanged
	}
	inventory := classifyLocalDoltServers(expectedPort, expectedDataDir, []DoltListener{{PID: selection.PID, Port: selection.Port}}, evidenceFor)
	return testLeakSelectionState(inventory, selection)
}

func terminateSelectedTestLeak(townRoot string, selection TestLeakSelection) error {
	process, err := os.FindProcess(selection.PID)
	if err != nil {
		return fmt.Errorf("finding Dolt process %d: %w", selection.PID, err)
	}
	config := DefaultConfig(townRoot)
	expectedPort, expectedDataDir := config.Port, config.DataDir
	if config.IsRemote() {
		expectedPort, expectedDataDir = 0, ""
	}
	return terminateRevalidatedProcess(selection.PID, func() (revalidatedProcessState, error) {
		return targetedTestLeakSelectionState(selection, expectedPort, expectedDataDir, findDoltServerOnPort, func(pid int) doltProcessEvidence {
			return processEvidence(townRoot, pid)
		}, processIsAlive), nil
	}, func(kill bool) error {
		if kill {
			return process.Kill()
		}
		return gracefulTerminate(process)
	}, time.Sleep)
}

func terminateSelectedTestLeakWith(selection TestLeakSelection, rescan func() ([]LocalDoltServer, error), signal func(bool) error, pause func(time.Duration)) error {
	return terminateRevalidatedProcess(selection.PID, func() (revalidatedProcessState, error) {
		current, err := rescan()
		return testLeakSelectionState(current, selection), err
	}, signal, pause)
}

func terminateRevalidatedProcess(pid int, currentState func() (revalidatedProcessState, error), signal func(bool) error, pause func(time.Duration)) error {
	state, err := currentState()
	if err != nil {
		return err
	}
	if state == revalidatedProcessChanged {
		return fmt.Errorf("process identity changed for PID %d", pid)
	}
	if state == revalidatedProcessAbsent {
		return fmt.Errorf("process PID %d is no longer present", pid)
	}
	if err := signal(false); err != nil {
		return fmt.Errorf("sending termination signal to PID %d: %w", pid, err)
	}
	for i := 0; i < 10; i++ {
		pause(500 * time.Millisecond)
		state, err = currentState()
		if err != nil {
			return err
		}
		if state == revalidatedProcessChanged {
			return fmt.Errorf("process identity changed for PID %d after termination signal", pid)
		}
		if state == revalidatedProcessAbsent {
			return nil
		}
	}
	state, err = currentState()
	if err != nil {
		return err
	}
	if state == revalidatedProcessChanged {
		return fmt.Errorf("process identity changed for PID %d before kill", pid)
	}
	if state == revalidatedProcessAbsent {
		return nil
	}
	if err := signal(true); err != nil {
		return fmt.Errorf("killing PID %d: %w", pid, err)
	}
	pause(100 * time.Millisecond)
	state, err = currentState()
	if err != nil {
		return err
	}
	if state == revalidatedProcessChanged {
		return fmt.Errorf("process identity changed for PID %d after kill", pid)
	}
	if state == revalidatedProcessOwned {
		return fmt.Errorf("PID %d remained alive after kill", pid)
	}
	return nil
}

// TerminateTestProcessCustody stops only the still-matching direct child.
func TerminateTestProcessCustody(custody TestProcessCustody) error {
	if custody.PID <= 0 || custody.ParentPID <= 0 || custody.OwnershipToken == "" {
		return fmt.Errorf("invalid test-process custody")
	}
	process, err := os.FindProcess(custody.PID)
	if err != nil {
		return fmt.Errorf("finding test process %d: %w", custody.PID, err)
	}
	return terminateRevalidatedProcess(custody.PID, func() (revalidatedProcessState, error) {
		if currentTestProcessCustody(custody.PID, custody.ParentPID) == custody {
			return revalidatedProcessOwned, nil
		}
		if processIsAlive(custody.PID) {
			return revalidatedProcessChanged, nil
		}
		return revalidatedProcessAbsent, nil
	}, func(kill bool) error {
		if kill {
			return process.Kill()
		}
		return gracefulTerminate(process)
	}, time.Sleep)
}

// RemediateLocalDoltServers previews by default and only signals listeners
// with an actionable inventory classification when apply is true.
func RemediateLocalDoltServers(townRoot string, apply bool) ([]LocalDoltServer, error) {
	initial := InventoryLocalDoltServers(townRoot)
	err := remediateInventory(initial, apply, terminateLocalDoltServer, func() []LocalDoltServer {
		return InventoryLocalDoltServers(townRoot)
	})
	return initial, err
}

// RemediateConfiguredPortImposters previews the full inventory but applies only
// to imposters occupying this town's configured port.
func RemediateConfiguredPortImposters(townRoot string, apply bool) ([]LocalDoltServer, error) {
	initial := InventoryLocalDoltServers(townRoot)
	err := remediateConfiguredInventory(initial, apply, terminateLocalDoltServer, func() []LocalDoltServer {
		return InventoryLocalDoltServers(townRoot)
	})
	return initial, err
}

// CleanupOwnedLocalDoltLeaks removes only new listeners with positive town/test
// ownership evidence. Pre-existing and unknown listeners are never signaled.
func CleanupOwnedLocalDoltLeaks(townRoot string, baseline []DoltListener) error {
	baselinePIDs := make(map[int]bool, len(baseline))
	for _, listener := range baseline {
		baselinePIDs[listener.PID] = true
	}
	return remediateOwnedLeaksSince(InventoryLocalDoltServers(townRoot), baselinePIDs, terminateLocalDoltServer, func() []LocalDoltServer {
		return InventoryLocalDoltServers(townRoot)
	})
}

// CleanupOwnedLocalTestLeaks removes only positively test-owned listeners that
// appeared after baseline, revalidating ownership before signaling.
func CleanupOwnedLocalTestLeaks(townRoot string, baseline []DoltListener, ownerRoot string) error {
	baselineListeners := make(map[DoltListener]bool, len(baseline))
	for _, listener := range baseline {
		baselineListeners[listener] = true
	}
	rescan := func() ([]LocalDoltServer, error) {
		inventory, err := inventoryLocalDoltServers(townRoot)
		if err != nil {
			return nil, err
		}
		return testLeaksWithin(inventory, ownerRoot), nil
	}
	initial, err := rescan()
	if err != nil {
		return err
	}
	return remediateTestLeaksSince(initial, baselineListeners, func(selection TestLeakSelection) error {
		return terminateSelectedTestLeak(townRoot, selection)
	}, rescan)
}

func testLeaksWithin(inventory []LocalDoltServer, ownerRoot string) []LocalDoltServer {
	var owned []LocalDoltServer
	for _, server := range inventory {
		if server.Class == DoltServerOwnedTestLeak && pathWithin(ownerRoot, server.OwnerPath) {
			owned = append(owned, server)
		}
	}
	return owned
}

// RemediatePreviewedTestLeaks applies only to still-positive test-owned
// listeners that were captured by a prior preview.
func RemediatePreviewedTestLeaks(townRoot string, preview []TestLeakSelection, apply bool) ([]LocalDoltServer, error) {
	initial, inventoryErr := inventoryLocalDoltServers(townRoot)
	if inventoryErr != nil {
		return nil, inventoryErr
	}
	err := remediatePreviewedTestLeaks(initial, preview, apply, func(selection TestLeakSelection) error {
		return terminateSelectedTestLeak(townRoot, selection)
	}, func() ([]LocalDoltServer, error) {
		return inventoryLocalDoltServers(townRoot)
	})
	return initial, err
}

// KillImposters remediates configured-port imposters only.
func KillImposters(townRoot string) error {
	_, err := RemediateConfiguredPortImposters(townRoot, true)
	return err
}

// containsPathBoundary checks whether line contains path as a complete path
// (not a prefix of a longer path). The character after the match must be a
// path separator, whitespace, or end-of-string.
func containsPathBoundary(line, path string) bool {
	if path == "" {
		return false
	}
	for start := 0; start < len(line); {
		idx := strings.Index(line[start:], path)
		if idx < 0 {
			return false
		}
		end := start + idx + len(path)
		if end >= len(line) {
			return true
		}
		c := line[end]
		if c == filepath.Separator || c == ' ' || c == '\t' {
			return true
		}
		start = start + idx + 1
	}
	return false
}

// FindIdleMonitorProcesses finds "bd dolt idle-monitor" processes scoped to
// this town. Matches by town root path in process args, or by the town's
// configured Dolt port. It is side-effect free so callers can use it for dry-run
// and shutdown verification without duplicating the matching rules.
func FindIdleMonitorProcesses(townRoot string) []int {
	absRoot, _ := filepath.Abs(townRoot)
	if absRoot == "" {
		return nil
	}

	psCmd := exec.Command("ps", "-eo", "pid,args")
	setProcessGroup(psCmd)
	output, err := psCmd.Output()
	if err != nil {
		return nil
	}

	config := DefaultConfig(townRoot)
	return findIdleMonitorProcessesFromPS(string(output), townRoot, absRoot, config.Port)
}

func findIdleMonitorProcessesFromPS(output, townRoot, absRoot string, port int) []int {
	portStr := strconv.Itoa(port)
	var pids []int
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "idle-monitor") {
			continue
		}
		if !strings.Contains(line, "dolt") {
			continue
		}
		// Scope to this town: match by path in args using path-boundary check
		// to avoid false matches on sibling paths (e.g., /tmp/gt matching /tmp/gt-old)
		matchesTown := containsPathBoundary(line, absRoot) || containsPathBoundary(line, townRoot)
		if !matchesTown {
			// Check for --port <portStr> as a discrete argument to avoid
			// false matches on PIDs or other numeric substrings
			args := strings.Fields(line)
			for i, arg := range args {
				if (arg == "--port" || arg == "-p") && i+1 < len(args) && args[i+1] == portStr {
					matchesTown = true
					break
				}
				if arg == "--port="+portStr {
					matchesTown = true
					break
				}
			}
		}
		if !matchesTown {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if filepath.Base(fields[1]) == "grep" {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// StopIdleMonitors finds and terminates "bd dolt idle-monitor" processes
// associated with this town. These background processes auto-spawn rogue
// Dolt servers from per-rig .beads/dolt/ directories when the canonical
// server is unreachable, creating a race condition during restart.
func StopIdleMonitors(townRoot string) int {
	stopped := 0
	for _, pid := range FindIdleMonitorProcesses(townRoot) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Kill(); err != nil {
			continue
		}

		// Wait briefly for termination
		for i := 0; i < 5; i++ {
			time.Sleep(100 * time.Millisecond)
			if !processIsAlive(pid) {
				break // Process exited
			}
			if i == 4 {
				_ = proc.Kill()
			}
		}
		stopped++
	}

	return stopped
}

// ReapOwnedTestServers terminates dolt sql-server processes that are provably
// owned by townRoot. This is intentionally narrower than operational cleanup:
// tests must never kill production Dolt by port or broad process pattern.
func ReapOwnedTestServers(townRoot string) (int, error) {
	absRoot, err := filepath.Abs(townRoot)
	if err != nil {
		return 0, fmt.Errorf("resolving town root: %w", err)
	}
	absTemp, err := filepath.Abs(os.TempDir())
	if err != nil {
		return 0, fmt.Errorf("resolving temp dir: %w", err)
	}
	rel, err := filepath.Rel(absTemp, absRoot)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return 0, fmt.Errorf("refusing to reap Dolt outside temp dir: %s", absRoot)
	}

	rescan := func() ([]LocalDoltServer, error) {
		return InventoryLocalDoltServersWithError(absRoot)
	}
	stopped, err := reapOwnedTestServersWith(absRoot, rescan, func(selection TestLeakSelection, rescan func() ([]LocalDoltServer, error)) error {
		return terminateSelectedTestLeak(absRoot, selection)
	})
	if err != nil {
		return 0, err
	}

	config := DefaultConfig(absRoot)
	candidates := ownedDoltTestServerCandidates(absRoot, config)
	for _, pid := range candidates {
		if pid <= 0 || pid == os.Getpid() || !processIsAlive(pid) {
			continue
		}
		if !isDoltSQLServerProcess(pid) {
			continue
		}
		if !doltProcessMatchesTown(absRoot, pid, config) {
			continue
		}
		processToken := getProcessStartToken(pid)
		if processToken == "" {
			return stopped, fmt.Errorf("capturing owned Dolt PID %d identity", pid)
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return stopped, fmt.Errorf("finding owned Dolt PID %d: %w", pid, err)
		}
		if err := terminateRevalidatedProcess(pid, func() (revalidatedProcessState, error) {
			if !processIsAlive(pid) {
				return revalidatedProcessAbsent, nil
			}
			if getProcessStartToken(pid) == processToken && isDoltSQLServerProcess(pid) && doltProcessMatchesTown(absRoot, pid, config) {
				return revalidatedProcessOwned, nil
			}
			return revalidatedProcessChanged, nil
		}, func(kill bool) error {
			if kill {
				return proc.Kill()
			}
			return gracefulTerminate(proc)
		}, time.Sleep); err != nil {
			return stopped, fmt.Errorf("terminating owned Dolt PID %d: %w", pid, err)
		}
		stopped++
	}

	return stopped, nil
}

func reapOwnedTestServersWith(ownerRoot string, rescan func() ([]LocalDoltServer, error), terminate func(TestLeakSelection, func() ([]LocalDoltServer, error)) error) (int, error) {
	initial, err := rescan()
	if err != nil {
		return 0, err
	}
	preview := TestLeakSelections(testLeaksWithin(initial, ownerRoot))
	if err := remediatePreviewedTestLeaks(initial, preview, true, func(selection TestLeakSelection) error {
		return terminate(selection, rescan)
	}, rescan); err != nil {
		return 0, err
	}
	return len(preview), nil
}

func ownedDoltTestServerCandidates(townRoot string, config *Config) []int {
	seen := map[int]bool{}
	var pids []int
	add := func(pid int) {
		if pid <= 0 || seen[pid] {
			return
		}
		seen[pid] = true
		pids = append(pids, pid)
	}

	if data, err := os.ReadFile(config.PidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			add(pid)
		}
	}
	if info, err := readSQLServerInfo(config); err == nil {
		add(info.PID)
	}
	for _, pid := range findOwnedDoltTestServerCandidatesFromPS(processList(), townRoot, config.DataDir) {
		add(pid)
	}
	return pids
}

func processList() string {
	cmd := exec.Command("ps", "-eo", "pid,args")
	setProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func findOwnedDoltTestServerCandidatesFromPS(output, townRoot, dataDir string) []int {
	absRoot, _ := filepath.Abs(townRoot)
	absDataDir, _ := filepath.Abs(dataDir)
	var pids []int
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "sql-server") || !strings.Contains(line, "dolt") {
			continue
		}
		if !containsPathBoundary(line, absRoot) && !containsPathBoundary(line, absDataDir) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		if isDoltSQLServerArgs(fields[1:]) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func isDoltSQLServerProcess(pid int) bool {
	return isDoltSQLServerArgs(getProcessArgs(pid))
}

func isDoltSQLServerArgs(args []string) bool {
	return len(args) >= 2 && filepath.Base(args[0]) == "dolt" && args[1] == "sql-server"
}

// CheckPortAvailable verifies that a TCP port is free for use as a Dolt server.
// Returns a user-friendly error if the port is already in use.
func CheckPortAvailable(port int) error {
	return checkPortAvailable(port)
}

// PortHolder returns the PID and data directory of the process holding port.
// Returns (0, "") if the port is free or the holder cannot be identified.
// Note: data directory is only available when townRoot context is known;
// without it, returns PID only (ZFC fix: gt-utuk).
func PortHolder(port int) (pid int, dataDir string) {
	pid = findDoltServerOnPort(port)
	if pid <= 0 {
		return 0, ""
	}
	if dataDir = GetDoltDataDirFromProcess(pid); dataDir != "" {
		return pid, dataDir
	}
	if configPath := getDoltConfigPathFromProcess(pid); configPath != "" {
		if filepath.Base(configPath) == "config.yaml" {
			return pid, filepath.Dir(configPath)
		}
		return pid, configPath
	}
	if cwd := getProcessCWD(pid); cwd != "" {
		return pid, cwd
	}
	return pid, ""
}

// FindFreePort returns the first free TCP port at or above startFrom.
// Returns 0 if no free port is found within 100 attempts.
func FindFreePort(startFrom int) int {
	for port := startFrom; port < startFrom+100; port++ {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			_ = ln.Close()
			return port
		}
	}
	return 0
}

func checkPortAvailable(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// Try to identify who holds the port
		detail := ""
		if pid := findDoltServerOnPort(port); pid > 0 {
			detail = fmt.Sprintf("\nPort is held by PID %d", pid)
		}
		return fmt.Errorf("port %d is already in use.%s\n"+
			"If you're running multiple Gas Town instances, each needs a unique Dolt port.\n"+
			"Set GT_DOLT_PORT in mayor/daemon.json env section:\n"+
			"  {\"env\": {\"GT_DOLT_PORT\": \"<port>\"}}", port, detail)
	}
	_ = ln.Close()
	return nil
}

// waitForPortRelease polls until the given port is free or the timeout expires.
// Used after killing an imposter to ensure the port is available before starting
// the canonical server, avoiding the race where a dying process still holds the port.
func waitForPortRelease(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = ln.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("port %d not released within %s", port, timeout)
}

// writeServerConfig writes a managed Dolt config.yaml from the Config struct.
// This ensures all required settings (especially connection timeouts) are always
// present when the server starts. The file is overwritten on each start to prevent
// configuration drift.
func writeServerConfig(config *Config, configPath string) error {
	// Build the listener host entry. Omit it when empty to use Dolt's default
	// (binds to all interfaces), which is the backward-compatible behavior.
	hostLine := ""
	if config.Host != "" {
		hostLine = fmt.Sprintf("\n  host: %s", config.Host)
	}

	// Build timeout entries. Omit when 0 to use Dolt's defaults (not recommended).
	readTimeoutLine := ""
	if config.ReadTimeoutMs > 0 {
		readTimeoutLine = fmt.Sprintf("\n  read_timeout_millis: %d", config.ReadTimeoutMs)
	}
	writeTimeoutLine := ""
	if config.WriteTimeoutMs > 0 {
		writeTimeoutLine = fmt.Sprintf("\n  write_timeout_millis: %d", config.WriteTimeoutMs)
	}

	maxConnLine := ""
	if config.MaxConnections > 0 {
		maxConnLine = fmt.Sprintf("\n  max_connections: %d", config.MaxConnections)
	}
	eventSchedulerLine := "  event_scheduler: \"OFF\"\n"
	if strings.EqualFold(config.EventScheduler, "omit") {
		eventSchedulerLine = ""
	} else if strings.TrimSpace(config.EventScheduler) != "" {
		eventSchedulerLine = fmt.Sprintf("  event_scheduler: %q\n", strings.ToUpper(strings.TrimSpace(config.EventScheduler)))
	}
	systemVariablesBlock := "\nsystem_variables:\n  dolt_stats_enabled: 0\n"
	if strings.EqualFold(config.DoltStatsEnabled, "omit") {
		systemVariablesBlock = ""
	} else if strings.TrimSpace(config.DoltStatsEnabled) != "" {
		systemVariablesBlock = fmt.Sprintf("\nsystem_variables:\n  dolt_stats_enabled: %s\n", strings.TrimSpace(config.DoltStatsEnabled))
	}

	// Non-blocking storage GC keeps the managed sql-server's RSS bounded (hq-excy9g);
	// enabled by default. GT_DOLT_AUTO_GC=off (or false/0/disabled) turns it off at
	// runtime (next Dolt restart) without a source revert+rebuild — the escape hatch
	// given the old blocking-GC lockup history. Dolt's non-blocking auto-GC is
	// default-on since 1.75.
	autoGcBlock := "  auto_gc_behavior:\n    enable: true\n    archive_level: 1\n"
	if v := strings.ToLower(strings.TrimSpace(config.AutoGC)); v == "off" || v == "false" || v == "0" || v == "disabled" {
		autoGcBlock = "  auto_gc_behavior:\n    enable: false\n    archive_level: 0\n"
	}

	content := fmt.Sprintf(`# Dolt SQL server configuration — managed by Gas Town (gt dolt start)
# Do not edit manually; changes are overwritten on each server start.
# To customize, set Gas Town environment variables:
#   GT_DOLT_PORT, GT_DOLT_HOST, GT_DOLT_USER, GT_DOLT_PASSWORD, GT_DOLT_LOGLEVEL
#   GT_DOLT_EVENT_SCHEDULER (OFF, ON, omit), GT_DOLT_STATS_ENABLED (0, 1, omit)
#   GT_DOLT_AUTO_GC (on, off)

log_level: %s

listener:
  port: %d%s%s%s%s

data_dir: "%s"

behavior:
  dolt_transaction_commit: false
%s%s%s`,
		config.LogLevel,
		config.Port,
		hostLine,
		maxConnLine,
		readTimeoutLine,
		writeTimeoutLine,
		filepath.ToSlash(config.DataDir),
		eventSchedulerLine,
		autoGcBlock,
		systemVariablesBlock,
	)

	return os.WriteFile(configPath, []byte(content), 0600)
}

func doltLifecycleLockPath(townRoot string) string {
	return filepath.Join(filepath.Dir(DefaultConfig(townRoot).LogFile), "dolt.lock")
}

func tryLockDoltLifecycle(townRoot string) (*flock.Flock, error) {
	lockPath := doltLifecycleLockPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("creating Dolt daemon directory: %w", err)
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquiring Dolt lifecycle lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("Dolt start or cleanup is in progress")
	}
	return lock, nil
}

func withDoltLifecycle(townRoot string, operation func() error) error {
	doltLifecycleMu.Lock()
	defer doltLifecycleMu.Unlock()
	lifecycleLock, err := tryLockDoltLifecycle(townRoot)
	if err != nil {
		return err
	}
	defer func() { _ = lifecycleLock.Unlock() }()
	return operation()
}

// Start starts the Dolt SQL server.
func Start(townRoot string) error {
	return start(townRoot, nil)
}

func start(townRoot string, started func(doltServerIncarnation)) (result error) {
	doltLifecycleMu.Lock()
	defer doltLifecycleMu.Unlock()

	config := DefaultConfig(townRoot)

	// Ensure daemon directory exists
	daemonDir := filepath.Dir(config.LogFile)
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		return fmt.Errorf("creating daemon directory: %w", err)
	}

	// Acquire exclusive lock to prevent concurrent starts (same pattern as gt daemon).
	// If the lock is held, retry briefly — the holder may be finishing up. If still
	// held after retries, check if the holding process is alive. (gt-tosjp)
	lockFile := doltLifecycleLockPath(townRoot)
	fileLock := flock.New(lockFile)
	locked, err := fileLock.TryLock()
	if err != nil {
		// Lock file may be corrupted — remove and retry once
		_ = os.Remove(lockFile)
		locked, err = fileLock.TryLock()
		if err != nil {
			return fmt.Errorf("acquiring lock: %w", err)
		}
	}
	if !locked {
		// Scale the retry window by the number of databases: each database takes
		// ~5s to initialize. Clamp between 15s and 120s to handle both small and
		// large installs. (gt-nkn: fix thundering herd)
		numDBs := countDoltDatabases(config.DataDir)
		lockTimeout := time.Duration(numDBs) * 5 * time.Second
		if lockTimeout < 15*time.Second {
			lockTimeout = 15 * time.Second
		}
		if lockTimeout > 120*time.Second {
			lockTimeout = 120 * time.Second
		}
		interval := 500 * time.Millisecond
		deadline := time.Now().Add(lockTimeout)
		for time.Now().Before(deadline) {
			time.Sleep(interval)
			locked, err = fileLock.TryLock()
			if err == nil && locked {
				break
			}
		}
		if !locked {
			// Still locked after the full timeout. The holder may have finished
			// starting Dolt successfully. If so, return nil instead of spawning a
			// duplicate server. Never unlink a contended flock: cleanup uses the
			// same lifecycle lock, and POSIX releases it automatically on death.
			if already, _, _ := IsRunning(townRoot); already {
				return nil
			}
			return fmt.Errorf("another gt dolt start or cleanup is in progress (lock held for %s)", lockTimeout.Round(time.Second))
		}
	}
	defer func() { _ = fileLock.Unlock() }()

	// Stop idle-monitor processes first. These background processes auto-spawn
	// rogue Dolt servers and will immediately respawn an imposter if we kill
	// one without stopping the monitors. (gt-restart-race fix)
	if stopped := StopIdleMonitors(townRoot); stopped > 0 {
		fmt.Fprintf(os.Stderr, "Stopped %d idle-monitor process(es)\n", stopped)
		// Brief pause to let spawned rogue processes settle
		time.Sleep(200 * time.Millisecond)
	}

	// Check if already running (checks both PID file AND port)
	running, pid, err := IsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking server status: %w", err)
	}

	// If IsRunning returns false, the port may still be held by a dolt process
	// that doesn't match this town's ownership (e.g., a leftover from an old
	// town setup started with different flags). IsRunning's ownership check
	// correctly returns false, but we need to evict the squatter before we can
	// bind the port. (fix: start-kills-unowned-port-holder)
	if !running {
		if squatterPID := findDoltServerOnPort(config.Port); squatterPID > 0 {
			fmt.Fprintf(os.Stderr, "Warning: port %d held by unowned dolt process (PID %d) — killing before start\n", config.Port, squatterPID)
			if proc, findErr := os.FindProcess(squatterPID); findErr == nil {
				_ = proc.Kill()
				if err := waitForPortRelease(config.Port, 5*time.Second); err != nil {
					// Kill didn't work, try again
					_ = proc.Kill()
					if err := waitForPortRelease(config.Port, 3*time.Second); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: port %d still occupied after killing PID %d: %v\n", config.Port, squatterPID, err)
					}
				}
			}
		}
	}

	if running {
		// If data directory doesn't exist, this is an orphaned server (e.g., user
		// deleted ~/gt and re-ran gt install). Kill it so we can start fresh.
		if _, statErr := os.Stat(config.DataDir); os.IsNotExist(statErr) {
			fmt.Fprintf(os.Stderr, "Warning: Dolt server (PID %d) is running but data directory %s does not exist — stopping orphaned server\n", pid, config.DataDir)
			if stopErr := stopLocked(townRoot); stopErr != nil {
				if pid > 0 {
					if proc, findErr := os.FindProcess(pid); findErr == nil {
						_ = proc.Kill()
						time.Sleep(100 * time.Millisecond)
					}
				}
			}
			// Fall through to start a new server
		} else {
			// Server is running with valid data dir — check if it's an imposter
			// (e.g., bd launched its own dolt server from a different data directory).
			legitimate, verifyErr := VerifyServerDataDir(townRoot)
			if verifyErr == nil && !legitimate {
				fmt.Fprintf(os.Stderr, "Warning: running Dolt server (PID %d) is an imposter — killing and restarting\n", pid)
				if killErr := KillImposters(townRoot); killErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to kill imposter: %v\n", killErr)
				}
				// Wait for port to be released, with retry
				if err := waitForPortRelease(config.Port, 5*time.Second); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: port %d still occupied after imposter kill: %v\n", config.Port, err)
				}
				// Fall through to start a new server
			} else if verifyErr != nil && !legitimate {
				// Verification failed but server is suspicious — log and try to kill
				fmt.Fprintf(os.Stderr, "Warning: could not verify Dolt server identity: %v — killing and restarting\n", verifyErr)
				if killErr := KillImposters(townRoot); killErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to kill imposter: %v\n", killErr)
				}
				if err := waitForPortRelease(config.Port, 5*time.Second); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: port %d still occupied after imposter kill: %v\n", config.Port, err)
				}
			} else {
				if changed, refreshErr := refreshPIDStateFromLiveInfo(townRoot, config, pid); refreshErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not refresh Dolt PID state: %v\n", refreshErr)
				} else if changed {
					fmt.Printf("Refreshed stale Dolt PID state (actual %d)\n", pid)
				}
				return nil // already running and legitimate — idempotent success
			}
		}
	}

	// Ensure data directory exists
	if err := os.MkdirAll(config.DataDir, 0755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	// Quarantine corrupted/phantom database dirs before server launch.
	// WARNING: DO NOT remove, delete, or modify files inside Dolt's .dolt/
	// directory — including noms/LOCK files. These are Dolt-internal files.
	// Removing them WILL cause unrecoverable data corruption and data loss.
	// Dolt manages these files itself; external interference is never safe.
	//
	// Previously this section quarantined/removed database dirs with missing
	// noms/manifest and cleaned up stale .dolt/noms/LOCK files. Both operations
	// manipulated Dolt-internal state and risked data corruption. Dolt handles
	// its own lock files and database integrity on startup.

	databases, _ := ListDatabases(townRoot)

	// Open log file
	logFile, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}

	// Remove stale Unix socket left behind by a previous Dolt crash.
	// Dolt creates /tmp/mysql.sock by default; if not cleaned up, the
	// next start emits "unix socket set up failed: file already in use"
	// and falls back to TCP-only. (GH#2687)
	cleanStaleDoltSocket()

	// Validate port is available before starting (catches multi-town port conflicts)
	if err := checkPortAvailable(config.Port); err != nil {
		logFile.Close()
		return err
	}

	// Clean stale Unix socket from prior crash. Dolt creates /tmp/mysql.sock by
	// default (or a port-specific variant). If the server crashed, the socket file
	// persists and Dolt warns "unix socket set up failed: file already in use".
	// Safe to remove: if a Dolt server were actually running, IsRunning() above
	// would have detected it and we'd have returned already. (gh-2687)
	socketPath := "/tmp/mysql.sock"
	if config.Port != 3306 {
		socketPath = fmt.Sprintf("/tmp/mysql.%d.sock", config.Port)
	}
	if _, statErr := os.Stat(socketPath); statErr == nil {
		fmt.Fprintf(os.Stderr, "Removing stale Unix socket: %s\n", socketPath)
		_ = os.Remove(socketPath)
	}

	// Always write a managed config.yaml from the Config struct before starting.
	// This ensures critical settings (especially read/write timeouts) are always
	// present, preventing CLOSE_WAIT accumulation from abandoned connections.
	// The config file uses --config so all settings come from this file; CLI flags
	// are ignored by dolt when --config is used.
	configPath := filepath.Join(config.DataDir, "config.yaml")
	if err := writeServerConfig(config, configPath); err != nil {
		logFile.Close()
		return fmt.Errorf("writing Dolt config: %w", err)
	}
	cmd := NewSQLServerCommand("dolt", config.DataDir, configPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Detach from terminal and put dolt in its own process group so that
	// signals sent to the parent process group (e.g. SIGHUP when the caller
	// calls syscall.Exec to become tmux) don't reach the dolt server.
	cmd.Stdin = nil
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		if closeErr := logFile.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close dolt log file: %v\n", closeErr)
		}
		return fmt.Errorf("starting Dolt server: %w", err)
	}
	incarnation := doltServerIncarnation{PID: cmd.Process.Pid, ProcessStartToken: getProcessStartToken(cmd.Process.Pid)}
	if started != nil {
		started(incarnation)
	}
	startupComplete := false
	defer func() {
		if !startupComplete {
			result = errors.Join(result, cleanupFailedDoltStart(townRoot, config, cmd, incarnation))
		}
	}()

	// Close log file in parent (child has its own handle)
	if closeErr := logFile.Close(); closeErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to close dolt log file: %v\n", closeErr)
	}

	// Write PID file
	if err := os.WriteFile(config.PidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0644); err != nil {
		// Try to kill the process we just started
		_ = cmd.Process.Kill()
		return fmt.Errorf("writing PID file: %w", err)
	}

	// Save state
	state := &State{
		Running:   true,
		PID:       cmd.Process.Pid,
		Port:      config.Port,
		StartedAt: time.Now(),
		DataDir:   config.DataDir,
		Databases: databases,
	}
	if err := SaveState(townRoot, state); err != nil {
		// Non-fatal - server is still running
		fmt.Fprintf(os.Stderr, "Warning: failed to save state: %v\n", err)
	}

	// Wait for the server to be accepting connections, not just alive.
	// We check process liveness directly via signal(0) rather than calling
	// IsRunning, because IsRunning removes the PID file when the process is
	// alive but not yet listening — treating a starting-up process as stale.
	// On systems with slow storage (CSI/NFS), dolt can take 1-2s to bind its
	// port, well past the first 500ms check. By using cmd.Process.Signal(0)
	// we detect true process death without the PID-file side effect.
	//
	// The number of attempts scales with the database count: each database
	// adds ~1s of startup overhead (LevelDB compaction, stats loading, etc.).
	// We allow 5s per database so that workspaces with many rigs don't time
	// out before Dolt finishes initializing.
	dbCount := len(databases)
	if dbCount < 1 {
		dbCount = 1
	}
	maxAttempts := dbCount * 10 // 10 × 500ms = 5s per database
	var lastErr error
	tcpReachable := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		time.Sleep(500 * time.Millisecond)

		// Check if the process we started is still alive.
		if !processIsAlive(cmd.Process.Pid) {
			return fmt.Errorf("Dolt server process died during startup (check logs with 'gt dolt logs')")
		}

		if !tcpReachable {
			if err := CheckServerReachable(townRoot); err != nil {
				lastErr = err
				continue
			}
			tcpReachable = true
		}

		// TCP listener is up. Verify that the expected on-disk databases are
		// actually being served before declaring success. Without this check
		// Start() can return on the first iteration where Dolt has bound its
		// port but is still discovering/loading databases — leaving callers
		// (and waiting agents) connected to a server that only exposes
		// information_schema and mysql. Symptom: gt down + gt up cycle leaves
		// SHOW DATABASES showing no rig databases until the user manually
		// runs gt dolt stop + gt dolt start. (gt-nq1)
		if len(databases) == 0 {
			applyWaitTimeout(townRoot, config) // Best-effort; see gh-3623.
			applyTimeZone(townRoot, config)    // Best-effort; see hq-57jr8.
			startupComplete = true
			return nil // Nothing to verify — fresh install or empty data dir
		}
		_, missing, verifyErr := VerifyDatabases(townRoot)
		if verifyErr != nil {
			lastErr = fmt.Errorf("verifying databases: %w", verifyErr)
			continue
		}
		if len(missing) == 0 {
			applyWaitTimeout(townRoot, config) // Best-effort; see gh-3623.
			applyTimeZone(townRoot, config)    // Best-effort; see hq-57jr8.
			startupComplete = true
			return nil // Server is up and serving every expected database
		}
		lastErr = fmt.Errorf("server is reachable but %d/%d databases not yet served (missing: %v)",
			len(missing), len(databases), missing)
	}

	totalTimeout := time.Duration(dbCount) * 5 * time.Second
	if !tcpReachable {
		return fmt.Errorf("Dolt server process started (PID %d) but not accepting connections after %v (%d databases × 5s): %w\nCheck logs with: gt dolt logs", cmd.Process.Pid, totalTimeout, dbCount, lastErr)
	}
	return fmt.Errorf("Dolt server process started (PID %d) and is reachable, but databases failed to load after %v (%d databases × 5s): %w\nRecovery: gt dolt stop && gt dolt start\nCheck logs with: gt dolt logs", cmd.Process.Pid, totalTimeout, dbCount, lastErr)
}

func cleanupFailedDoltStart(townRoot string, config *Config, cmd *exec.Cmd, incarnation doltServerIncarnation) error {
	var cleanupErr error
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("killing failed Dolt start: %w", err))
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("waiting for failed Dolt start: %w", err))
		}
	}
	if data, err := os.ReadFile(config.PidFile); err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(incarnation.PID) {
		if err := os.Remove(config.PidFile); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("removing failed Dolt PID file: %w", err))
		}
	}
	state, err := LoadState(townRoot)
	if err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("loading failed Dolt state: %w", err))
	} else if state.PID == incarnation.PID {
		state.Running = false
		state.PID = 0
		if err := SaveState(townRoot, state); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("saving failed Dolt state: %w", err))
		}
	}
	return cleanupErr
}

// WARNING: DO NOT remove, delete, or modify files inside Dolt's .dolt/
// directory — including noms/LOCK files. These are Dolt-internal files.
// Removing them WILL cause unrecoverable data corruption and data loss.
// Dolt manages these files itself; external interference is never safe.
//
// cleanupStaleDoltLock previously removed stale .dolt/noms/LOCK files.
// This was unsafe — Dolt manages its own lock files on startup.

// DefaultDoltSocketPath is the default Unix socket Dolt creates.
const DefaultDoltSocketPath = "/tmp/mysql.sock"

// cleanStaleDoltSocket removes the default Unix socket file that Dolt creates
// at /tmp/mysql.sock. After a crash, this file lingers and prevents the next
// server start from binding the Unix socket, causing a warning and TCP-only
// fallback.
func cleanStaleDoltSocket() {
	cleanStaleSocket(DefaultDoltSocketPath)
}

// cleanStaleSocket removes a Unix socket file if it exists and no process
// currently holds it open.
func cleanStaleSocket(socketPath string) {
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		return
	}

	// Check if any process holds the socket open
	cmd := exec.Command("lsof", socketPath)
	setProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		// lsof exit code 1 = no process holds it → stale, safe to remove
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			_ = os.Remove(socketPath)
		}
	}
	// If lsof succeeds (exit 0), a process is using it — leave it alone.
}

// drainConnectionsBeforeStop waits for active queries to complete before SIGTERM,
// reducing the nbs_manifest race window in Dolt's NomsBlockStore.Close() (gt-9bxzs).
//
// Dolt panics (Fatalf) when SIGTERM arrives while a goroutine is mid-write on an
// nbs_manifest temp file. By waiting until no queries are in-flight, we shrink
// the window where SIGTERM hits live storage I/O. Non-fatal: if the drain times
// out or the server is unreachable, we proceed with SIGTERM anyway.
func drainConnectionsBeforeStop(config *Config) {
	dsn := fmt.Sprintf("%s@tcp(%s:%d)/?timeout=3s&readTimeout=5s&writeTimeout=5s",
		config.User, config.EffectiveHost(), config.Port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return
	}
	defer db.Close()
	db.SetConnMaxLifetime(5 * time.Second)
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Poll until only 1 connection remains (ours) or the drain window expires.
	// INFORMATION_SCHEMA.PROCESSLIST counts all server connections including ours.
	for {
		select {
		case <-ctx.Done():
			return // Drain window expired — proceed with SIGTERM
		default:
		}
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM INFORMATION_SCHEMA.PROCESSLIST").Scan(&count); err != nil {
			return // Server unreachable — proceed with SIGTERM
		}
		if count <= 1 {
			// Only our drain connection remains — safe to send SIGTERM
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Stop stops the Dolt SQL server.
// Works for both servers started via gt dolt start AND externally-started servers.
func Stop(townRoot string) error {
	doltLifecycleMu.Lock()
	defer doltLifecycleMu.Unlock()
	lifecycleLock, err := tryLockDoltLifecycle(townRoot)
	if err != nil {
		return err
	}
	defer func() { _ = lifecycleLock.Unlock() }()
	return stopLocked(townRoot)
}

func stopDoltIncarnation(townRoot string, incarnation doltServerIncarnation) error {
	if incarnation.PID <= 0 {
		return nil
	}
	return withDoltLifecycle(townRoot, func() error {
		running, pid, err := IsRunning(townRoot)
		if err != nil {
			return fmt.Errorf("checking temporary Dolt server: %w", err)
		}
		if !running {
			return nil
		}
		if pid != incarnation.PID {
			return fmt.Errorf("temporary Dolt server changed from PID %d to PID %d", incarnation.PID, pid)
		}
		if incarnation.ProcessStartToken != "" && getProcessStartToken(pid) != incarnation.ProcessStartToken {
			return fmt.Errorf("temporary Dolt server PID %d was reused", pid)
		}
		return stopLocked(townRoot)
	})
}

// WithStoppedDoltForDatabaseMove excludes start/stop races and prevents a live
// server from observing a partial physical database move. A server that was
// running is restored after the operation, including when the operation fails.
func WithStoppedDoltForDatabaseMove(townRoot string, operation func() error) error {
	if operation == nil {
		return fmt.Errorf("database move operation is required")
	}
	wasRunning := false
	operationErr := func() error {
		doltLifecycleMu.Lock()
		defer doltLifecycleMu.Unlock()
		lifecycleLock, err := tryLockDoltLifecycle(townRoot)
		if err != nil {
			return fmt.Errorf("locking Dolt lifecycle for database move: %w", err)
		}
		defer func() { _ = lifecycleLock.Unlock() }()

		running, pid, err := IsRunning(townRoot)
		if err != nil {
			return fmt.Errorf("checking Dolt lifecycle before database move: %w", err)
		}
		if running && pid <= 0 {
			return fmt.Errorf("Dolt server is reachable but local process ownership is unavailable; refusing database move")
		}
		if running {
			const minStableAge = 60 * time.Second
			state, _ := LoadState(townRoot)
			if state != nil && !state.StartedAt.IsZero() && time.Since(state.StartedAt) < minStableAge {
				return fmt.Errorf("Dolt server is too new to stop safely for a database move")
			}
			wasRunning = true
			if err := stopLocked(townRoot); err != nil {
				return fmt.Errorf("stopping Dolt server for database move: %w", err)
			}
		}
		return operation()
	}()

	if wasRunning {
		if restartErr := Start(townRoot); restartErr != nil {
			if operationErr != nil {
				return fmt.Errorf("database move failed: %w; restarting Dolt server failed: %v", operationErr, restartErr)
			}
			return fmt.Errorf("restarting Dolt server after database move: %w", restartErr)
		}
	}
	return operationErr
}

func stopLocked(townRoot string) error {
	config := DefaultConfig(townRoot)

	running, pid, err := IsRunning(townRoot)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("Dolt server is not running")
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process: %w", err)
	}

	// Drain active connections before stopping to reduce the nbs_manifest
	// race window inside Dolt's NomsBlockStore.Close(). Non-fatal: proceeds even
	// if drain times out (10s max). Skipped for remote servers (no local PID).
	if !config.IsRemote() {
		drainConnectionsBeforeStop(config)
	}

	// Send termination signal for graceful shutdown (SIGTERM on Unix, Kill on Windows)
	if err := gracefulTerminate(process); err != nil {
		return fmt.Errorf("sending termination signal: %w", err)
	}

	// Wait for graceful shutdown (dolt needs more time)
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if !processIsAlive(pid) {
			break
		}
	}

	// Check if still running
	if processIsAlive(pid) {
		// Still running, force kill
		_ = process.Kill()
		time.Sleep(100 * time.Millisecond)
	}

	// Clean up PID file
	_ = os.Remove(config.PidFile)

	// Update state - preserve historical info
	state, _ := LoadState(townRoot)
	if state == nil {
		state = &State{}
	}
	state.Running = false
	state.PID = 0
	_ = SaveState(townRoot, state)

	return nil
}

// GetConnectionString returns the MySQL connection string for the server.
// Use GetConnectionStringForRig for a specific database.
func GetConnectionString(townRoot string) string {
	config := DefaultConfig(townRoot)
	return fmt.Sprintf("%s@tcp(%s)/", config.displayDSN(), config.HostPort())
}

// GetConnectionStringForRig returns the MySQL connection string for a specific rig database.
func GetConnectionStringForRig(townRoot, rigName string) string {
	config := DefaultConfig(townRoot)
	return fmt.Sprintf("%s@tcp(%s)/%s", config.displayDSN(), config.HostPort(), rigName)
}

// displayDSN returns the user[:password] portion for display, masking any password.
func (c *Config) displayDSN() string {
	if c.Password != "" {
		return c.User + ":****"
	}
	return c.User
}

// dbCache deduplicates and caches SHOW DATABASES results to prevent the
// "thundering herd" problem where multiple concurrent callers each spawn a
// dolt sql subprocess. See GH#2180.
var dbCache = struct {
	mu       sync.Mutex
	result   []string
	err      error
	updated  time.Time
	inflight chan struct{} // non-nil when a fetch is in progress
}{} //nolint:gochecknoglobals // process-level cache, intentional

const dbCacheTTL = 30 * time.Second

// InvalidateDBCache clears the cached ListDatabases result, forcing the next
// call to re-query. Use after operations that change the database set (e.g.,
// CREATE DATABASE, DROP DATABASE, InitRig).
func InvalidateDBCache() {
	dbCache.mu.Lock()
	dbCache.result = nil
	dbCache.err = nil
	dbCache.updated = time.Time{}
	dbCache.mu.Unlock()
}

// ListDatabases returns the list of available rig databases.
// For local servers, scans the data directory on disk.
// For remote servers, queries SHOW DATABASES via SQL.
//
// Results are cached for 30 seconds and concurrent callers share a single
// in-flight query to avoid overwhelming the Dolt server (GH#2180).
func ListDatabases(townRoot string) ([]string, error) {
	config := DefaultConfig(townRoot)

	if config.IsRemote() {
		return listDatabasesCached(config)
	}

	return listDatabasesLocal(config)
}

// listDatabasesLocal scans the filesystem for valid Dolt database directories.
func listDatabasesLocal(config *Config) ([]string, error) {
	entries, err := os.ReadDir(config.DataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var databases []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Check if this directory is a valid Dolt database.
		// A phantom/corrupted .dolt/ dir (e.g., from DROP + catalog re-materialization)
		// will have .dolt/ but no noms/manifest. Loading such a dir crashes the server.
		doltDir := filepath.Join(config.DataDir, entry.Name(), ".dolt")
		if _, err := os.Stat(doltDir); err != nil {
			continue
		}
		manifest := filepath.Join(doltDir, "noms", "manifest")
		if _, err := os.Stat(manifest); err != nil {
			// .dolt/ exists but no noms/manifest — corrupted/phantom database
			fmt.Fprintf(os.Stderr, "Warning: skipping corrupted database %q (missing noms/manifest)\n", entry.Name())
			continue
		}
		databases = append(databases, entry.Name())
	}

	return databases, nil
}

// listDatabasesCached returns cached SHOW DATABASES results for remote servers,
// deduplicating concurrent queries via a shared in-flight channel.
func listDatabasesCached(config *Config) ([]string, error) {
	dbCache.mu.Lock()

	// Return cached result if fresh.
	if dbCache.result != nil && time.Since(dbCache.updated) < dbCacheTTL {
		result := make([]string, len(dbCache.result))
		copy(result, dbCache.result)
		dbCache.mu.Unlock()
		return result, nil
	}

	// If another goroutine is already fetching, wait for it.
	if dbCache.inflight != nil {
		ch := dbCache.inflight
		dbCache.mu.Unlock()
		<-ch
		// Re-read the result the fetcher stored.
		dbCache.mu.Lock()
		result := make([]string, len(dbCache.result))
		copy(result, dbCache.result)
		err := dbCache.err
		dbCache.mu.Unlock()
		return result, err
	}

	// We're the fetcher. Mark in-flight.
	ch := make(chan struct{})
	dbCache.inflight = ch
	dbCache.mu.Unlock()

	// Execute the actual query.
	result, err := listDatabasesRemote(config)

	// Store result and wake waiters.
	dbCache.mu.Lock()
	dbCache.result = result
	dbCache.err = err
	if err == nil {
		dbCache.updated = time.Now()
	}
	dbCache.inflight = nil
	dbCache.mu.Unlock()
	close(ch)

	if err != nil {
		return nil, err
	}
	out := make([]string, len(result))
	copy(out, result)
	return out, nil
}

// listDatabasesRemote queries SHOW DATABASES on a remote Dolt server.
func listDatabasesRemote(config *Config) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// SHOW DATABASES is catalog-scoped; must query the running server's in-memory
	// catalog, not dolt's embedded-mode filesystem view (see #3518 for precedent).
	cmd := buildServerSQLCmd(ctx, config, "-r", "json", "-q", "SHOW DATABASES")

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("querying remote SHOW DATABASES: %w (stderr: %s)", err, strings.TrimSpace(stderrBuf.String()))
	}

	var result struct {
		Rows []struct {
			Database string `json:"Database"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("parsing SHOW DATABASES JSON: %w", err)
	}

	var databases []string
	for _, row := range result.Rows {
		db := row.Database
		if !IsSystemDatabase(db) {
			databases = append(databases, db)
		}
	}
	return databases, nil
}

// VerifyDatabases queries the running Dolt SQL server for SHOW DATABASES and
// compares the result against the filesystem-discovered databases from
// ListDatabases. Returns the list of databases the server is actually serving
// and any that exist on disk but are missing from the server.
//
// This catches the silent failure mode where Dolt skips databases with stale
// manifests after migration — the filesystem says they exist, but the server
// doesn't serve them.
func VerifyDatabases(townRoot string) (served, missing []string, err error) {
	return verifyDatabasesWithRetry(townRoot, 1)
}

// VerifyExpectedDatabasesAtConfig queries SHOW DATABASES on the exact server
// described by config and reports which expected database names are missing.
// Unlike VerifyDatabases, this helper does not inspect the filesystem; it is
// intended for health checks that must validate a specific server address from
// metadata rather than the town's default local Dolt config.
func VerifyExpectedDatabasesAtConfig(config *Config, expected []string) (served, missing []string, err error) {
	const baseBackoff = 1 * time.Second
	const maxBackoff = 8 * time.Second
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := buildServerSQLCmd(ctx, config, "-r", "json", "-q", "SHOW DATABASES")
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		output, queryErr := cmd.Output()
		cancel()
		if queryErr != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			errDetail := strings.TrimSpace(string(output))
			if stderrMsg != "" {
				errDetail = errDetail + " (stderr: " + stderrMsg + ")"
			}
			lastErr = fmt.Errorf("querying SHOW DATABASES: %w (output: %s)", queryErr, errDetail)
			if attempt < 3 {
				backoff := baseBackoff
				for i := 1; i < attempt; i++ {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
						break
					}
				}
				time.Sleep(backoff)
			}
			continue
		}

		served, err = parseShowDatabases(output)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing SHOW DATABASES output: %w", err)
		}

		missing = findMissingDatabases(served, expected)
		return served, missing, nil
	}

	return nil, nil, lastErr
}

// VerifyDatabasesWithRetry is like VerifyDatabases but retries the SHOW DATABASES
// query with exponential backoff to handle the case where the server has just started
// and is still loading databases.
func VerifyDatabasesWithRetry(townRoot string, maxAttempts int) (served, missing []string, err error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return verifyDatabasesWithRetry(townRoot, maxAttempts)
}

func verifyDatabasesWithRetry(townRoot string, maxAttempts int) (served, missing []string, err error) {
	config := DefaultConfig(townRoot)

	// Retry with backoff since the server may still be loading databases
	// after a recent start (Start() only waits 500ms + process-alive check).
	// Both reachability and query are inside the loop so transient startup
	// failures are retried.
	const baseBackoff = 1 * time.Second
	const maxBackoff = 8 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Check if the server is reachable (TCP-level).
		if reachErr := CheckServerReachable(townRoot); reachErr != nil {
			lastErr = fmt.Errorf("server not reachable: %w", reachErr)
			if attempt < maxAttempts {
				backoff := baseBackoff
				for i := 1; i < attempt; i++ {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
						break
					}
				}
				time.Sleep(backoff)
			}
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		// SHOW DATABASES is catalog-scoped; embedded mode sees the on-disk catalog
		// rather than the running server's, which is the exact bug #3518/#3641 fix.
		cmd := buildServerSQLCmd(ctx, config,
			"-r", "json",
			"-q", "SHOW DATABASES",
		)

		// Capture stderr separately so it doesn't corrupt JSON parsing.
		// Dolt commonly writes deprecation/manifest warnings to stderr.
		// See also daemon/dolt.go:listDatabases() which uses cmd.Output()
		// for the same reason.
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		output, queryErr := cmd.Output()
		cancel()
		if queryErr != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			errDetail := strings.TrimSpace(string(output))
			if stderrMsg != "" {
				errDetail = errDetail + " (stderr: " + stderrMsg + ")"
			}
			lastErr = fmt.Errorf("querying SHOW DATABASES: %w (output: %s)", queryErr, errDetail)
			if attempt < maxAttempts {
				backoff := baseBackoff
				for i := 1; i < attempt; i++ {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
						break
					}
				}
				time.Sleep(backoff)
			}
			continue
		}

		var parseErr error
		served, parseErr = parseShowDatabases(output)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parsing SHOW DATABASES output: %w", parseErr)
		}

		// Compare against filesystem databases.
		fsDatabases, fsErr := ListDatabases(townRoot)
		if fsErr != nil {
			return served, nil, fmt.Errorf("listing filesystem databases: %w", fsErr)
		}

		missing = findMissingDatabases(served, fsDatabases)
		return served, missing, nil
	}
	return nil, nil, lastErr
}

// systemDatabases is the set of Dolt/MySQL internal databases that should be
// filtered from SHOW DATABASES results. These are not user rig databases:
//   - information_schema: MySQL standard metadata
//   - mysql: MySQL system database (privileges, users)
//   - dolt_cluster: Dolt clustering internal database (present when clustering is configured)
var systemDatabases = map[string]bool{
	"information_schema": true,
	"mysql":              true,
	"dolt_cluster":       true,
}

// IsSystemDatabase returns true if the given database name is a Dolt/MySQL
// internal database that should be excluded from user-facing database lists.
func IsSystemDatabase(name string) bool {
	return systemDatabases[strings.ToLower(name)]
}

// parseShowDatabases parses the output of SHOW DATABASES from dolt sql.
// It tries JSON parsing first, falling back to line-based parsing for
// plain-text output. Returns an error if the output format is unrecognized.
// Filters out system databases (information_schema, mysql, dolt_cluster).
func parseShowDatabases(output []byte) ([]string, error) {
	// Try JSON first. Use a raw map to detect schema presence.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(output, &raw); err != nil {
		// Check if the output looks like JSON that failed to parse —
		// don't fall through to line parsing with JSON-shaped text.
		trimmed := strings.TrimSpace(string(output))
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			return nil, fmt.Errorf("output looks like JSON but failed to parse: %w", err)
		}

		// Fall back to line parsing for plain-text output.
		var databases []string
		for _, line := range strings.Split(string(output), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && line != "Database" && !strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "|") {
				if !IsSystemDatabase(line) {
					databases = append(databases, line)
				}
			}
		}
		if len(databases) == 0 && len(trimmed) > 0 {
			return nil, fmt.Errorf("fallback parser returned zero databases from non-empty output (%d bytes); format may be unrecognized", len(trimmed))
		}
		return databases, nil
	}

	// JSON parsed — require the expected "rows" key.
	rowsRaw, hasRows := raw["rows"]
	if !hasRows {
		return nil, fmt.Errorf("JSON output missing expected 'rows' key (keys: %v); Dolt output schema may have changed", jsonKeys(raw))
	}

	var rows []struct {
		Database string `json:"Database"`
	}
	if err := json.Unmarshal(rowsRaw, &rows); err != nil {
		return nil, fmt.Errorf("JSON 'rows' field has unexpected type: %w", err)
	}

	var databases []string
	for _, row := range rows {
		if row.Database != "" && !IsSystemDatabase(row.Database) {
			databases = append(databases, row.Database)
		}
	}
	return databases, nil
}

// findMissingDatabases returns filesystem databases not present in the served list.
// Comparison is case-insensitive since Dolt database names are case-insensitive
// in SQL but case-preserving on the filesystem.
func findMissingDatabases(served, fsDatabases []string) []string {
	servedSet := make(map[string]bool, len(served))
	for _, db := range served {
		servedSet[strings.ToLower(db)] = true
	}
	var missing []string
	for _, db := range fsDatabases {
		if !servedSet[strings.ToLower(db)] {
			missing = append(missing, db)
		}
	}
	return missing
}

// jsonKeys returns the top-level keys from a JSON object map, for diagnostics.
func jsonKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// InitRig initializes a new rig database in the data directory.
// If the Dolt server is running, it executes CREATE DATABASE to register the
// database with the live server (avoiding the need for a restart).
// Returns (serverWasRunning, created, err). created is false when the database
// already existed on disk (idempotent no-op).
func InitRig(townRoot, rigName string) (serverWasRunning bool, created bool, err error) {
	serverWasRunning, created, _, err = InitRigOwned(townRoot, rigName)
	return serverWasRunning, created, err
}

// InitRigOwned also returns the stable root identity of a database it created.
// Callers use that identity for exact compensating cleanup after later failure.
func InitRigOwned(townRoot, rigName string) (serverWasRunning bool, created bool, rootIdentity string, err error) {
	if rigName == "" {
		return false, false, "", fmt.Errorf("rig name cannot be empty")
	}

	config := DefaultConfig(townRoot)

	// Validate rig name (simple alphanumeric + underscore/dash)
	for _, r := range rigName {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false, false, "", fmt.Errorf("invalid rig name %q: must contain only alphanumeric, underscore, or dash", rigName)
		}
	}

	err = withDoltLifecycle(townRoot, func() error {
		running, runningPID, runningErr := IsRunning(townRoot)
		if runningErr != nil {
			return fmt.Errorf("checking Dolt server before rig initialization: %w", runningErr)
		}
		if running {
			if _, statErr := os.Stat(config.DataDir); os.IsNotExist(statErr) {
				fmt.Fprintf(os.Stderr, "Warning: Dolt server (PID %d) is running but data directory %s does not exist — stopping orphaned server\n", runningPID, config.DataDir)
				if stopErr := stopLocked(townRoot); stopErr != nil {
					return fmt.Errorf("stopping orphaned Dolt server: %w", stopErr)
				}
				running = false
			}
		}
		serverWasRunning = running

		if err := WithDatabaseOwnershipTransaction(townRoot, func() error {
			rigDir := filepath.Join(config.DataDir, rigName)
			if _, statErr := os.Stat(filepath.Join(rigDir, ".dolt")); statErr == nil {
				created = false
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("checking existing rig database: %w", statErr)
			} else {
				created = true
				if serverWasRunning {
					if err := serverExecSQL(townRoot, fmt.Sprintf("CREATE DATABASE `%s`", rigName)); err != nil {
						return fmt.Errorf("creating database on running server: %w", err)
					}
					if err := waitForCatalog(townRoot, rigName); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: catalog visibility wait timed out (will retry on use): %v\n", err)
					}
				} else {
					if err := os.MkdirAll(rigDir, 0o755); err != nil {
						return fmt.Errorf("creating rig directory: %w", err)
					}
					cmd := exec.Command("dolt", "init")
					cmd.Dir = rigDir
					setProcessGroup(cmd)
					output, err := cmd.CombinedOutput()
					if err != nil {
						return fmt.Errorf("initializing Dolt database: %w\n%s", err, output)
					}
				}
				InvalidateDBCache()
			}

			beadsDir, err := FindOrCreateRigBeadsDir(townRoot, rigName)
			if err != nil {
				return fmt.Errorf("resolving beads directory: %w", err)
			}
			if err := ensureMetadataForBeadsDirLocked(townRoot, beadsDir, rigName); err != nil {
				return fmt.Errorf("publishing database ownership: %w", err)
			}
			if created {
				incarnation, err := databaseCleanupIncarnation(townRoot, rigName, rigDir, serverWasRunning)
				if err != nil {
					return fmt.Errorf("capturing created database identity: %w", err)
				}
				rootIdentity = databaseCleanupRootIdentity(incarnation)
			}
			return nil
		}); err != nil {
			return err
		}
		if serverWasRunning {
			return EnsureRigIssuePrefix(townRoot, rigName, true)
		}
		return nil
	})
	if err != nil {
		return serverWasRunning, created, rootIdentity, err
	}
	if !serverWasRunning {
		if err := EnsureRigIssuePrefix(townRoot, rigName, false); err != nil {
			return serverWasRunning, created, rootIdentity, fmt.Errorf("ensuring issue_prefix for database %q: %w", rigName, err)
		}
	}

	return serverWasRunning, created, rootIdentity, nil
}

// EnsureRigIssuePrefix initializes the beads schema for a rig database and
// persists config.issue_prefix. This covers direct `gt dolt init-rig` usage,
// where no later InitBeads call exists to run bd init/config repair.
func EnsureRigIssuePrefix(townRoot, rigName string, serverMode bool) error {
	if townRoot == "" {
		return fmt.Errorf("townRoot cannot be empty")
	}
	if rigName == "" {
		return fmt.Errorf("rig name cannot be empty")
	}

	prefix := issuePrefixForRigInit(townRoot, rigName)
	beadsDir, err := FindOrCreateRigBeadsDir(townRoot, rigName)
	if err != nil {
		return err
	}
	if err := beads.EnsureConfigYAML(beadsDir, prefix); err != nil {
		return fmt.Errorf("ensuring config.yaml: %w", err)
	}
	if err := beads.EnsureConfigYAMLValue(beadsDir, "types.custom", constants.BeadsCustomTypes); err != nil {
		return fmt.Errorf("ensuring types.custom in config.yaml: %w", err)
	}
	if err := beads.EnsureConfigYAMLValue(beadsDir, "types.infra", constants.BeadsInfraTypes); err != nil {
		return fmt.Errorf("ensuring types.infra in config.yaml: %w", err)
	}
	if err := EnsureMetadataForBeadsDir(townRoot, beadsDir, rigName, rigName); err != nil {
		return fmt.Errorf("ensuring metadata.json: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !serverMode {
		var started doltServerIncarnation
		if err := start(townRoot, func(incarnation doltServerIncarnation) { started = incarnation }); err != nil {
			return fmt.Errorf("starting temporary Dolt server: %w", err)
		}
		defer func() {
			if err := stopDoltIncarnation(townRoot, started); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not stop temporary Dolt server after issue_prefix seed: %v\n", err)
			}
		}()
	}

	store, err := openRigStoreFromConfig(ctx, townRoot, beadsDir, rigName)
	if err != nil {
		return fmt.Errorf("opening beads database: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		return fmt.Errorf("setting issue_prefix: %w", err)
	}
	return nil
}

var beadsOpenEnvMu sync.Mutex

func openRigStoreFromConfig(ctx context.Context, townRoot, beadsDir, rigName string) (beadssdk.Storage, error) {
	// bd's public config loader lets BEADS_DOLT_* env override metadata.json.
	// Polecat/rig processes often carry those env vars for their current database,
	// so scope them to the database/server being initialized here.
	beadsOpenEnvMu.Lock()
	defer beadsOpenEnvMu.Unlock()

	gtConfig := DefaultConfig(townRoot)
	overrides := map[string]string{
		"BEADS_DOLT_SERVER_DATABASE": rigName,
		"BEADS_DOLT_SERVER_HOST":     gtConfig.EffectiveHost(),
		"BEADS_DOLT_SERVER_PORT":     strconv.Itoa(gtConfig.Port),
		"BEADS_DOLT_PORT":            strconv.Itoa(gtConfig.Port),
	}
	type oldEnv struct {
		value string
		had   bool
	}
	old := make(map[string]oldEnv, len(overrides))
	for key, value := range overrides {
		oldValue, had := os.LookupEnv(key)
		old[key] = oldEnv{value: oldValue, had: had}
		if err := os.Setenv(key, value); err != nil {
			return nil, err
		}
	}
	defer func() {
		for key, oldValue := range old {
			if oldValue.had {
				_ = os.Setenv(key, oldValue.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}()

	return beadssdk.OpenFromConfig(ctx, beadsDir)
}

func issuePrefixForRigInit(townRoot, rigName string) string {
	beadsDir := filepath.Join(townRoot, ".beads")
	if routes, err := beads.LoadRoutes(beadsDir); err == nil {
		for _, route := range routes {
			parts := strings.SplitN(route.Path, string(filepath.Separator), 2)
			if len(parts) == 0 || parts[0] != rigName {
				parts = strings.SplitN(route.Path, "/", 2)
			}
			if len(parts) > 0 && parts[0] == rigName {
				if prefix := strings.TrimSpace(strings.TrimSuffix(route.Prefix, "-")); prefix != "" {
					return prefix
				}
			}
		}
	}

	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if rigsConfig, err := configpkg.LoadRigsConfig(rigsConfigPath); err == nil {
		if entry, ok := rigsConfig.Rigs[rigName]; ok && entry.BeadsConfig != nil {
			if prefix := strings.TrimSpace(strings.TrimSuffix(entry.BeadsConfig.Prefix, "-")); prefix != "" {
				return prefix
			}
		}
	}

	return strings.TrimSuffix(rigName, "-")
}

// Migration represents a database migration from old to new location.
type Migration struct {
	RigName    string
	SourcePath string
	TargetPath string
}

const (
	databaseMigrationReceiptVersion   = 2
	databaseMigrationStageName        = ".migration-stage"
	databaseMigrationCleanupClaimName = ".gastown-migration-claim"
)

type databaseMigrationPhase string

const (
	databaseMigrationPrepared        databaseMigrationPhase = "prepared"
	databaseMigrationStaged          databaseMigrationPhase = "staged"
	databaseMigrationTargetReady     databaseMigrationPhase = "target-ready"
	databaseMigrationCleanupReady    databaseMigrationPhase = "cleanup-ready"
	databaseMigrationCleanupRemoving databaseMigrationPhase = "cleanup-removing"
	databaseMigrationCleanupEmpty    databaseMigrationPhase = "cleanup-empty"
	databaseMigrationSourceCleaned   databaseMigrationPhase = "source-cleaned"
)

type databaseMigrationReceipt struct {
	Version       int                    `json:"version"`
	RigName       string                 `json:"rig_name"`
	SourcePath    string                 `json:"source_path"`
	CleanupPath   string                 `json:"cleanup_path"`
	TargetPath    string                 `json:"target_path"`
	StagePath     string                 `json:"stage_path"`
	SourceDigest  string                 `json:"source_digest"`
	CleanupToken  string                 `json:"cleanup_token,omitempty"`
	TargetToken   string                 `json:"target_token"`
	LegacyUpgrade bool                   `json:"legacy_upgrade,omitempty"`
	Phase         databaseMigrationPhase `json:"phase"`
}

var (
	databaseMigrationBeforeTargetMutation       = func() {}
	databaseMigrationBeforeIrreversibleMutation = func() {}
	databaseMigrationBeforeCleanupRemoval       = func() {}
	databaseMigrationAfterCleanupRevalidation   = func() {}
	databaseMigrationBeforeCleanupEmptyMove     = func() {}
	databaseMigrationAfterSourceClaimValidation = func() {}
	databaseMigrationBeforeCleanupChildRemoval  = func(string) error { return nil }
	databaseMigrationBeforeClaimPublication     = func() {}
	databaseMigrationAfterClaimTokenRead        = func() {}
)

func databaseMigrationReceiptPath(townRoot, rigName string) string {
	return filepath.Join(townRoot, ".runtime", "dolt-database-migrations", canonicalDatabaseName(rigName)+".json")
}

func writeDatabaseMigrationReceipt(path string, receipt databaseMigrationReceipt) error {
	if err := ensurePrivateDatabaseCleanupDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return writeDatabaseCleanupFileDurable(path, append(data, '\n'), 0o600)
}

func readDatabaseMigrationReceipt(townRoot, path string) (databaseMigrationReceipt, error) {
	f, err := os.Open(path)
	if err != nil {
		return databaseMigrationReceipt{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return databaseMigrationReceipt{}, fmt.Errorf("database migration receipt is not a private regular file")
	}
	var receipt databaseMigrationReceipt
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&receipt); err != nil {
		return databaseMigrationReceipt{}, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return databaseMigrationReceipt{}, fmt.Errorf("database migration receipt has trailing data")
	}
	if err := validateDatabaseMigrationReceipt(townRoot, path, receipt); err != nil {
		return databaseMigrationReceipt{}, err
	}
	return receipt, nil
}

func validateDatabaseMigrationReceipt(townRoot, receiptPath string, receipt databaseMigrationReceipt) error {
	if (receipt.Version != 1 && receipt.Version != databaseMigrationReceiptVersion) || !validSQLName(receipt.RigName) || receipt.RigName == "." || receipt.RigName == ".." {
		return fmt.Errorf("invalid database migration receipt")
	}
	validPhase := receipt.Phase == databaseMigrationPrepared || receipt.Phase == databaseMigrationStaged || receipt.Phase == databaseMigrationTargetReady || receipt.Phase == databaseMigrationCleanupReady || receipt.Phase == databaseMigrationCleanupRemoving || receipt.Phase == databaseMigrationCleanupEmpty || receipt.Phase == databaseMigrationSourceCleaned
	if receipt.Version == 1 {
		validPhase = receipt.Phase == databaseMigrationPrepared || receipt.Phase == databaseMigrationStaged || receipt.Phase == databaseMigrationTargetReady || receipt.Phase == databaseMigrationCleanupRemoving
	}
	if !validPhase {
		return fmt.Errorf("invalid database migration receipt phase %q", receipt.Phase)
	}
	digest, err := hex.DecodeString(receipt.SourceDigest)
	if err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("invalid database migration source digest")
	}
	if receipt.CleanupToken != "" {
		token, err := hex.DecodeString(receipt.CleanupToken)
		if err != nil || len(token) != sha256.Size {
			return fmt.Errorf("invalid database migration cleanup token")
		}
	}
	if receipt.Version == databaseMigrationReceiptVersion {
		targetToken, err := hex.DecodeString(receipt.TargetToken)
		if err != nil || len(targetToken) != sha256.Size {
			return fmt.Errorf("invalid database migration target token")
		}
		if (receipt.Phase == databaseMigrationCleanupReady || receipt.Phase == databaseMigrationCleanupRemoving || receipt.Phase == databaseMigrationCleanupEmpty) && receipt.CleanupToken == "" {
			return fmt.Errorf("database migration cleanup phase has no claim token")
		}
	} else if receipt.TargetToken != "" || receipt.CleanupToken != "" || receipt.LegacyUpgrade {
		return fmt.Errorf("legacy database migration receipt has unexpected claim tokens")
	}
	townAbs, err := filepath.Abs(townRoot)
	if err != nil {
		return err
	}
	sourceAbs, err := filepath.Abs(receipt.SourcePath)
	if err != nil || sourceAbs != receipt.SourcePath || !pathIsWithin(townAbs, sourceAbs) {
		return fmt.Errorf("database migration source is outside the town root")
	}
	targetPath, err := DatabasePath(townRoot, receipt.RigName)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(targetPath)
	if err != nil || targetAbs != receipt.TargetPath {
		return fmt.Errorf("database migration target does not match rig %q", receipt.RigName)
	}
	dataDirAbs, err := filepath.Abs(DefaultConfig(townRoot).DataDir)
	if err != nil {
		return err
	}
	if pathIsWithin(dataDirAbs, sourceAbs) {
		return fmt.Errorf("database migration source is inside the shared data directory")
	}
	legacyCleanupPath := receipt.SourcePath + ".migration-cleanup"
	tokenCleanupPath := receipt.SourcePath + ".migration-cleanup-" + receipt.TargetToken
	cleanupPathValid := receipt.CleanupPath == legacyCleanupPath
	if receipt.Version == databaseMigrationReceiptVersion {
		cleanupPathValid = cleanupPathValid || receipt.CleanupPath == tokenCleanupPath
	}
	if !cleanupPathValid || !pathIsWithin(townAbs, receipt.CleanupPath) {
		return fmt.Errorf("database migration cleanup path does not match source")
	}
	if receipt.StagePath != filepath.Join(targetAbs, databaseMigrationStageName) {
		return fmt.Errorf("database migration stage does not match target")
	}
	if filepath.Clean(receiptPath) != filepath.Clean(databaseMigrationReceiptPath(townRoot, receipt.RigName)) {
		return fmt.Errorf("database migration receipt path does not match rig %q", receipt.RigName)
	}
	return nil
}

func pathIsWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func databaseMigrationRootRelativePath(townRoot, path string) (string, error) {
	townAbs, err := filepath.Abs(townRoot)
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil || !pathIsWithin(townAbs, pathAbs) {
		return "", fmt.Errorf("database migration path is outside the town root")
	}
	return filepath.Rel(townAbs, pathAbs)
}

func validateDatabaseMigrationPathCustody(root *os.Root, rel string) error {
	current := ""
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("database migration path contains symbolic link at %s", current)
		}
	}
	return nil
}

func databaseMigrationRootPathState(root *os.Root, rel string) (exists, complete bool, err error) {
	info, err := root.Lstat(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return true, false, fmt.Errorf("migration path %s is not a real directory", rel)
	}
	info, err = root.Lstat(filepath.Join(rel, ".dolt"))
	if err != nil {
		if os.IsNotExist(err) {
			return true, false, nil
		}
		return true, false, err
	}
	return true, info.IsDir() && info.Mode()&os.ModeSymlink == 0, nil
}

func loadDatabaseMigrationReceipts(townRoot string) ([]databaseMigrationReceipt, error) {
	dir := filepath.Join(townRoot, ".runtime", "dolt-database-migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	receipts := make([]databaseMigrationReceipt, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		receipt, err := readDatabaseMigrationReceipt(townRoot, filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading pending database migration %s: %w", entry.Name(), err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func databaseMigrationTreeDigest(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("database migration source is not a real directory")
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer rootHandle.Close()
	return databaseMigrationTreeDigestFromRoot(rootHandle, true)
}

func databaseMigrationTreeDigestFromRoot(root *os.Root, ignoreCleanupClaim bool) (string, error) {
	hash := sha256.New()
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		if ignoreCleanupClaim && path == databaseMigrationCleanupClaimName {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", path, info.Mode()&os.ModeType)
		switch {
		case info.Mode().IsRegular():
			fmt.Fprintf(hash, "%d\x00", info.Size())
			file, err := root.Open(filepath.FromSlash(path))
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			return errors.Join(copyErr, closeErr)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := root.Readlink(filepath.FromSlash(path))
			if err != nil {
				return err
			}
			_, err = io.WriteString(hash, target)
			return err
		case info.IsDir():
			return nil
		default:
			return fmt.Errorf("unsupported database migration entry %s", filepath.Join(root.Name(), filepath.FromSlash(path)))
		}
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// findLocalDoltDB scans beadsDir/dolt/ for a subdirectory containing a .dolt
// directory (an embedded Dolt database). Returns the full path to the database
// directory, or "" if none found.
//
// bd names the subdirectory based on internal conventions (e.g., beads_hq,
// beads_gt) that have changed across versions. Scanning avoids hardcoding
// assumptions about the naming scheme.
//
// If multiple databases are found, returns "" and logs a warning to stderr.
// Callers should not silently pick one — ambiguity requires manual resolution.
func findLocalDoltDB(beadsDir string) string {
	doltParent := filepath.Join(beadsDir, "dolt")
	entries, err := os.ReadDir(doltParent)
	if err != nil {
		return ""
	}
	var candidates []string
	for _, e := range entries {
		// Resolve symlinks: DirEntry.IsDir() returns false for symlinks-to-directories
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(filepath.Join(doltParent, e.Name()))
			if err != nil {
				continue
			}
			fi, err := os.Stat(resolved)
			if err != nil || !fi.IsDir() {
				continue
			}
		} else if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(doltParent, e.Name())
		if _, err := os.Stat(filepath.Join(candidate, ".dolt")); err == nil {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		if len(entries) > 0 {
			fmt.Fprintf(os.Stderr, "[doltserver] Warning: %s exists but contains no valid dolt database\n", doltParent)
		}
		return ""
	}
	if len(candidates) > 1 {
		fmt.Fprintf(os.Stderr, "[doltserver] Warning: multiple dolt databases found in %s: %v — manual resolution required\n", doltParent, candidates)
		return ""
	}
	return candidates[0]
}

// FindMigratableDatabases finds existing dolt databases that can be migrated.
func FindMigratableDatabases(townRoot string) []Migration {
	migrations, err := FindMigratableDatabasesChecked(townRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[doltserver] Warning: finding migratable databases: %v\n", err)
	}
	return migrations
}

// FindMigratableDatabasesChecked includes durable in-progress migrations and
// reports receipt uncertainty instead of silently treating it as no work.
func FindMigratableDatabasesChecked(townRoot string) ([]Migration, error) {
	var migrations []Migration
	config := DefaultConfig(townRoot)
	seen := make(map[string]bool)
	receipts, err := loadDatabaseMigrationReceipts(townRoot)
	if err != nil {
		return nil, err
	}
	for _, receipt := range receipts {
		canonical := canonicalDatabaseName(receipt.RigName)
		seen[canonical] = true
		migrations = append(migrations, Migration{
			RigName:    receipt.RigName,
			SourcePath: receipt.SourcePath,
			TargetPath: receipt.TargetPath,
		})
	}

	// Check town-level beads database -> .dolt-data/hq
	townBeadsDir := beads.ResolveBeadsDir(townRoot)
	townSource := findLocalDoltDB(townBeadsDir)
	if townSource != "" && !seen["hq"] {
		// Check target doesn't already have data
		targetDir := filepath.Join(config.DataDir, "hq")
		if _, err := os.Stat(filepath.Join(targetDir, ".dolt")); os.IsNotExist(err) {
			migrations = append(migrations, Migration{
				RigName:    "hq",
				SourcePath: townSource,
				TargetPath: targetDir,
			})
		}
	}

	// Check rig-level beads databases
	// Look for directories in townRoot, following .beads/redirect if present
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return migrations, err
	}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		rigName := entry.Name()
		if seen[canonicalDatabaseName(rigName)] {
			continue
		}
		resolvedBeadsDir := beads.ResolveBeadsDir(filepath.Join(townRoot, rigName))
		rigSource := findLocalDoltDB(resolvedBeadsDir)

		if rigSource != "" {
			// Check target doesn't already have data
			targetDir := filepath.Join(config.DataDir, rigName)
			if _, err := os.Stat(filepath.Join(targetDir, ".dolt")); os.IsNotExist(err) {
				migrations = append(migrations, Migration{
					RigName:    rigName,
					SourcePath: rigSource,
					TargetPath: targetDir,
				})
			}
		}
	}

	return migrations, nil
}

// MigrateRigFromBeads migrates an existing beads Dolt database to the data directory.
// This is used to migrate from the old per-rig .beads/dolt/<db_name> layout to the new
// centralized .dolt-data/<rigname> layout.
func MigrateRigFromBeads(townRoot, rigName, sourcePath string) error {
	targetDir, err := DatabasePath(townRoot, rigName)
	if err != nil {
		return err
	}
	targetDir, err = filepath.Abs(targetDir)
	if err != nil {
		return err
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	receiptPath := databaseMigrationReceiptPath(townRoot, rigName)
	return WithStoppedDoltForDatabaseMove(townRoot, func() error {
		return WithDatabaseOwnershipTransaction(townRoot, func() error {
			receipt, readErr := readDatabaseMigrationReceipt(townRoot, receiptPath)
			if readErr != nil {
				if !os.IsNotExist(readErr) {
					return fmt.Errorf("reading database migration receipt: %w", readErr)
				}
				root, err := os.OpenRoot(townRoot)
				if err != nil {
					return err
				}
				defer root.Close()
				targetRel, err := databaseMigrationRootRelativePath(townRoot, targetDir)
				if err != nil {
					return err
				}
				sourceRel, err := databaseMigrationRootRelativePath(townRoot, sourcePath)
				if err != nil {
					return err
				}
				targetToken, err := newDatabaseMigrationCleanupToken()
				if err != nil {
					return fmt.Errorf("creating migration target token: %w", err)
				}
				cleanupPath := sourcePath + ".migration-cleanup-" + targetToken
				cleanupRel, err := databaseMigrationRootRelativePath(townRoot, cleanupPath)
				if err != nil {
					return err
				}
				for _, rel := range []string{targetRel, sourceRel, cleanupRel} {
					if err := validateDatabaseMigrationPathCustody(root, rel); err != nil {
						return err
					}
				}
				targetExists, _, err := databaseMigrationRootPathState(root, targetRel)
				if err != nil {
					return fmt.Errorf("checking target database: %w", err)
				}
				if targetExists {
					return fmt.Errorf("rig database %q already exists at %s", rigName, targetDir)
				}
				sourceExists, sourceComplete, err := databaseMigrationRootPathState(root, sourceRel)
				if err != nil {
					return fmt.Errorf("checking source database: %w", err)
				}
				if !sourceExists || !sourceComplete {
					return fmt.Errorf("checking source database: %s is not a complete Dolt database", sourcePath)
				}
				if _, err := root.Lstat(filepath.Join(sourceRel, databaseMigrationStageName)); err == nil {
					return fmt.Errorf("source database contains reserved migration stage entry %q", databaseMigrationStageName)
				} else if !os.IsNotExist(err) {
					return fmt.Errorf("checking reserved migration stage entry: %w", err)
				}
				if _, err := root.Lstat(filepath.Join(sourceRel, databaseMigrationCleanupClaimName)); err == nil {
					return fmt.Errorf("source database contains reserved migration cleanup entry %q", databaseMigrationCleanupClaimName)
				} else if !os.IsNotExist(err) {
					return fmt.Errorf("checking reserved migration cleanup entry: %w", err)
				}
				if cleanupExists, _, err := databaseMigrationRootPathState(root, cleanupRel); err != nil {
					return fmt.Errorf("checking migration cleanup path: %w", err)
				} else if cleanupExists {
					return fmt.Errorf("migration cleanup path already exists at %s", cleanupPath)
				}
				sourceRoot, err := root.OpenRoot(sourceRel)
				if err != nil {
					return fmt.Errorf("opening source database: %w", err)
				}
				digest, digestErr := databaseMigrationTreeDigestFromRoot(sourceRoot, false)
				closeErr := sourceRoot.Close()
				if err := errors.Join(digestErr, closeErr); err != nil {
					return fmt.Errorf("hashing source database: %w", err)
				}
				receipt = databaseMigrationReceipt{
					Version:      databaseMigrationReceiptVersion,
					RigName:      rigName,
					SourcePath:   sourcePath,
					CleanupPath:  cleanupPath,
					TargetPath:   targetDir,
					StagePath:    filepath.Join(targetDir, databaseMigrationStageName),
					SourceDigest: digest,
					TargetToken:  targetToken,
					Phase:        databaseMigrationPrepared,
				}
				if err := validateDatabaseMigrationReceipt(townRoot, receiptPath, receipt); err != nil {
					return err
				}
				if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
					return fmt.Errorf("writing database migration receipt: %w", err)
				}
			} else {
				receipt, err = upgradeDatabaseMigrationReceipt(townRoot, receiptPath, receipt)
				if err != nil {
					return fmt.Errorf("upgrading database migration receipt: %w", err)
				}
			}
			if receipt.RigName != rigName || receipt.SourcePath != sourcePath || receipt.TargetPath != targetDir {
				return fmt.Errorf("pending database migration does not match requested rig or source")
			}
			return resumeDatabaseMigrationLocked(townRoot, receiptPath, receipt)
		})
	})
}

func upgradeDatabaseMigrationReceipt(townRoot, receiptPath string, receipt databaseMigrationReceipt) (databaseMigrationReceipt, error) {
	legacyCleanupPath := receipt.SourcePath + ".migration-cleanup"
	if receipt.Version == databaseMigrationReceiptVersion && receipt.CleanupPath != legacyCleanupPath {
		return receipt, nil
	}
	wasVersionOne := receipt.Version == 1
	if wasVersionOne {
		token, err := newDatabaseMigrationCleanupToken()
		if err != nil {
			return receipt, err
		}
		receipt.Version = databaseMigrationReceiptVersion
		receipt.TargetToken = token
		if receipt.Phase == databaseMigrationCleanupRemoving {
			receipt.CleanupToken, err = newDatabaseMigrationCleanupToken()
			if err != nil {
				return receipt, err
			}
		}
		receipt.LegacyUpgrade = true
		if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
			return receipt, err
		}
	}

	root, err := os.OpenRoot(townRoot)
	if err != nil {
		return receipt, err
	}
	defer root.Close()
	targetRel, err := databaseMigrationRootRelativePath(townRoot, receipt.TargetPath)
	if err != nil {
		return receipt, err
	}
	stageRel, err := databaseMigrationRootRelativePath(townRoot, receipt.StagePath)
	if err != nil {
		return receipt, err
	}
	for _, rel := range []string{targetRel, stageRel} {
		if err := validateDatabaseMigrationPathCustody(root, rel); err != nil {
			return receipt, err
		}
	}
	allowLegacyClaim := wasVersionOne || receipt.LegacyUpgrade
	if err := ensureLegacyDatabaseMigrationTargetClaim(root, targetRel, stageRel, receipt, allowLegacyClaim); err != nil {
		return receipt, err
	}

	newCleanupPath := receipt.SourcePath + ".migration-cleanup-" + receipt.TargetToken
	oldCleanupRel, err := databaseMigrationRootRelativePath(townRoot, legacyCleanupPath)
	if err != nil {
		return receipt, err
	}
	newCleanupRel, err := databaseMigrationRootRelativePath(townRoot, newCleanupPath)
	if err != nil {
		return receipt, err
	}
	sourceRel, err := databaseMigrationRootRelativePath(townRoot, receipt.SourcePath)
	if err != nil {
		return receipt, err
	}
	for _, rel := range []string{sourceRel, targetRel, stageRel, oldCleanupRel, newCleanupRel} {
		if err := validateDatabaseMigrationPathCustody(root, rel); err != nil {
			return receipt, err
		}
	}
	oldExists, oldComplete, err := databaseMigrationRootPathState(root, oldCleanupRel)
	if err != nil {
		return receipt, err
	}
	newExists, _, err := databaseMigrationRootPathState(root, newCleanupRel)
	if err != nil {
		return receipt, err
	}
	if oldExists && newExists {
		return receipt, fmt.Errorf("legacy and token-bound migration cleanup claims both exist")
	}
	if oldExists || newExists {
		if receipt.CleanupToken == "" {
			token, err := newDatabaseMigrationCleanupToken()
			if err != nil {
				return receipt, err
			}
			receipt.CleanupToken = token
			if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
				return receipt, err
			}
		}
		claimRel := oldCleanupRel
		claimComplete := oldComplete
		if newExists {
			claimRel = newCleanupRel
		}
		claimPath := filepath.Join(claimRel, databaseMigrationCleanupClaimName)
		if _, err := root.Lstat(claimPath); os.IsNotExist(err) {
			partialLegacyRemoval := wasVersionOne && receipt.Phase == databaseMigrationCleanupRemoving
			if !claimComplete && !partialLegacyRemoval {
				return receipt, fmt.Errorf("legacy migration cleanup claim is incomplete")
			}
			if !allowLegacyClaim {
				return receipt, fmt.Errorf("migration cleanup claim has no durable identity")
			}
			if !partialLegacyRemoval {
				if err := verifyDatabaseMigrationDigestAt(root, claimRel, receipt.SourceDigest); err != nil {
					return receipt, err
				}
			}
			if err := writeDatabaseMigrationClaim(root, claimRel, receipt.CleanupToken); err != nil {
				return receipt, err
			}
		} else if err != nil {
			return receipt, err
		}
		if err := verifyDatabaseMigrationClaimToken(root, claimRel, receipt.CleanupToken); err != nil {
			return receipt, err
		}
		if receipt.Phase != databaseMigrationCleanupRemoving {
			if err := verifyDatabaseMigrationClaim(root, claimRel, receipt.SourceDigest, receipt.CleanupToken); err != nil {
				return receipt, err
			}
		}
		if oldExists {
			if err := moveClaimedDatabaseMigrationRoot(root, oldCleanupRel, newCleanupRel, receipt.SourceDigest, receipt.CleanupToken, receipt.Phase != databaseMigrationCleanupRemoving, "cleanup", nil); err != nil {
				return receipt, err
			}
		}
		if receipt.Phase == databaseMigrationTargetReady {
			receipt.Phase = databaseMigrationCleanupReady
		}
	} else {
		sourceExists, _, err := databaseMigrationRootPathState(root, sourceRel)
		if err != nil {
			return receipt, err
		}
		if !sourceExists {
			_, targetComplete, err := databaseMigrationRootPathState(root, targetRel)
			if err != nil {
				return receipt, err
			}
			if !targetComplete {
				return receipt, fmt.Errorf("legacy migration has neither source, cleanup claim, nor complete target")
			}
			switch receipt.Phase {
			case databaseMigrationCleanupRemoving, databaseMigrationSourceCleaned:
				receipt.Phase = databaseMigrationSourceCleaned
			case databaseMigrationTargetReady:
				if !allowLegacyClaim {
					return receipt, fmt.Errorf("verified migration cleanup claim is absent")
				}
				receipt.Phase = databaseMigrationSourceCleaned
			case databaseMigrationPrepared, databaseMigrationStaged:
				// A prior version could publish the complete, claimed target before
				// advancing the receipt. The normal resume loop verifies and advances it.
			default:
				return receipt, fmt.Errorf("verified migration cleanup claim is absent")
			}
		}
	}
	receipt.CleanupPath = newCleanupPath
	receipt.LegacyUpgrade = false
	if err := validateDatabaseMigrationReceipt(townRoot, receiptPath, receipt); err != nil {
		return receipt, err
	}
	if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func ensureLegacyDatabaseMigrationTargetClaim(root *os.Root, targetRel, stageRel string, receipt databaseMigrationReceipt, allowClaim bool) error {
	targetExists, targetComplete, err := databaseMigrationRootPathState(root, targetRel)
	if err != nil || !targetExists {
		return err
	}
	claimRel := filepath.Join(targetRel, databaseMigrationCleanupClaimName)
	if _, err := root.Lstat(claimRel); os.IsNotExist(err) {
		if !allowClaim {
			return fmt.Errorf("migration target claim is absent")
		}
		if targetComplete {
			if err := verifyDatabaseMigrationDigestAt(root, targetRel, receipt.SourceDigest); err != nil {
				return err
			}
		} else if err := validateLegacyDatabaseMigrationPartialTarget(root, targetRel, stageRel); err != nil {
			return err
		}
		if err := writeDatabaseMigrationClaim(root, targetRel, receipt.TargetToken); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if targetComplete {
		return verifyDatabaseMigrationClaim(root, targetRel, receipt.SourceDigest, receipt.TargetToken)
	}
	return verifyDatabaseMigrationClaimToken(root, targetRel, receipt.TargetToken)
}

func validateLegacyDatabaseMigrationPartialTarget(root *os.Root, targetRel, stageRel string) error {
	targetRoot, err := root.OpenRoot(targetRel)
	if err != nil {
		return err
	}
	defer targetRoot.Close()
	entries, err := fs.ReadDir(targetRoot.FS(), ".")
	if err != nil {
		return err
	}
	stageName := filepath.Base(stageRel)
	for _, entry := range entries {
		if entry.Name() != stageName {
			return fmt.Errorf("legacy migration partial target contains unowned entry %q", entry.Name())
		}
	}
	return nil
}

func resumeDatabaseMigrationLocked(townRoot, receiptPath string, receipt databaseMigrationReceipt) error {
	root, err := os.OpenRoot(townRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	sourceRel, err := databaseMigrationRootRelativePath(townRoot, receipt.SourcePath)
	if err != nil {
		return err
	}
	cleanupRel, err := databaseMigrationRootRelativePath(townRoot, receipt.CleanupPath)
	if err != nil {
		return err
	}
	targetRel, err := databaseMigrationRootRelativePath(townRoot, receipt.TargetPath)
	if err != nil {
		return err
	}
	stageRel, err := databaseMigrationRootRelativePath(townRoot, receipt.StagePath)
	if err != nil {
		return err
	}
	for {
		if err := validateDatabaseMigrationPathCustody(root, sourceRel); err != nil {
			return err
		}
		if err := validateDatabaseMigrationPathCustody(root, cleanupRel); err != nil {
			return err
		}
		if err := validateDatabaseMigrationPathCustody(root, targetRel); err != nil {
			return err
		}
		if err := validateDatabaseMigrationPathCustody(root, stageRel); err != nil {
			return err
		}
		sourceExists, sourceComplete, err := databaseMigrationRootPathState(root, sourceRel)
		if err != nil {
			return err
		}
		_, targetComplete, err := databaseMigrationRootPathState(root, targetRel)
		if err != nil {
			return err
		}

		switch receipt.Phase {
		case databaseMigrationPrepared:
			if targetComplete {
				if sourceComplete {
					return fmt.Errorf("ambiguous prepared migration: source and target both contain Dolt databases")
				}
				if err := removeEmptyDatabaseMigrationStage(root, targetRel, receipt.TargetToken); err != nil {
					return err
				}
				if err := verifyDatabaseMigrationClaim(root, targetRel, receipt.SourceDigest, receipt.TargetToken); err != nil {
					return fmt.Errorf("verifying migration target claim: %w", err)
				}
				receipt.Phase = databaseMigrationTargetReady
				if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
					return err
				}
				continue
			}
			if !sourceExists || !sourceComplete {
				return fmt.Errorf("prepared migration source is missing or incomplete")
			}
			if err := verifyDatabaseMigrationDigestAt(root, sourceRel, receipt.SourceDigest); err != nil {
				return err
			}
			if err := ensureDatabaseMigrationTargetClaim(root, targetRel, receipt.TargetToken); err != nil {
				return err
			}
			if err := copyDatabaseMigrationToStage(root, sourceRel, targetRel, receipt.SourceDigest, receipt.TargetToken); err != nil {
				return err
			}
			receipt.Phase = databaseMigrationStaged
			if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
				return err
			}

		case databaseMigrationStaged:
			if targetComplete {
				if err := removeEmptyDatabaseMigrationStage(root, targetRel, receipt.TargetToken); err != nil {
					return err
				}
				if err := verifyDatabaseMigrationClaim(root, targetRel, receipt.SourceDigest, receipt.TargetToken); err != nil {
					return fmt.Errorf("verifying migration target claim: %w", err)
				}
				receipt.Phase = databaseMigrationTargetReady
				if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
					return err
				}
				continue
			}
			_, stageComplete, err := databaseMigrationRootPathState(root, stageRel)
			if err != nil {
				return err
			}
			if !stageComplete {
				if !sourceComplete {
					return fmt.Errorf("staged migration has neither a complete stage nor source")
				}
				receipt.Phase = databaseMigrationPrepared
				if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
					return err
				}
				continue
			}
			if err := verifyDatabaseMigrationStage(root, targetRel, receipt.SourceDigest, receipt.TargetToken); err != nil {
				if !sourceComplete {
					return err
				}
				if sourceErr := verifyDatabaseMigrationDigestAt(root, sourceRel, receipt.SourceDigest); sourceErr != nil {
					return errors.Join(err, sourceErr)
				}
				receipt.Phase = databaseMigrationPrepared
				if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
					return err
				}
				continue
			}
			if err := promoteDatabaseMigrationStage(root, targetRel, receipt.SourceDigest, receipt.TargetToken); err != nil {
				return err
			}
			if err := verifyDatabaseMigrationClaim(root, targetRel, receipt.SourceDigest, receipt.TargetToken); err != nil {
				return fmt.Errorf("verifying migration target claim: %w", err)
			}
			receipt.Phase = databaseMigrationTargetReady
			if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
				return err
			}

		case databaseMigrationTargetReady:
			if !targetComplete {
				return fmt.Errorf("target-ready migration target is missing or incomplete")
			}
			if err := removeEmptyDatabaseMigrationStage(root, targetRel, receipt.TargetToken); err != nil {
				return err
			}
			if err := verifyDatabaseMigrationClaim(root, targetRel, receipt.SourceDigest, receipt.TargetToken); err != nil {
				return fmt.Errorf("verifying migration target claim: %w", err)
			}
			cleanupExists, cleanupComplete, err := databaseMigrationRootPathState(root, cleanupRel)
			if err != nil {
				return err
			}
			if sourceExists && cleanupExists {
				return fmt.Errorf("migration source and cleanup claim both exist")
			}
			if sourceExists {
				if !sourceComplete {
					return fmt.Errorf("migration source became incomplete before cleanup")
				}
				if err := verifyDatabaseMigrationDigestAt(root, sourceRel, receipt.SourceDigest); err != nil {
					return err
				}
				token, err := newDatabaseMigrationCleanupToken()
				if err != nil {
					return fmt.Errorf("creating migration cleanup token: %w", err)
				}
				receipt.CleanupToken = token
				receipt.Phase = databaseMigrationCleanupReady
				if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
					return err
				}
				continue
			}
			if cleanupExists {
				if !cleanupComplete {
					return fmt.Errorf("migration cleanup claim is incomplete")
				}
				return fmt.Errorf("unowned migration cleanup claim exists")
			}
			receipt.Phase = databaseMigrationSourceCleaned
			if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
				return err
			}

		case databaseMigrationCleanupReady:
			if !targetComplete {
				return fmt.Errorf("cleanup-ready migration target is missing or incomplete")
			}
			if err := verifyDatabaseMigrationClaim(root, targetRel, receipt.SourceDigest, receipt.TargetToken); err != nil {
				return fmt.Errorf("verifying migration target claim: %w", err)
			}
			cleanupExists, cleanupComplete, err := databaseMigrationRootPathState(root, cleanupRel)
			if err != nil {
				return err
			}
			if sourceExists && cleanupExists {
				return fmt.Errorf("migration source and cleanup claim both exist")
			}
			if sourceExists {
				if !sourceComplete {
					return fmt.Errorf("migration source became incomplete before cleanup")
				}
				claimRel := filepath.Join(sourceRel, databaseMigrationCleanupClaimName)
				if _, err := root.Lstat(claimRel); os.IsNotExist(err) {
					if err := verifyDatabaseMigrationDigestAt(root, sourceRel, receipt.SourceDigest); err != nil {
						return err
					}
					if err := writeDatabaseMigrationClaim(root, sourceRel, receipt.CleanupToken); err != nil {
						return err
					}
				} else if err != nil {
					return err
				}
				if err := claimDatabaseMigrationSource(root, sourceRel, cleanupRel, receipt.SourceDigest, receipt.CleanupToken); err != nil {
					return err
				}
				continue
			}
			if !cleanupExists {
				return fmt.Errorf("verified migration cleanup claim is absent")
			}
			if !cleanupComplete {
				return fmt.Errorf("migration cleanup claim is incomplete")
			}
			claimRel := filepath.Join(cleanupRel, databaseMigrationCleanupClaimName)
			if _, err := root.Lstat(claimRel); os.IsNotExist(err) {
				return fmt.Errorf("migration cleanup claim has no durable identity")
			} else if err != nil {
				return err
			}
			if err := verifyDatabaseMigrationClaim(root, cleanupRel, receipt.SourceDigest, receipt.CleanupToken); err != nil {
				return err
			}
			receipt.Phase = databaseMigrationCleanupRemoving
			if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
				return err
			}

		case databaseMigrationCleanupRemoving:
			if !targetComplete {
				return fmt.Errorf("cleanup-removing migration target is missing or incomplete")
			}
			if sourceExists {
				return fmt.Errorf("migration source reappeared during cleanup")
			}
			cleanupExists, _, err := databaseMigrationRootPathState(root, cleanupRel)
			if err != nil {
				return err
			}
			if !cleanupExists {
				receipt.Phase = databaseMigrationSourceCleaned
				if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
					return err
				}
				continue
			}
			if err := withVerifiedDatabaseMigrationRoot(root, targetRel, receipt.SourceDigest, receipt.TargetToken, func(revalidateTarget func() error) error {
				databaseMigrationBeforeIrreversibleMutation()
				if err := revalidateTarget(); err != nil {
					return err
				}
				return removeClaimedDatabaseMigrationContents(root, cleanupRel, receipt.CleanupToken)
			}); err != nil {
				return err
			}
			receipt.Phase = databaseMigrationCleanupEmpty
			if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
				return err
			}

		case databaseMigrationCleanupEmpty:
			if !targetComplete {
				return fmt.Errorf("cleanup-empty migration target is missing or incomplete")
			}
			if sourceExists {
				return fmt.Errorf("migration source reappeared during cleanup")
			}
			emptyPath := filepath.Join(townRoot, ".runtime", "dolt-database-migrations", "empty", canonicalDatabaseName(receipt.RigName)+"-"+receipt.CleanupToken)
			if err := ensurePrivateDatabaseCleanupDirectory(filepath.Dir(emptyPath)); err != nil {
				return err
			}
			emptyRel, err := databaseMigrationRootRelativePath(townRoot, emptyPath)
			if err != nil {
				return err
			}
			if err := validateDatabaseMigrationPathCustody(root, emptyRel); err != nil {
				return err
			}
			cleanupExists, _, err := databaseMigrationRootPathState(root, cleanupRel)
			if err != nil {
				return err
			}
			emptyExists, _, err := databaseMigrationRootPathState(root, emptyRel)
			if err != nil {
				return err
			}
			if cleanupExists && emptyExists {
				return fmt.Errorf("migration cleanup and empty quarantine both exist")
			}
			if cleanupExists || emptyExists {
				if err := withVerifiedDatabaseMigrationRoot(root, targetRel, receipt.SourceDigest, receipt.TargetToken, func(revalidateTarget func() error) error {
					databaseMigrationBeforeIrreversibleMutation()
					if err := revalidateTarget(); err != nil {
						return err
					}
					if cleanupExists {
						return removeEmptyClaimedDatabaseMigrationRoot(root, cleanupRel, emptyRel, receipt.CleanupToken)
					}
					return removeEmptyDatabaseMigrationQuarantine(root, emptyRel)
				}); err != nil {
					return err
				}
				if err := syncDatabaseMigrationDirectory(root, filepath.Dir(cleanupRel)); err != nil {
					return err
				}
				continue
			}
			receipt.Phase = databaseMigrationSourceCleaned
			if err := writeDatabaseMigrationReceipt(receiptPath, receipt); err != nil {
				return err
			}

		case databaseMigrationSourceCleaned:
			if !targetComplete {
				return fmt.Errorf("source-cleaned migration target is missing or incomplete")
			}
			cleanupExists, _, err := databaseMigrationRootPathState(root, cleanupRel)
			if err != nil {
				return err
			}
			if sourceExists || cleanupExists {
				return fmt.Errorf("source-cleaned migration still has source data")
			}
			if err := withVerifiedDatabaseMigrationRoot(root, targetRel, receipt.SourceDigest, receipt.TargetToken, func(revalidate func() error) error {
				beadsDir, err := FindOrCreateRigBeadsDir(townRoot, receipt.RigName)
				if err != nil {
					return fmt.Errorf("publishing migrated database ownership: %w", err)
				}
				databaseMigrationBeforeIrreversibleMutation()
				if err := revalidate(); err != nil {
					return err
				}
				if err := ensureMetadataForBeadsDirLocked(townRoot, beadsDir, receipt.RigName); err != nil {
					return fmt.Errorf("publishing migrated database ownership: %w", err)
				}
				databaseMigrationBeforeIrreversibleMutation()
				if err := revalidate(); err != nil {
					return err
				}
				if err := clearDatabaseCleanupReceipt(receiptPath); err != nil {
					return fmt.Errorf("clearing database migration receipt: %w", err)
				}
				return nil
			}); err != nil {
				return err
			}
			InvalidateDBCache()
			return nil
		}
	}
}

func claimDatabaseMigrationSource(root *os.Root, sourceRel, cleanupRel, digest, token string) error {
	return moveClaimedDatabaseMigrationRoot(root, sourceRel, cleanupRel, digest, token, true, "source", databaseMigrationAfterSourceClaimValidation)
}

func moveClaimedDatabaseMigrationRoot(root *os.Root, fromRel, toRel, digest, token string, requireDigest bool, identityName string, afterValidation func()) (result error) {
	claimedRoot, err := root.OpenRoot(fromRel)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, claimedRoot.Close()) }()
	claimedInfo, err := claimedRoot.Stat(".")
	if err != nil {
		return err
	}
	revalidate := func() error {
		currentInfo, err := root.Stat(fromRel)
		if err != nil || !os.SameFile(claimedInfo, currentInfo) {
			return fmt.Errorf("migration %s identity changed", identityName)
		}
		if requireDigest {
			return verifyDatabaseMigrationClaimAt(claimedRoot, digest, token)
		}
		return verifyDatabaseMigrationClaimTokenAt(claimedRoot, token)
	}
	if err := revalidate(); err != nil {
		return err
	}
	if afterValidation != nil {
		afterValidation()
	}
	if err := root.Rename(fromRel, toRel); err != nil {
		return fmt.Errorf("moving verified migration %s: %w", identityName, err)
	}
	movedRoot, err := root.OpenRoot(toRel)
	if err != nil {
		return fmt.Errorf("opening moved migration %s: %w", identityName, err)
	}
	movedInfo, statErr := movedRoot.Stat(".")
	closeErr := movedRoot.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return err
	}
	if !os.SameFile(claimedInfo, movedInfo) {
		fromExists, _, err := databaseMigrationRootPathState(root, fromRel)
		if err != nil {
			return errors.Join(fmt.Errorf("migration %s identity changed", identityName), err)
		}
		if fromExists {
			return errors.Join(fmt.Errorf("migration %s identity changed", identityName), fmt.Errorf("cannot roll back mismatched migration move: original path is occupied"))
		}
		if err := root.Rename(toRel, fromRel); err != nil {
			return errors.Join(fmt.Errorf("migration %s identity changed", identityName), fmt.Errorf("rolling back mismatched migration move: %w", err))
		}
		if err := syncDatabaseMigrationDirectory(root, filepath.Dir(fromRel)); err != nil {
			return errors.Join(fmt.Errorf("migration %s identity changed", identityName), err)
		}
		return fmt.Errorf("migration %s identity changed", identityName)
	}
	if err := revalidateMovedDatabaseMigrationRoot(claimedRoot, digest, token, requireDigest); err != nil {
		return err
	}
	return syncDatabaseMigrationDirectory(root, filepath.Dir(fromRel))
}

func revalidateMovedDatabaseMigrationRoot(root *os.Root, digest, token string, requireDigest bool) error {
	if requireDigest {
		return verifyDatabaseMigrationClaimAt(root, digest, token)
	}
	return verifyDatabaseMigrationClaimTokenAt(root, token)
}

func removeClaimedDatabaseMigrationContents(root *os.Root, cleanupRel, token string) (result error) {
	cleanupRoot, err := root.OpenRoot(cleanupRel)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, cleanupRoot.Close()) }()
	cleanupInfo, err := cleanupRoot.Stat(".")
	if err != nil {
		return err
	}
	revalidateIdentity := func() error {
		currentInfo, err := root.Stat(cleanupRel)
		if err != nil || !os.SameFile(cleanupInfo, currentInfo) {
			return fmt.Errorf("migration cleanup identity changed")
		}
		return nil
	}
	revalidateClaim := func() error {
		if err := revalidateIdentity(); err != nil {
			return err
		}
		return verifyDatabaseMigrationClaimTokenAt(cleanupRoot, token)
	}
	if err := revalidateClaim(); err != nil {
		return err
	}
	databaseMigrationBeforeCleanupRemoval()
	if err := revalidateClaim(); err != nil {
		return err
	}
	databaseMigrationAfterCleanupRevalidation()
	entries, err := fs.ReadDir(cleanupRoot.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == databaseMigrationCleanupClaimName {
			continue
		}
		if err := databaseMigrationBeforeCleanupChildRemoval(entry.Name()); err != nil {
			return err
		}
		if err := cleanupRoot.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("removing claimed migration source child %q: %w", entry.Name(), err)
		}
	}
	if err := revalidateIdentity(); err != nil {
		return err
	}
	if err := verifyDatabaseMigrationClaimTokenAt(cleanupRoot, token); err != nil {
		return err
	}
	return nil
}

func removeEmptyClaimedDatabaseMigrationRoot(root *os.Root, cleanupRel, emptyRel, token string) (result error) {
	cleanupRoot, err := root.OpenRoot(cleanupRel)
	if err != nil {
		return err
	}
	cleanupInfo, err := cleanupRoot.Stat(".")
	if err != nil {
		_ = cleanupRoot.Close()
		return err
	}
	currentInfo, err := root.Stat(cleanupRel)
	if err != nil || !os.SameFile(cleanupInfo, currentInfo) {
		_ = cleanupRoot.Close()
		return fmt.Errorf("migration cleanup identity changed")
	}
	entries, err := fs.ReadDir(cleanupRoot.FS(), ".")
	if err != nil {
		_ = cleanupRoot.Close()
		return err
	}
	claimPresent := false
	for _, entry := range entries {
		if entry.Name() != databaseMigrationCleanupClaimName {
			_ = cleanupRoot.Close()
			return fmt.Errorf("cleanup-empty migration contains unclaimed entry %q", entry.Name())
		}
		claimPresent = true
	}
	if claimPresent {
		if err := verifyDatabaseMigrationClaimTokenAt(cleanupRoot, token); err != nil {
			_ = cleanupRoot.Close()
			return err
		}
		if err := cleanupRoot.Remove(databaseMigrationCleanupClaimName); err != nil {
			_ = cleanupRoot.Close()
			return fmt.Errorf("removing migration cleanup claim: %w", err)
		}
		dir, err := cleanupRoot.Open(".")
		if err != nil {
			_ = cleanupRoot.Close()
			return err
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			_ = cleanupRoot.Close()
			return err
		}
	}
	if err := cleanupRoot.Close(); err != nil {
		return err
	}

	databaseMigrationBeforeCleanupEmptyMove()
	if err := root.Rename(cleanupRel, emptyRel); err != nil {
		return fmt.Errorf("moving empty migration cleanup directory: %w", err)
	}
	movedRoot, err := root.OpenRoot(emptyRel)
	if err != nil {
		return errors.Join(fmt.Errorf("migration cleanup identity changed"), rollbackEmptyDatabaseMigrationMove(root, emptyRel, cleanupRel))
	}
	movedInfo, statErr := movedRoot.Stat(".")
	movedEntries, readErr := fs.ReadDir(movedRoot.FS(), ".")
	closeErr := movedRoot.Close()
	if err := errors.Join(statErr, readErr, closeErr); err != nil {
		return errors.Join(err, rollbackEmptyDatabaseMigrationMove(root, emptyRel, cleanupRel))
	}
	if !os.SameFile(cleanupInfo, movedInfo) || len(movedEntries) != 0 {
		return errors.Join(fmt.Errorf("migration cleanup identity changed"), rollbackEmptyDatabaseMigrationMove(root, emptyRel, cleanupRel))
	}
	return removeEmptyDatabaseMigrationQuarantine(root, emptyRel)
}

func rollbackEmptyDatabaseMigrationMove(root *os.Root, fromRel, toRel string) error {
	if _, err := root.Lstat(toRel); err == nil {
		return fmt.Errorf("cannot roll back migration cleanup move: original path is occupied")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := root.Rename(fromRel, toRel); err != nil {
		return fmt.Errorf("rolling back migration cleanup move: %w", err)
	}
	return syncDatabaseMigrationDirectory(root, filepath.Dir(toRel))
}

func removeEmptyDatabaseMigrationQuarantine(root *os.Root, emptyRel string) error {
	emptyRoot, err := root.OpenRoot(emptyRel)
	if err != nil {
		return err
	}
	emptyInfo, err := emptyRoot.Stat(".")
	if err != nil {
		_ = emptyRoot.Close()
		return err
	}
	currentInfo, err := root.Stat(emptyRel)
	if err != nil || !os.SameFile(emptyInfo, currentInfo) {
		_ = emptyRoot.Close()
		return fmt.Errorf("migration empty quarantine identity changed")
	}
	entries, err := fs.ReadDir(emptyRoot.FS(), ".")
	if err != nil {
		_ = emptyRoot.Close()
		return err
	}
	if len(entries) != 0 {
		_ = emptyRoot.Close()
		return fmt.Errorf("migration empty quarantine contains unclaimed data")
	}
	if err := root.Remove(emptyRel); err != nil {
		_ = emptyRoot.Close()
		return fmt.Errorf("removing empty migration cleanup quarantine: %w", err)
	}
	entries, readErr := fs.ReadDir(emptyRoot.FS(), ".")
	closeErr := emptyRoot.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("removed migration quarantine identity gained data")
	}
	return syncDatabaseMigrationDirectory(root, filepath.Dir(emptyRel))
}

func verifyDatabaseMigrationDigestAt(root *os.Root, rel, want string) error {
	databaseRoot, err := root.OpenRoot(rel)
	if err != nil {
		return fmt.Errorf("opening migrated database at %s: %w", rel, err)
	}
	got, digestErr := databaseMigrationTreeDigestFromRoot(databaseRoot, false)
	closeErr := databaseRoot.Close()
	if err := errors.Join(digestErr, closeErr); err != nil {
		return fmt.Errorf("hashing migrated database at %s: %w", rel, err)
	}
	if got != want {
		return fmt.Errorf("migrated database digest mismatch at %s", rel)
	}
	return nil
}

func withVerifiedDatabaseMigrationRoot(root *os.Root, rel, want, token string, operation func(func() error) error) error {
	return withDatabaseMigrationTargetRoot(root, rel, want, token, func(_ *os.Root, revalidate func() error) error {
		return operation(revalidate)
	})
}

func withClaimedDatabaseMigrationRoot(root *os.Root, rel, token string, operation func(*os.Root) error) error {
	return withDatabaseMigrationTargetRoot(root, rel, "", token, func(databaseRoot *os.Root, _ func() error) error {
		return operation(databaseRoot)
	})
}

func withDatabaseMigrationTargetRoot(root *os.Root, rel, want, token string, operation func(*os.Root, func() error) error) (result error) {
	databaseRoot, err := root.OpenRoot(rel)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, databaseRoot.Close()) }()
	openedInfo, err := databaseRoot.Stat(".")
	if err != nil {
		return err
	}
	databaseMigrationBeforeTargetMutation()
	revalidate := func() error {
		if want == "" {
			err = verifyDatabaseMigrationClaimTokenAt(databaseRoot, token)
		} else {
			err = verifyDatabaseMigrationClaimAt(databaseRoot, want, token)
		}
		if err != nil {
			return fmt.Errorf("verifying migration target claim: %w", err)
		}
		return verifyDatabaseMigrationTargetIdentity(root, rel, openedInfo)
	}
	if err := revalidate(); err != nil {
		return err
	}
	operationErr := operation(databaseRoot, revalidate)
	return errors.Join(operationErr, revalidate())
}

func verifyDatabaseMigrationTargetIdentity(root *os.Root, rel string, openedInfo fs.FileInfo) error {
	currentInfo, err := root.Stat(rel)
	if err != nil || !os.SameFile(openedInfo, currentInfo) {
		return fmt.Errorf("migration target identity changed")
	}
	return nil
}

func newDatabaseMigrationCleanupToken() (string, error) {
	var token [sha256.Size]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func writeDatabaseMigrationClaim(root *os.Root, cleanupRel, token string) error {
	claimRel := filepath.Join(cleanupRel, databaseMigrationCleanupClaimName)
	file, err := root.OpenFile(claimRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating migration cleanup claim: %w", err)
	}
	_, writeErr := io.WriteString(file, token+"\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	dir, err := root.Open(cleanupRel)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func verifyDatabaseMigrationClaimToken(root *os.Root, claimRel, token string) error {
	claimRoot, err := root.OpenRoot(claimRel)
	if err != nil {
		return err
	}
	defer claimRoot.Close()
	return verifyDatabaseMigrationClaimTokenAt(claimRoot, token)
}

func verifyDatabaseMigrationClaimTokenAt(claimRoot *os.Root, token string) error {
	claim, err := claimRoot.Lstat(databaseMigrationCleanupClaimName)
	if err != nil {
		return fmt.Errorf("reading migration claim: %w", err)
	}
	if !claim.Mode().IsRegular() || claim.Mode().Perm() != 0o600 {
		return fmt.Errorf("migration claim is not a private regular file")
	}
	data, err := claimRoot.ReadFile(databaseMigrationCleanupClaimName)
	if err != nil {
		return err
	}
	databaseMigrationAfterClaimTokenRead()
	if string(data) != token+"\n" {
		return fmt.Errorf("migration claim token mismatch")
	}
	return nil
}

func verifyDatabaseMigrationClaim(root *os.Root, cleanupRel, digest, token string) error {
	cleanupRoot, err := root.OpenRoot(cleanupRel)
	if err != nil {
		return err
	}
	defer cleanupRoot.Close()
	return verifyDatabaseMigrationClaimAt(cleanupRoot, digest, token)
}

func verifyDatabaseMigrationClaimAt(claimRoot *os.Root, digest, token string) error {
	if err := verifyDatabaseMigrationClaimTokenAt(claimRoot, token); err != nil {
		return err
	}
	got, err := databaseMigrationTreeDigestFromRoot(claimRoot, true)
	if err != nil {
		return err
	}
	if got != digest {
		return fmt.Errorf("migration claim digest mismatch")
	}
	return nil
}

func ensureDatabaseMigrationTargetClaim(root *os.Root, targetRel, token string) error {
	claimRel := targetRel + ".migration-target-" + token
	if err := validateDatabaseMigrationPathCustody(root, claimRel); err != nil {
		return err
	}
	targetExists, _, err := databaseMigrationRootPathState(root, targetRel)
	if err != nil {
		return err
	}
	if targetExists {
		if err := verifyDatabaseMigrationClaimToken(root, targetRel, token); err != nil {
			return fmt.Errorf("verifying migration target claim: %w", err)
		}
		return nil
	}
	claimExists, _, err := databaseMigrationRootPathState(root, claimRel)
	if err != nil {
		return err
	}
	if !claimExists {
		if err := root.MkdirAll(filepath.Dir(targetRel), 0o755); err != nil {
			return fmt.Errorf("creating data directory: %w", err)
		}
		if err := root.Mkdir(claimRel, 0o700); err != nil {
			return fmt.Errorf("creating migration target claim: %w", err)
		}
		if err := writeDatabaseMigrationClaim(root, claimRel, token); err != nil {
			return err
		}
	}
	claimRoot, err := root.OpenRoot(claimRel)
	if err != nil {
		return err
	}
	defer claimRoot.Close()
	claimInfo, err := claimRoot.Stat(".")
	if err != nil {
		return err
	}
	databaseMigrationBeforeTargetMutation()
	revalidatePreparedClaim := func() error {
		if err := verifyDatabaseMigrationClaimTokenAt(claimRoot, token); err != nil {
			return fmt.Errorf("verifying prepared migration target claim: %w", err)
		}
		targetExists, _, err = databaseMigrationRootPathState(root, targetRel)
		if err != nil {
			return err
		}
		if targetExists {
			return fmt.Errorf("migration target appeared during claim publication")
		}
		currentClaimInfo, err := root.Stat(claimRel)
		if err != nil || !os.SameFile(claimInfo, currentClaimInfo) {
			return fmt.Errorf("prepared migration target claim identity changed")
		}
		return nil
	}
	if err := revalidatePreparedClaim(); err != nil {
		return err
	}
	databaseMigrationBeforeClaimPublication()
	if err := revalidatePreparedClaim(); err != nil {
		return err
	}
	if err := root.Rename(claimRel, targetRel); err != nil {
		return fmt.Errorf("publishing migration target claim: %w", err)
	}
	if err := syncDatabaseMigrationDirectory(root, filepath.Dir(targetRel)); err != nil {
		return err
	}
	currentTargetInfo, err := root.Stat(targetRel)
	if err != nil || !os.SameFile(claimInfo, currentTargetInfo) {
		return fmt.Errorf("migration target identity changed during claim publication")
	}
	if err := verifyDatabaseMigrationClaimTokenAt(claimRoot, token); err != nil {
		return fmt.Errorf("verifying migration target claim: %w", err)
	}
	return nil
}

func copyDatabaseMigrationToStage(root *os.Root, sourceRel, targetRel, digest, token string) error {
	sourceRoot, err := root.OpenRoot(sourceRel)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	sourceInfo, err := sourceRoot.Stat(".")
	if err != nil {
		return err
	}
	return withClaimedDatabaseMigrationRoot(root, targetRel, token, func(targetRoot *os.Root) error {
		entries, err := fs.ReadDir(targetRoot.FS(), ".")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == databaseMigrationCleanupClaimName {
				continue
			}
			if err := targetRoot.RemoveAll(entry.Name()); err != nil {
				return err
			}
		}
		if err := syncDatabaseMigrationDirectory(targetRoot, "."); err != nil {
			return err
		}
		if err := targetRoot.Mkdir(databaseMigrationStageName, sourceInfo.Mode().Perm()); err != nil {
			return err
		}
		stageRoot, err := targetRoot.OpenRoot(databaseMigrationStageName)
		if err != nil {
			return err
		}
		copyErr := fs.WalkDir(sourceRoot.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == "." {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			name := filepath.FromSlash(path)
			switch {
			case info.IsDir():
				return stageRoot.Mkdir(name, info.Mode().Perm())
			case info.Mode().IsRegular():
				source, err := sourceRoot.Open(name)
				if err != nil {
					return err
				}
				target, err := stageRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
				if err != nil {
					_ = source.Close()
					return err
				}
				_, copyErr := io.Copy(target, source)
				return errors.Join(copyErr, target.Sync(), target.Close(), source.Close())
			case info.Mode()&os.ModeSymlink != 0:
				target, err := sourceRoot.Readlink(name)
				if err != nil {
					return err
				}
				return stageRoot.Symlink(target, name)
			default:
				return fmt.Errorf("unsupported database migration entry %s", path)
			}
		})
		if copyErr != nil {
			_ = stageRoot.Close()
			return fmt.Errorf("copying database to migration stage: %w", copyErr)
		}
		got, digestErr := databaseMigrationTreeDigestFromRoot(stageRoot, false)
		if digestErr == nil && got != digest {
			digestErr = fmt.Errorf("migrated database digest mismatch at %s", databaseMigrationStageName)
		}
		if digestErr != nil {
			_ = stageRoot.Close()
			return digestErr
		}
		return errors.Join(syncDatabaseMigrationTree(stageRoot), stageRoot.Close())
	})
}

func syncDatabaseMigrationTree(root *os.Root) error {
	var dirs []string
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported database migration entry %s", path)
		}
		file, err := root.Open(filepath.FromSlash(path))
		if err != nil {
			return err
		}
		return errors.Join(file.Sync(), file.Close())
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDatabaseMigrationDirectory(root, filepath.FromSlash(dirs[i])); err != nil {
			return err
		}
	}
	return nil
}

func verifyDatabaseMigrationStage(root *os.Root, targetRel, digest, token string) error {
	targetRoot, err := root.OpenRoot(targetRel)
	if err != nil {
		return err
	}
	defer targetRoot.Close()
	if err := verifyDatabaseMigrationClaimTokenAt(targetRoot, token); err != nil {
		return fmt.Errorf("verifying migration target claim: %w", err)
	}
	return verifyDatabaseMigrationStageAt(targetRoot, digest)
}

func verifyDatabaseMigrationStageAt(targetRoot *os.Root, digest string) error {
	stageRoot, err := targetRoot.OpenRoot(databaseMigrationStageName)
	if err != nil {
		return err
	}
	got, digestErr := databaseMigrationTreeDigestFromRoot(stageRoot, false)
	closeErr := stageRoot.Close()
	if err := errors.Join(digestErr, closeErr); err != nil {
		return err
	}
	if got != digest {
		return fmt.Errorf("migration stage digest mismatch")
	}
	return nil
}

func promoteDatabaseMigrationStage(root *os.Root, targetRel, digest, token string) error {
	return withClaimedDatabaseMigrationRoot(root, targetRel, token, func(targetRoot *os.Root) error {
		if err := verifyDatabaseMigrationStageAt(targetRoot, digest); err != nil {
			return err
		}
		stageRoot, err := targetRoot.OpenRoot(databaseMigrationStageName)
		if err != nil {
			return err
		}
		entries, readErr := fs.ReadDir(stageRoot.FS(), ".")
		closeErr := stageRoot.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == ".dolt" {
				continue
			}
			if entry.Name() == databaseMigrationStageName {
				return fmt.Errorf("database contains reserved migration stage entry %q", entry.Name())
			}
			source := filepath.Join(databaseMigrationStageName, entry.Name())
			if err := targetRoot.RemoveAll(entry.Name()); err != nil {
				return err
			}
			if err := targetRoot.Rename(source, entry.Name()); err != nil {
				return err
			}
		}
		if _, err := targetRoot.Lstat(".dolt"); err == nil {
			return fmt.Errorf("migration target became complete before stage promotion")
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := targetRoot.Rename(filepath.Join(databaseMigrationStageName, ".dolt"), ".dolt"); err != nil {
			return err
		}
		if err := syncDatabaseMigrationDirectory(targetRoot, "."); err != nil {
			return err
		}
		return removeEmptyDatabaseMigrationStageAt(targetRoot)
	})
}

func removeEmptyDatabaseMigrationStage(root *os.Root, targetRel, token string) error {
	return withClaimedDatabaseMigrationRoot(root, targetRel, token, removeEmptyDatabaseMigrationStageAt)
}

func removeEmptyDatabaseMigrationStageAt(targetRoot *os.Root) error {
	stageRoot, err := targetRoot.OpenRoot(databaseMigrationStageName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	entries, readErr := fs.ReadDir(stageRoot.FS(), ".")
	closeErr := stageRoot.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("database migration stage is not empty")
	}
	if err := targetRoot.Remove(databaseMigrationStageName); err != nil {
		return err
	}
	return syncDatabaseMigrationDirectory(targetRoot, ".")
}

func syncDatabaseMigrationDirectory(root *os.Root, rel string) error {
	dir, err := root.Open(rel)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// DatabaseExists checks whether a rig database exists on the host filesystem
// (.dolt-data/<name>/.dolt). This is a conservative filesystem-only check;
// for containerised Dolt use WLCommons.DatabaseExists instead.
func DatabaseExists(townRoot, rigName string) bool {
	config := DefaultConfig(townRoot)
	doltDir := filepath.Join(config.DataDir, rigName, ".dolt")
	_, err := os.Stat(doltDir)
	return err == nil
}

// BrokenWorkspace represents a workspace whose metadata.json points to a
// nonexistent database on the Dolt server.
type BrokenWorkspace struct {
	// RigName is the rig whose database is missing.
	RigName string

	// BeadsDir is the path to the .beads directory with the broken metadata.
	BeadsDir string

	// ConfiguredDB is the dolt_database value from metadata.json.
	ConfiguredDB string

	// HasLocalData is true if .beads/dolt/<dbname> exists locally and can be migrated.
	HasLocalData bool

	// LocalDataPath is the path to local Dolt data, if present.
	LocalDataPath string

	// NotServed is true when the database exists on the filesystem but the
	// running Dolt server is not serving it. This typically means the server
	// needs a restart or was started from a different data directory.
	NotServed bool
}

// OrphanedDatabase represents a database in .dolt-data/ that is not referenced
// by any rig's metadata.json. These are leftover from partial setups, renames,
// or failed migrations.
type OrphanedDatabase struct {
	// Name is the database directory name in .dolt-data/.
	Name string

	// Path is the full path to the database directory.
	Path string

	// SizeBytes is the total size of the database directory.
	SizeBytes int64
}

// protectedSharedServerDatabases returns the registry of databases that are
// intentionally hosted by the shared Dolt server but are not referenced by any
// rig's metadata. Mapped to a human-readable owner label used by
// CollectDatabaseOwners so `gt dolt list` / `gt dolt status` annotate them as
// protected rather than reporting them as orphans.
//
// Single source of truth for orphan-detection skipping (FindOrphanedDatabases,
// RemoveDatabase) and owner-label reporting (CollectDatabaseOwners). Adding a
// new protected database here automatically picks up all three surfaces.
func protectedSharedServerDatabases() map[string]string {
	return map[string]string{
		"beads_global": "global shared beads database (protected)",
	}
}

func canonicalDatabaseName(dbName string) string {
	return strings.ToLower(dbName)
}

// isProtectedSharedServerDatabase reports databases that are intentionally
// hosted by the shared Dolt server but are not referenced by rig metadata.
func isProtectedSharedServerDatabase(dbName string) bool {
	_, ok := protectedSharedServerDatabases()[canonicalDatabaseName(dbName)]
	return ok
}

// FindOrphanedDatabases scans .dolt-data/ for databases that are not referenced
// by any rig's metadata.json dolt_database field. These orphans consume disk space
// and are served by the Dolt server unnecessarily.
func FindOrphanedDatabases(townRoot string) ([]OrphanedDatabase, error) {
	databases, err := ListDatabases(townRoot)
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	if len(databases) == 0 {
		return nil, nil
	}

	// Collect all referenced database names from metadata.json files
	referenced, err := collectReferencedDatabases(townRoot)
	if err != nil {
		return nil, fmt.Errorf("collecting database ownership: %w", err)
	}

	// Find databases that exist on disk but aren't referenced
	config := DefaultConfig(townRoot)
	var orphans []OrphanedDatabase
	for _, dbName := range databases {
		if referenced[canonicalDatabaseName(dbName)] || isProtectedSharedServerDatabase(dbName) {
			continue
		}
		dbPath := filepath.Join(config.DataDir, dbName)
		size := dirSize(dbPath)
		orphans = append(orphans, OrphanedDatabase{
			Name:      dbName,
			Path:      dbPath,
			SizeBytes: size,
		})
	}

	return orphans, nil
}

// readExistingDoltDatabase reads the dolt_database field from an existing metadata.json.
// A missing file or field is not an error; unreadable or malformed ownership
// metadata is, because cleanup must not translate uncertainty into "unowned."
func readExistingDoltDatabase(beadsDir string) (string, error) {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading %s: %w", metadataPath, err)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parsing %s: %w", metadataPath, err)
	}
	value, exists := meta["dolt_database"]
	if !exists || value == nil {
		return "", nil
	}
	db, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("parsing %s: dolt_database must be a string", metadataPath)
	}
	if db != "" && (!validSQLName(db) || db == "." || db == "..") {
		return "", fmt.Errorf("parsing %s: invalid dolt_database %q", metadataPath, db)
	}
	return db, nil
}

// collectReferencedDatabases returns a set of database names referenced by
// any rig's metadata.json dolt_database field. It checks multiple sources
// to avoid falsely flagging legitimate databases as orphans (gt-q8f6n):
//   - town-level .beads/metadata.json (HQ)
//   - all rigs from rigs.json
//   - all routes from routes.jsonl (catches rigs not yet in rigs.json)
//   - broad scan of metadata.json files under town root
func collectReferencedDatabases(townRoot string) (map[string]bool, error) {
	referenced := make(map[string]bool)
	migrationReceipts, err := loadDatabaseMigrationReceipts(townRoot)
	if err != nil {
		return nil, fmt.Errorf("loading pending database migrations: %w", err)
	}
	for _, receipt := range migrationReceipts {
		referenced[canonicalDatabaseName(receipt.RigName)] = true
	}
	addReference := func(beadsDir string) error {
		db, err := readExistingDoltDatabase(beadsDir)
		if err != nil {
			return err
		}
		if db != "" {
			referenced[canonicalDatabaseName(db)] = true
		}
		return nil
	}

	// Check town-level beads (hq)
	townBeadsDir := filepath.Join(townRoot, ".beads")
	townDB, err := readExistingDoltDatabase(townBeadsDir)
	if err != nil {
		return nil, err
	}
	// hq is the canonical town database even while metadata is absent or being
	// repaired; never let registry loss make it eligible for cleanup.
	referenced["hq"] = true
	if townDB != "" {
		referenced[canonicalDatabaseName(townDB)] = true
	}

	// Check all rigs from rigs.json. A configured rig that cannot be resolved is
	// ownership uncertainty, not evidence that its database is orphaned.
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := configpkg.LoadRigsConfig(rigsPath)
	if err != nil && !errors.Is(err, configpkg.ErrNotFound) {
		return nil, fmt.Errorf("loading rig ownership registry: %w", err)
	}
	if rigsConfig != nil {
		for rigName, entry := range rigsConfig.Rigs {
			beadsDir := FindRigBeadsDir(townRoot, rigName)
			if beadsDir == "" {
				return nil, fmt.Errorf("configured rig %q has no resolvable beads directory", rigName)
			}
			if _, err := os.Stat(beadsDir); err != nil {
				return nil, fmt.Errorf("resolving configured rig %q ownership: %w", rigName, err)
			}
			db, err := readExistingDoltDatabase(beadsDir)
			if err != nil {
				return nil, err
			}
			if db != "" {
				referenced[canonicalDatabaseName(db)] = true
			}
			if db == "" {
				return nil, fmt.Errorf("configured rig %q has no database ownership metadata", rigName)
			}
			if entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
				referenced[canonicalDatabaseName(strings.TrimSuffix(entry.BeadsConfig.Prefix, "-"))] = true
			}
		}
	}

	// Also check routes.jsonl — catches rigs that have routes but aren't in
	// rigs.json yet (e.g., hop before gt rig add). (gt-q8f6n fix)
	routesPath := filepath.Join(townRoot, ".beads", "routes.jsonl")
	if routesData, readErr := os.ReadFile(routesPath); readErr == nil {
		for lineNumber, line := range strings.Split(string(routesData), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			var route struct {
				Prefix      string `json:"prefix"`
				Path        string `json:"path"`
				PendingPath string `json:"_gt_pending_path"`
			}
			if err := json.Unmarshal([]byte(line), &route); err != nil {
				return nil, fmt.Errorf("parsing route ownership registry %s:%d: %w", routesPath, lineNumber+1, err)
			}
			if route.Path == "" && route.PendingPath != "" {
				continue
			}
			cleanPath := filepath.Clean(route.Path)
			if route.Path == "" || filepath.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("parsing route ownership registry %s:%d: invalid path %q", routesPath, lineNumber+1, route.Path)
			}
			// route.Path is relative to town root, e.g., "hop", "beads/mayor/rig"
			beadsDir := filepath.Join(townRoot, cleanPath, ".beads")
			if _, err := os.Stat(beadsDir); err != nil {
				return nil, fmt.Errorf("resolving route ownership registry %s:%d: %w", routesPath, lineNumber+1, err)
			}
			db, err := readExistingDoltDatabase(beadsDir)
			if err != nil {
				return nil, err
			}
			if db != "" {
				referenced[canonicalDatabaseName(db)] = true
			}
			if db == "" {
				return nil, fmt.Errorf("route ownership registry %s:%d has no database ownership metadata", routesPath, lineNumber+1)
			}
			if route.Prefix != "" {
				referenced[canonicalDatabaseName(strings.TrimSuffix(route.Prefix, "-"))] = true
			}
		}
	} else if !os.IsNotExist(readErr) {
		return nil, fmt.Errorf("reading route ownership registry: %w", readErr)
	}

	// Scan top-level directories for any .beads/metadata.json with dolt_database.
	// This catches rigs that exist on disk but aren't in rigs.json or routes.jsonl.
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return nil, fmt.Errorf("enumerating town ownership metadata: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".beads" || entry.Name() == "mayor" {
			continue
		}
		// Check <rig>/.beads/metadata.json
		if err := addReference(filepath.Join(townRoot, entry.Name(), ".beads")); err != nil {
			return nil, err
		}
		// Check <rig>/mayor/rig/.beads/metadata.json
		if err := addReference(filepath.Join(townRoot, entry.Name(), "mayor", "rig", ".beads")); err != nil {
			return nil, err
		}
	}

	return referenced, nil
}

// CollectDatabaseOwners returns a map from database name to a human-readable
// owner description (e.g., "gastown rig beads", "town beads"). This is used by
// gt dolt status to annotate each database with its rig owner, preventing
// accidental drops of production databases. (GH#2252)
func CollectDatabaseOwners(townRoot string) map[string]string {
	owners := make(map[string]string)

	// Check town-level beads (hq)
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if db, err := readExistingDoltDatabase(townBeadsDir); err == nil && db != "" {
		owners[canonicalDatabaseName(db)] = "town beads"
	}

	// Check all rigs from rigs.json
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	data, err := os.ReadFile(rigsPath)
	if err == nil {
		var config struct {
			Rigs map[string]interface{} `json:"rigs"`
		}
		if err := json.Unmarshal(data, &config); err == nil {
			for rigName := range config.Rigs {
				beadsDir := FindRigBeadsDir(townRoot, rigName)
				if beadsDir == "" {
					continue
				}
				if db, err := readExistingDoltDatabase(beadsDir); err == nil && db != "" {
					owners[canonicalDatabaseName(db)] = rigName + " rig beads"
				}
			}
		}
	}

	// Also check routes.jsonl
	routesPath := filepath.Join(townRoot, ".beads", "routes.jsonl")
	if routesData, readErr := os.ReadFile(routesPath); readErr == nil {
		for _, line := range strings.Split(string(routesData), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var route struct {
				Prefix      string `json:"prefix"`
				Path        string `json:"path"`
				PendingPath string `json:"_gt_pending_path"`
			}
			if json.Unmarshal([]byte(line), &route) != nil || route.Path == "" {
				continue
			}
			beadsDir := filepath.Join(townRoot, route.Path, ".beads")
			if db, err := readExistingDoltDatabase(beadsDir); err == nil && db != "" {
				canonical := canonicalDatabaseName(db)
				if _, already := owners[canonical]; !already {
					// Derive a name from the route path
					parts := strings.Split(route.Path, "/")
					owners[canonical] = parts[0] + " rig beads"
				}
			}
		}
	}

	// Scan top-level directories for any .beads/metadata.json
	if entries, readErr := os.ReadDir(townRoot); readErr == nil {
		for _, entry := range entries {
			if !entry.IsDir() || entry.Name() == ".beads" || entry.Name() == "mayor" {
				continue
			}
			dirName := entry.Name()
			if db, err := readExistingDoltDatabase(filepath.Join(townRoot, dirName, ".beads")); err == nil && db != "" {
				canonical := canonicalDatabaseName(db)
				if _, already := owners[canonical]; !already {
					owners[canonical] = dirName + " rig beads"
				}
			}
			if db, err := readExistingDoltDatabase(filepath.Join(townRoot, dirName, "mayor", "rig", ".beads")); err == nil && db != "" {
				canonical := canonicalDatabaseName(db)
				if _, already := owners[canonical]; !already {
					owners[canonical] = dirName + " rig beads"
				}
			}
		}
	}

	// Label protected shared-server databases so `gt dolt list` doesn't render
	// them as orphans. Only labels protected DBs that actually exist on disk —
	// otherwise we'd advertise a phantom owner. Never overwrites a rig-derived
	// label if one is already present.
	config := DefaultConfig(townRoot)
	if entries, err := os.ReadDir(config.DataDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			canonical := canonicalDatabaseName(entry.Name())
			label, protected := protectedSharedServerDatabases()[canonical]
			if !protected {
				continue
			}
			if _, err := os.Stat(filepath.Join(config.DataDir, entry.Name(), ".dolt")); err != nil {
				continue
			}
			if _, already := owners[canonical]; already {
				continue
			}
			owners[canonical] = label
		}
	}

	return owners
}

const databaseCleanupReceiptVersion = 3

type databaseCleanupPhase string

const (
	databaseCleanupPrepared      databaseCleanupPhase = "prepared"
	databaseCleanupDropAttempted databaseCleanupPhase = "drop-attempted"
	databaseCleanupOfflineReady  databaseCleanupPhase = "offline-prepared"
)

type databaseCleanupReceipt struct {
	Version     int                  `json:"version"`
	Database    string               `json:"database"`
	Force       bool                 `json:"force"`
	Phase       databaseCleanupPhase `json:"phase"`
	Incarnation string               `json:"incarnation"`
}

var (
	databaseCleanupFileSync = func(f *os.File) error { return f.Sync() }
	databaseCleanupDirSync  = func(f *os.File) error { return f.Sync() }
)

// WithDatabaseOwnershipTransaction excludes destructive cleanup while a caller
// creates, renames, or publishes ownership for a Dolt database.
func WithDatabaseOwnershipTransaction(townRoot string, operation func() error) error {
	return doltlock.WithDatabaseOwnership(townRoot, operation)
}

// RemoveDatabase removes an orphaned database directory from .dolt-data/.
// The caller should verify the database is actually orphaned before calling this.
// If the Dolt server is running, it will DROP the database first.
// If force is false and the database has real user tables, it refuses to remove. (gt-q8f6n)
func RemoveDatabase(townRoot, dbName string, force bool) error {
	return removeDatabase(townRoot, dbName, force, "")
}

// RemoveDatabaseIfRootIncarnation removes dbName only when it is still the
// database created by the owning operation.
func RemoveDatabaseIfRootIncarnation(townRoot, dbName, rootIdentity string, force bool) error {
	if rootIdentity == "" {
		return fmt.Errorf("database root identity is required")
	}
	return removeDatabase(townRoot, dbName, force, rootIdentity)
}

func removeDatabase(townRoot, dbName string, force bool, rootIdentity string) error {
	if _, err := DatabasePath(townRoot, dbName); err != nil {
		return err
	}
	if isProtectedSharedServerDatabase(dbName) {
		return fmt.Errorf("database %q is a protected shared-server database", dbName)
	}

	doltLifecycleMu.Lock()
	defer doltLifecycleMu.Unlock()
	lifecycleLock, err := tryLockDoltLifecycle(townRoot)
	if err != nil {
		return err
	}
	defer func() { _ = lifecycleLock.Unlock() }()

	return WithDatabaseOwnershipTransaction(townRoot, func() error {
		lockDir := filepath.Join(townRoot, ".runtime", "dolt-database-cleanup")
		if err := ensurePrivateDatabaseCleanupDirectory(lockDir); err != nil {
			return fmt.Errorf("creating database cleanup lock directory: %w", err)
		}
		databaseLockPath := filepath.Join(lockDir, canonicalDatabaseName(dbName)+".lock")
		fileLock := flock.New(databaseLockPath)
		if err := fileLock.Lock(); err != nil {
			return fmt.Errorf("locking database cleanup for %q: %w", dbName, err)
		}
		defer func() { _ = fileLock.Unlock() }()
		if err := os.Chmod(databaseLockPath, 0o600); err != nil {
			return fmt.Errorf("tightening database cleanup lock for %q: %w", dbName, err)
		}

		return removeDatabaseLocked(townRoot, dbName, force, rootIdentity)
	})
}

func removeDatabaseLocked(townRoot, dbName string, force bool, rootIdentity string) error {
	config := DefaultConfig(townRoot)
	dbPath := filepath.Join(config.DataDir, dbName)
	receiptPath := databaseCleanupReceiptPath(townRoot, dbName)
	receipt, receiptErr := readDatabaseCleanupReceipt(receiptPath)
	if receiptErr == nil {
		if receipt.Database != dbName {
			return fmt.Errorf("database cleanup receipt does not match %q", dbName)
		}
		if receipt.Force && !force {
			return fmt.Errorf("database %q has a pending forced cleanup — rerun with --force", dbName)
		}
		if rootIdentity != "" && databaseCleanupRootIdentity(receipt.Incarnation) != rootIdentity {
			return fmt.Errorf("database %q no longer matches owning root incarnation", dbName)
		}
		running, _, runningErr := IsRunning(townRoot)
		if runningErr != nil {
			return fmt.Errorf("checking Dolt server before resuming database cleanup: %w", runningErr)
		}
		if receipt.Phase == databaseCleanupOfflineReady {
			if running {
				return fmt.Errorf("database %q has a pending offline cleanup — stop the server and retry", dbName)
			}
			return resumeOfflineDatabaseRemoval(townRoot, dbName, dbPath, receiptPath, receipt)
		}
		if !running {
			return fmt.Errorf("database %q has a pending live-server cleanup — start the server and retry", dbName)
		}
		return resumeRunningDatabaseRemoval(townRoot, dbName, dbPath, receiptPath, receipt)
	}
	if !os.IsNotExist(receiptErr) {
		return fmt.Errorf("reading database cleanup receipt for %q: %w", dbName, receiptErr)
	}

	// Verify the directory exists
	if _, err := os.Stat(filepath.Join(dbPath, ".dolt")); err != nil {
		return fmt.Errorf("database %q not found at %s", dbName, dbPath)
	}
	if err := ensureDatabaseCleanupUnreferenced(townRoot, dbName); err != nil {
		return err
	}

	// Safety check: if DB has real data and force is not set, refuse. (gt-q8f6n, gt-xvh)
	// This prevents destroying legitimate databases that happen to be unreferenced.
	running, _, runningErr := IsRunning(townRoot)
	if runningErr != nil {
		return fmt.Errorf("checking Dolt server before removing database %q: %w", dbName, runningErr)
	}
	if !force {
		if !running {
			// Server is down — check via filesystem size as a safety proxy. (gt-xvh)
			// Databases with >1MB of data are almost certainly not empty orphans.
			// Without the server, we can't query tables, so size is the best heuristic.
			size := dirSize(dbPath)
			const safeRemoveThreshold = 1 << 20 // 1MB
			if size > safeRemoveThreshold {
				return fmt.Errorf("database %q has %s of data (server offline, cannot verify contents) — start server or use --force to remove",
					dbName, formatBytes(size))
			}
		}
	}

	if running {
		controlDB, targetLive, err := liveBranchControlDatabase(townRoot, dbName)
		if err != nil {
			return err
		}
		if !targetLive && !force {
			return fmt.Errorf("database %q is not loaded by the live server; cannot inspect user tables — use --force to remove", dbName)
		}
		if targetLive && !force {
			hasData, inspectErr := databaseHasUserTables(townRoot, dbName)
			if inspectErr != nil {
				return fmt.Errorf("inspecting user tables in database %q: %w", dbName, inspectErr)
			}
			if hasData {
				return fmt.Errorf("database %q has user tables — use --force to remove", dbName)
			}
		}
		if controlDB == "" {
			return fmt.Errorf("no surviving live database available for branch-control cleanup of %q", dbName)
		}
		controlIdentifier := strings.ReplaceAll(controlDB, "`", "``")
		if err := serverExecSQL(townRoot, fmt.Sprintf("USE `%s`; SELECT 1", controlIdentifier)); err != nil {
			return fmt.Errorf("preflighting live branch-control database %q: %w", controlDB, err)
		}
		incarnation, err := databaseCleanupIncarnation(townRoot, dbName, dbPath, targetLive)
		if err != nil {
			return fmt.Errorf("identifying database %q before cleanup: %w", dbName, err)
		}
		if rootIdentity != "" && databaseCleanupRootIdentity(incarnation) != rootIdentity {
			return fmt.Errorf("database %q no longer matches owning root incarnation", dbName)
		}
		receipt = databaseCleanupReceipt{
			Version: databaseCleanupReceiptVersion, Database: dbName, Force: force,
			Phase: databaseCleanupPrepared, Incarnation: incarnation,
		}
		if err := writeDatabaseCleanupReceipt(receiptPath, receipt); err != nil {
			return fmt.Errorf("writing database cleanup receipt for %q: %w", dbName, err)
		}
		return resumeRunningDatabaseRemoval(townRoot, dbName, dbPath, receiptPath, receipt)
	}

	incarnation, err := databaseCleanupIncarnation(townRoot, dbName, dbPath, false)
	if err != nil {
		return fmt.Errorf("identifying offline database %q before cleanup: %w", dbName, err)
	}
	if rootIdentity != "" && databaseCleanupRootIdentity(incarnation) != rootIdentity {
		return fmt.Errorf("database %q no longer matches owning root incarnation", dbName)
	}
	if err := ensureDatabaseCleanupTarget(townRoot, dbName, dbPath, incarnation, false); err != nil {
		return err
	}
	receipt = databaseCleanupReceipt{
		Version: databaseCleanupReceiptVersion, Database: dbName, Force: force,
		Phase: databaseCleanupOfflineReady, Incarnation: incarnation,
	}
	if err := writeDatabaseCleanupReceipt(receiptPath, receipt); err != nil {
		return fmt.Errorf("writing offline database cleanup receipt for %q: %w", dbName, err)
	}
	return resumeOfflineDatabaseRemoval(townRoot, dbName, dbPath, receiptPath, receipt)
}

// PendingDatabaseCleanupNames returns valid durable database-cleanup receipts.
func PendingDatabaseCleanupNames(townRoot string) ([]string, error) {
	dir := filepath.Join(townRoot, ".runtime", "dolt-database-cleanup")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading pending database cleanup directory: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		receipt, err := readDatabaseCleanupReceipt(filepath.Join(dir, entry.Name()))
		if err != nil {
			if quarantineErr := moveDatabaseCleanupReceiptToQuarantine(filepath.Join(dir, entry.Name())); quarantineErr != nil {
				return nil, errors.Join(
					fmt.Errorf("reading pending database cleanup %q: %w", entry.Name(), err),
					fmt.Errorf("quarantining invalid pending database cleanup: %w", quarantineErr),
				)
			}
			continue
		}
		if entry.Name() != canonicalDatabaseName(receipt.Database)+".json" {
			if err := moveDatabaseCleanupReceiptToQuarantine(filepath.Join(dir, entry.Name())); err != nil {
				return nil, fmt.Errorf("quarantining mismatched pending database cleanup %q: %w", entry.Name(), err)
			}
			continue
		}
		names = append(names, receipt.Database)
	}
	return names, nil
}

func resumeRunningDatabaseRemoval(townRoot, dbName, dbPath, receiptPath string, receipt databaseCleanupReceipt) error {
	controlDB, targetLive, err := liveBranchControlDatabase(townRoot, dbName)
	if err != nil {
		return err
	}
	allowMissing := receipt.Phase == databaseCleanupDropAttempted
	claimExists := false
	if receipt.Phase == databaseCleanupPrepared {
		if _, err := os.Stat(filepath.Join(databaseCleanupClaimPath(townRoot, dbName, receipt.Incarnation), ".dolt")); err == nil {
			claimExists = true
			allowMissing = true
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking claimed database directory: %w", err)
		}
	}
	if err := ensureDatabaseCleanupTarget(townRoot, dbName, dbPath, receipt.Incarnation, allowMissing); err != nil {
		return quarantineDatabaseCleanupReceipt(receiptPath, err)
	}
	if !targetLive && receipt.Phase == databaseCleanupPrepared && !receipt.Force {
		return quarantineDatabaseCleanupReceipt(receiptPath,
			fmt.Errorf("database %q is not loaded by the live server; cannot inspect user tables", dbName))
	}
	if targetLive && !receipt.Force {
		hasData, inspectErr := databaseHasUserTables(townRoot, dbName)
		if inspectErr != nil {
			return fmt.Errorf("inspecting user tables in database %q: %w", dbName, inspectErr)
		}
		if hasData {
			return quarantineDatabaseCleanupReceipt(receiptPath,
				fmt.Errorf("database %q has user tables — use --force to remove", dbName))
		}
	}
	if controlDB == "" {
		err := fmt.Errorf("no surviving live database available for branch-control cleanup of %q", dbName)
		if receipt.Phase == databaseCleanupPrepared && !claimExists {
			return clearUnmutatedDatabaseCleanupReceipt(receiptPath, err)
		}
		return err
	}

	controlIdentifier := strings.ReplaceAll(controlDB, "`", "``")
	if err := serverExecSQL(townRoot, fmt.Sprintf("USE `%s`; SELECT 1", controlIdentifier)); err != nil {
		preflightErr := fmt.Errorf("preflighting live branch-control database %q: %w", controlDB, err)
		if receipt.Phase == databaseCleanupPrepared && !claimExists {
			return clearUnmutatedDatabaseCleanupReceipt(receiptPath, preflightErr)
		}
		return preflightErr
	}

	if targetLive {
		if receipt.Phase == databaseCleanupPrepared {
			receipt.Phase = databaseCleanupDropAttempted
			if err := writeDatabaseCleanupReceipt(receiptPath, receipt); err != nil {
				return fmt.Errorf("persisting DROP-attempted cleanup phase for %q: %w", dbName, err)
			}
		}
		if err := ensureDatabaseCleanupTarget(townRoot, dbName, dbPath, receipt.Incarnation, false); err != nil {
			return quarantineDatabaseCleanupReceipt(receiptPath, err)
		}
		identifier := strings.ReplaceAll(dbName, "`", "``")
		if dropErr := serverExecSQL(townRoot, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", identifier)); dropErr != nil {
			if IsReadOnlyError(dropErr.Error()) {
				return fmt.Errorf("DROP put server into read-only mode; database cleanup receipt preserved: %w", dropErr)
			}
			if !isDatabaseNotFoundError(dropErr) {
				return fmt.Errorf("dropping database %q; database cleanup receipt preserved: %w", dbName, dropErr)
			}
		}
		InvalidateDBCache()
	}
	if err := ensureDatabaseCleanupTarget(townRoot, dbName, dbPath, receipt.Incarnation, true); err != nil {
		return quarantineDatabaseCleanupReceipt(receiptPath, err)
	}

	// Dolt does not remove branch-control rows during DROP. Keeping a durable
	// receipt until this idempotent DELETE succeeds makes a committed DROP
	// resumable after transport or control-database failure.
	branchQuery := fmt.Sprintf("USE `%s`; DELETE FROM dolt_branch_control WHERE `database` = '%s'", controlIdentifier, EscapeSQL(dbName))
	if err := serverExecSQL(townRoot, branchQuery); err != nil {
		return fmt.Errorf("cleaning branch-control entries for database %q; database cleanup receipt preserved: %w", dbName, err)
	}
	if err := ensureDatabaseCleanupTarget(townRoot, dbName, dbPath, receipt.Incarnation, true); err != nil {
		return quarantineDatabaseCleanupReceipt(receiptPath, err)
	}
	claimedPath, err := claimDatabaseCleanupDirectory(townRoot, dbName, dbPath, receipt.Incarnation)
	if err != nil {
		return fmt.Errorf("claiming database directory; database cleanup receipt preserved: %w", err)
	}
	if claimedPath != "" {
		if err := ensureDatabaseCleanupUnreferenced(townRoot, dbName); err != nil {
			return restoreClaimedDatabaseAndQuarantineReceipt(dbPath, claimedPath, receiptPath, err)
		}
		claimedIncarnation, err := databaseCleanupIncarnation(townRoot, dbName, claimedPath, false)
		if err != nil || claimedIncarnation != receipt.Incarnation {
			return restoreClaimedDatabaseAndQuarantineReceipt(dbPath, claimedPath, receiptPath,
				fmt.Errorf("claimed database %q no longer matches cleanup incarnation", dbName))
		}
		if err := os.RemoveAll(claimedPath); err != nil {
			return fmt.Errorf("removing claimed database directory; database cleanup receipt preserved: %w", err)
		}
		if err := syncDatabaseCleanupDirectory(filepath.Dir(claimedPath)); err != nil {
			return fmt.Errorf("syncing removed database directory; database cleanup receipt preserved: %w", err)
		}
	}
	if err := clearDatabaseCleanupReceipt(receiptPath); err != nil {
		return fmt.Errorf("database removed but cleanup receipt could not be cleared: %w", err)
	}
	InvalidateDBCache()
	return nil
}

func resumeOfflineDatabaseRemoval(townRoot, dbName, dbPath, receiptPath string, receipt databaseCleanupReceipt) error {
	if err := ensureDatabaseCleanupTarget(townRoot, dbName, dbPath, receipt.Incarnation, true); err != nil {
		return quarantineDatabaseCleanupReceipt(receiptPath, err)
	}
	claimedPath, err := claimDatabaseCleanupDirectory(townRoot, dbName, dbPath, receipt.Incarnation)
	if err != nil {
		return fmt.Errorf("claiming offline database directory; cleanup receipt preserved: %w", err)
	}
	if claimedPath == "" {
		if err := clearDatabaseCleanupReceipt(receiptPath); err != nil {
			return fmt.Errorf("database already removed but cleanup receipt could not be cleared: %w", err)
		}
		InvalidateDBCache()
		return nil
	}
	if err := ensureDatabaseCleanupUnreferenced(townRoot, dbName); err != nil {
		return restoreClaimedDatabaseAndQuarantineReceipt(dbPath, claimedPath, receiptPath, err)
	}
	claimedIncarnation, err := databaseCleanupIncarnation(townRoot, dbName, claimedPath, false)
	if err != nil || claimedIncarnation != receipt.Incarnation {
		return restoreClaimedDatabaseAndQuarantineReceipt(dbPath, claimedPath, receiptPath,
			fmt.Errorf("claimed database %q no longer matches cleanup incarnation", dbName))
	}
	if err := os.RemoveAll(claimedPath); err != nil {
		return fmt.Errorf("removing claimed offline database directory; cleanup receipt preserved: %w", err)
	}
	if err := syncDatabaseCleanupDirectory(filepath.Dir(claimedPath)); err != nil {
		return fmt.Errorf("syncing removed offline database directory; cleanup receipt preserved: %w", err)
	}
	if err := clearDatabaseCleanupReceipt(receiptPath); err != nil {
		return fmt.Errorf("offline database removed but cleanup receipt could not be cleared: %w", err)
	}
	InvalidateDBCache()
	return nil
}

func liveBranchControlDatabase(townRoot, removedDB string) (controlDB string, targetLive bool, err error) {
	databases, err := listDatabasesRemote(DefaultConfig(townRoot))
	if err != nil {
		return "", false, fmt.Errorf("listing live databases for branch-control cleanup: %w", err)
	}
	for _, database := range databases {
		if strings.EqualFold(database, removedDB) {
			targetLive = true
			continue
		}
		if controlDB == "" {
			controlDB = database
		}
	}
	return controlDB, targetLive, nil
}

func ensureDatabaseCleanupUnreferenced(townRoot, dbName string) error {
	referenced, err := collectReferencedDatabases(townRoot)
	if err != nil {
		return fmt.Errorf("reading database ownership metadata: %w", err)
	}
	if referenced[canonicalDatabaseName(dbName)] {
		return fmt.Errorf("database %q is referenced by current Gas Town ownership metadata", dbName)
	}
	return nil
}

func ensureDatabaseCleanupTarget(townRoot, dbName, dbPath, wantIncarnation string, allowMissing bool) error {
	if err := ensureDatabaseCleanupUnreferenced(townRoot, dbName); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dbPath, ".dolt")); err != nil {
		if os.IsNotExist(err) && allowMissing {
			claimedPath := databaseCleanupClaimPath(townRoot, dbName, wantIncarnation)
			if _, claimErr := os.Stat(filepath.Join(claimedPath, ".dolt")); claimErr != nil {
				if os.IsNotExist(claimErr) {
					return nil
				}
				return fmt.Errorf("database %q claimed cleanup target is unavailable: %w", dbName, claimErr)
			}
			gotIncarnation, claimErr := databaseCleanupIncarnation(townRoot, dbName, claimedPath, false)
			if claimErr != nil {
				return fmt.Errorf("identifying claimed database %q during cleanup: %w", dbName, claimErr)
			}
			if gotIncarnation != wantIncarnation {
				return fmt.Errorf("claimed database %q no longer matches cleanup incarnation", dbName)
			}
			return nil
		}
		return fmt.Errorf("database %q cleanup target is unavailable: %w", dbName, err)
	}
	running, _, err := IsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking Dolt server while validating database %q: %w", dbName, err)
	}
	targetLive := false
	if running {
		databases, listErr := listDatabasesRemote(DefaultConfig(townRoot))
		if listErr != nil {
			return fmt.Errorf("listing live databases while validating database %q: %w", dbName, listErr)
		}
		for _, database := range databases {
			if strings.EqualFold(database, dbName) {
				targetLive = true
				break
			}
		}
	}
	gotIncarnation, err := databaseCleanupIncarnation(townRoot, dbName, dbPath, targetLive)
	if err != nil {
		return fmt.Errorf("identifying database %q during cleanup: %w", dbName, err)
	}
	if gotIncarnation != wantIncarnation {
		return fmt.Errorf("database %q no longer matches cleanup incarnation", dbName)
	}
	return nil
}

func databaseCleanupIncarnation(townRoot, dbName, dbPath string, targetLive bool) (string, error) {
	const rootCommitQuery = "SELECT commit_hash AS incarnation FROM dolt_log WHERE parents = '' ORDER BY commit_hash LIMIT 1"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if targetLive {
		identifier := strings.ReplaceAll(dbName, "`", "``")
		cmd = buildServerSQLCmd(ctx, DefaultConfig(townRoot), "-r", "json", "-q", fmt.Sprintf("USE `%s`; %s", identifier, rootCommitQuery))
	} else {
		cmd = exec.CommandContext(ctx, "dolt", "sql", "-r", "json", "-q", rootCommitQuery)
		cmd.Dir = dbPath
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	rootIdentity := ""
	if err != nil {
		if !targetLive {
			manifest, readErr := os.ReadFile(filepath.Join(dbPath, ".dolt", "noms", "manifest"))
			if readErr != nil {
				return "", fmt.Errorf("querying root commit: %w (%s); reading Dolt manifest: %v", err, strings.TrimSpace(stderr.String()), readErr)
			}
			sum := sha256.Sum256(manifest)
			rootIdentity = fmt.Sprintf("manifest-root:%x", sum[:])
		} else {
			return "", fmt.Errorf("querying root commit: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
	} else {
		var result struct {
			Rows []struct {
				Incarnation string `json:"incarnation"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			return "", fmt.Errorf("parsing root commit JSON: %w", err)
		}
		if len(result.Rows) != 1 || len(result.Rows[0].Incarnation) != 32 {
			return "", fmt.Errorf("expected one 32-character root commit")
		}
		rootIdentity = "dolt-root:" + result.Rows[0].Incarnation
	}
	manifest, err := os.ReadFile(filepath.Join(dbPath, ".dolt", "noms", "manifest"))
	if err != nil {
		return "", fmt.Errorf("reading Dolt manifest: %w", err)
	}
	state := sha256.Sum256(manifest)
	return fmt.Sprintf("%s/manifest-sha256:%x", rootIdentity, state[:]), nil
}

func databaseCleanupRootIdentity(incarnation string) string {
	root, _, _ := strings.Cut(incarnation, "/")
	return root
}

func databaseCleanupClaimPath(townRoot, dbName, incarnation string) string {
	sum := sha256.Sum256([]byte(incarnation))
	return filepath.Join(townRoot, ".runtime", "dolt-database-cleanup", "claimed", fmt.Sprintf("%s-%x", canonicalDatabaseName(dbName), sum[:8]))
}

func claimDatabaseCleanupDirectory(townRoot, dbName, dbPath, incarnation string) (string, error) {
	claimPath := databaseCleanupClaimPath(townRoot, dbName, incarnation)
	_, claimErr := os.Stat(claimPath)
	_, dbErr := os.Stat(filepath.Join(dbPath, ".dolt"))
	if claimErr == nil {
		if dbErr == nil {
			return "", fmt.Errorf("both current and claimed directories exist for database %q", dbName)
		}
		if !os.IsNotExist(dbErr) {
			return "", dbErr
		}
		return claimPath, nil
	}
	if !os.IsNotExist(claimErr) {
		return "", claimErr
	}
	if os.IsNotExist(dbErr) {
		return "", nil
	}
	if dbErr != nil {
		return "", dbErr
	}
	claimDir := filepath.Dir(claimPath)
	if err := ensurePrivateDatabaseCleanupDirectory(claimDir); err != nil {
		return "", err
	}
	if err := os.Rename(dbPath, claimPath); err != nil {
		return "", err
	}
	if err := syncDatabaseCleanupDirectory(filepath.Dir(dbPath)); err != nil {
		return "", err
	}
	if err := syncDatabaseCleanupDirectory(claimDir); err != nil {
		return "", err
	}
	return claimPath, nil
}

func restoreClaimedDatabaseCleanupDirectory(dbPath, claimedPath string) error {
	if _, err := os.Stat(dbPath); err == nil {
		return fmt.Errorf("refusing to overwrite replacement database at %s", dbPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(claimedPath, dbPath); err != nil {
		return err
	}
	return errors.Join(
		syncDatabaseCleanupDirectory(filepath.Dir(dbPath)),
		syncDatabaseCleanupDirectory(filepath.Dir(claimedPath)),
	)
}

func restoreClaimedDatabaseAndQuarantineReceipt(dbPath, claimedPath, receiptPath string, reason error) error {
	if err := restoreClaimedDatabaseCleanupDirectory(dbPath, claimedPath); err != nil {
		return errors.Join(reason, fmt.Errorf("restoring claimed database; cleanup receipt preserved: %w", err))
	}
	return quarantineDatabaseCleanupReceipt(receiptPath, reason)
}

func databaseCleanupReceiptPath(townRoot, dbName string) string {
	return filepath.Join(townRoot, ".runtime", "dolt-database-cleanup", canonicalDatabaseName(dbName)+".json")
}

func writeDatabaseCleanupReceipt(path string, receipt databaseCleanupReceipt) error {
	if err := ensurePrivateDatabaseCleanupDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return writeDatabaseCleanupFileDurable(path, append(data, '\n'), 0o600)
}

func ensurePrivateDatabaseCleanupDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if grandparent := filepath.Dir(parent); grandparent != parent {
		if err := syncDatabaseCleanupDirectory(grandparent); err != nil {
			return err
		}
	}
	return syncDatabaseCleanupDirectory(parent)
}

func writeDatabaseCleanupFileDurable(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if err := databaseCleanupFileSync(tmp); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncDatabaseCleanupDirectory(dir)
}

func syncDatabaseCleanupDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return databaseCleanupDirSync(dir)
}

func readDatabaseCleanupReceipt(path string) (databaseCleanupReceipt, error) {
	f, err := os.Open(path)
	if err != nil {
		return databaseCleanupReceipt{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return databaseCleanupReceipt{}, fmt.Errorf("database cleanup receipt is not a private regular file")
	}
	var receipt databaseCleanupReceipt
	if err := json.NewDecoder(f).Decode(&receipt); err != nil {
		return databaseCleanupReceipt{}, err
	}
	incarnationParts := strings.Split(receipt.Incarnation, "/manifest-sha256:")
	validIncarnation := false
	if len(incarnationParts) == 2 && len(incarnationParts[1]) == 64 {
		rootCommit := strings.TrimPrefix(incarnationParts[0], "dolt-root:")
		manifestRoot := strings.TrimPrefix(incarnationParts[0], "manifest-root:")
		validIncarnation = (len(rootCommit) == 32 && rootCommit != incarnationParts[0]) ||
			(len(manifestRoot) == 64 && manifestRoot != incarnationParts[0])
	}
	validPhase := receipt.Phase == databaseCleanupPrepared || receipt.Phase == databaseCleanupDropAttempted ||
		receipt.Phase == databaseCleanupOfflineReady
	if receipt.Version != databaseCleanupReceiptVersion || !validSQLName(receipt.Database) || receipt.Database == "." || receipt.Database == ".." ||
		!validPhase || !validIncarnation {
		return databaseCleanupReceipt{}, fmt.Errorf("invalid database cleanup receipt")
	}
	return receipt, nil
}

func quarantineDatabaseCleanupReceipt(path string, reason error) error {
	if err := moveDatabaseCleanupReceiptToQuarantine(path); err != nil {
		return errors.Join(reason, fmt.Errorf("quarantining stale database cleanup receipt: %w", err))
	}
	return fmt.Errorf("%w; stale cleanup receipt quarantined", reason)
}

func moveDatabaseCleanupReceiptToQuarantine(path string) error {
	quarantinePath := fmt.Sprintf("%s.%d.quarantine", path, time.Now().UnixNano())
	if err := os.Rename(path, quarantinePath); err != nil {
		return err
	}
	return syncDatabaseCleanupDirectory(filepath.Dir(path))
}

func clearUnmutatedDatabaseCleanupReceipt(path string, operationErr error) error {
	if err := clearDatabaseCleanupReceipt(path); err != nil {
		return errors.Join(operationErr, fmt.Errorf("clearing unmutated database cleanup receipt: %w", err))
	}
	return operationErr
}

func clearDatabaseCleanupReceipt(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDatabaseCleanupDirectory(filepath.Dir(path))
}

func isDatabaseNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown database") || strings.Contains(msg, "database not found:")
}

// databaseHasUserTables checks if a database has tables beyond Dolt system tables.
// Returns (true, nil) if user tables exist, (false, nil) if only system tables or empty.
func databaseHasUserTables(townRoot, dbName string) (bool, error) {
	config := DefaultConfig(townRoot)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := fmt.Sprintf("USE `%s`; SHOW TABLES", dbName)
	// USE <db>; SHOW TABLES needs the running server's catalog to resolve <db>;
	// embedded mode would only see the on-disk layout (see #3518 for precedent).
	cmd := buildServerSQLCmd(ctx, config, "-r", "csv", "-q", query)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, err
	}

	// Parse output — each line is a table name. Skip Dolt system tables.
	for _, line := range strings.Split(string(output), "\n") {
		table := strings.TrimSpace(line)
		if table == "" || table == "Tables_in_"+dbName || table == "Table" {
			continue
		}
		// Dolt system tables start with "dolt_"
		if !strings.HasPrefix(table, "dolt_") {
			return true, nil
		}
	}
	return false, nil
}

// FindBrokenWorkspaces scans all rig metadata.json files for Dolt server
// configuration where the referenced database doesn't exist in .dolt-data/
// or exists on disk but isn't served by the running Dolt server.
// These workspaces are broken: bd commands will fail or silently create
// isolated local databases instead of connecting to the centralized server.
func FindBrokenWorkspaces(townRoot string) ([]BrokenWorkspace, string) {
	var broken []BrokenWorkspace
	var warning string

	// Query the running server once for all served databases.
	// If the server isn't running, servedDBs will be nil and we
	// fall back to filesystem-only checks (previous behavior).
	var servedDBs map[string]bool
	if running, _, _ := IsRunning(townRoot); running {
		if served, _, err := VerifyDatabasesWithRetry(townRoot, 3); err == nil {
			servedDBs = make(map[string]bool, len(served))
			for _, db := range served {
				servedDBs[strings.ToLower(db)] = true
			}
		} else {
			warning = fmt.Sprintf("Warning: Dolt server is running but could not verify databases: %v\n"+
				"Server-aware checks are disabled; only filesystem checks will be performed.", err)
		}
	}

	// Check town-level beads (hq)
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if ws := checkWorkspace(townRoot, "hq", townBeadsDir, servedDBs); ws != nil {
		broken = append(broken, *ws)
	}

	// Check rig-level beads via rigs.json
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	data, err := os.ReadFile(rigsPath)
	if err != nil {
		return broken, warning
	}
	var config struct {
		Rigs map[string]interface{} `json:"rigs"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return broken, warning
	}

	for rigName := range config.Rigs {
		beadsDir := FindRigBeadsDir(townRoot, rigName)
		if beadsDir == "" {
			continue
		}
		if ws := checkWorkspace(townRoot, rigName, beadsDir, servedDBs); ws != nil {
			broken = append(broken, *ws)
		}
	}

	return broken, warning
}

// checkWorkspace checks a single rig's metadata.json for broken Dolt configuration.
// Returns nil if the workspace is healthy or not configured for Dolt server mode.
func checkWorkspace(townRoot, rigName, beadsDir string, servedDBs map[string]bool) *BrokenWorkspace {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil
	}

	var metadata struct {
		DoltMode     string `json:"dolt_mode"`
		DoltDatabase string `json:"dolt_database"`
		Backend      string `json:"backend"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil
	}

	// Only check workspaces configured for Dolt server mode
	if metadata.DoltMode != "server" || metadata.Backend != "dolt" {
		return nil
	}

	dbName := metadata.DoltDatabase
	if dbName == "" {
		dbName = rigName
	}

	existsOnDisk := DatabaseExists(townRoot, dbName)

	// If the server is running (servedDBs != nil), also check that the
	// database is actually being served. A database can exist on disk but
	// not be served if the server was started from a different data
	// directory or needs a restart after migration.
	if existsOnDisk {
		if servedDBs != nil && !servedDBs[strings.ToLower(dbName)] {
			return &BrokenWorkspace{
				RigName:      rigName,
				BeadsDir:     beadsDir,
				ConfiguredDB: dbName,
				NotServed:    true,
			}
		}
		return nil // healthy: exists on disk and (served or server not checked)
	}

	ws := &BrokenWorkspace{
		RigName:      rigName,
		BeadsDir:     beadsDir,
		ConfiguredDB: dbName,
	}

	// Check for local data that could be migrated
	localDoltPath := findLocalDoltDB(beadsDir)
	if localDoltPath != "" {
		ws.HasLocalData = true
		ws.LocalDataPath = localDoltPath
	}

	return ws
}

// RepairWorkspace fixes a broken workspace by creating the missing database
// or migrating local data if present. Returns a description of what was done.
func RepairWorkspace(townRoot string, ws BrokenWorkspace) (string, error) {
	if ws.HasLocalData {
		// Migrate local data to centralized location
		if err := MigrateRigFromBeads(townRoot, ws.ConfiguredDB, ws.LocalDataPath); err != nil {
			return "", fmt.Errorf("migrating local data for %s: %w", ws.RigName, err)
		}
		return fmt.Sprintf("migrated local data from %s", ws.LocalDataPath), nil
	}

	// No local data — create a fresh database
	_, created, err := InitRig(townRoot, ws.ConfiguredDB)
	if err != nil {
		return "", fmt.Errorf("creating database for %s: %w", ws.RigName, err)
	}
	if !created {
		return "database already exists (no-op)", nil
	}
	return "created new database", nil
}

// EnsureMetadata writes or updates the metadata.json for a rig's beads directory
// to include proper Dolt server configuration. This prevents the split-brain problem
// where bd falls back to local embedded databases instead of connecting to the
// centralized Dolt server.
//
// For the "hq" rig, it writes to <townRoot>/.beads/metadata.json.
// For other rigs, it writes to mayor/rig/.beads/metadata.json if that path exists,
// otherwise to <townRoot>/<rigName>/.beads/metadata.json.
// EnsureMetadata ensures that the .beads/metadata.json for a rig has correct
// Dolt server configuration.  rigName is the rig's directory name (e.g.
// "beads_el"). When dolt_database is absent the default is rigName, which is
// correct for rigs whose Dolt database name matches their directory name.
// Callers that know the rig uses a short DB prefix (e.g. "be" for "beads_el")
// should pass it as doltDatabase so metadata.json gets the right value.
func EnsureMetadata(townRoot, rigName string, doltDatabase ...string) error {
	// Use FindOrCreateRigBeadsDir to atomically resolve and create the directory,
	// avoiding the TOCTOU race where the directory state changes between
	// FindRigBeadsDir's Stat check and our subsequent file operations.
	beadsDir, err := FindOrCreateRigBeadsDir(townRoot, rigName)
	if err != nil {
		return fmt.Errorf("resolving beads directory for rig %q: %w", rigName, err)
	}

	return EnsureMetadataForBeadsDir(townRoot, beadsDir, rigName, doltDatabase...)
}

// EnsureMetadataForBeadsDir writes or updates metadata.json in a known beads
// directory. It only writes local config and does not require the Dolt server or
// database to exist, so callers can leave a recoverable rig behind after partial
// initialization failures.
func EnsureMetadataForBeadsDir(townRoot, beadsDir, rigName string, doltDatabase ...string) error {
	return WithDatabaseOwnershipTransaction(townRoot, func() error {
		return ensureMetadataForBeadsDirLocked(townRoot, beadsDir, rigName, doltDatabase...)
	})
}

func ensureMetadataForBeadsDirLocked(townRoot, beadsDir, rigName string, doltDatabase ...string) error {
	if beadsDir == "" {
		return fmt.Errorf("beads directory is required")
	}
	if rigName == "" {
		return fmt.Errorf("rig name is required")
	}

	// Determine the Dolt database name to write when the field is absent.
	// Default: rigName (correct when db-name == rig-dir-name, e.g. "gastown").
	// Callers from EnsureAllMetadata pass the actual DB prefix ("at", "be") so
	// that rigs with short prefixes get the correct database name, not the full
	// rig directory name.
	explicitDB := len(doltDatabase) > 0 && doltDatabase[0] != ""
	effectiveDB := rigName
	if explicitDB {
		effectiveDB = doltDatabase[0]
	}

	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		return fmt.Errorf("creating beads directory: %w", err)
	}

	metadataPath := filepath.Join(beadsDir, "metadata.json")

	// Acquire per-path mutex for goroutine synchronization.
	// EnsureAllMetadata calls EnsureMetadata concurrently; flock (inter-process)
	// cannot reliably synchronize goroutines within the same process.
	mu := getMetadataMu(metadataPath)
	mu.Lock()
	defer mu.Unlock()

	// Load existing metadata if present (preserve any extra fields)
	existing := make(map[string]interface{})
	if data, err := os.ReadFile(metadataPath); err == nil {
		_ = json.Unmarshal(data, &existing) // best effort
	}

	// Resolve the authoritative server config (config.yaml > env > daemon.json > default).
	config := DefaultConfig(townRoot)

	// Patch dolt server fields. Only write when values actually change so tracked
	// metadata.json files in source repos stay clean.
	changed := false
	if existing["database"] != "dolt" {
		existing["database"] = "dolt"
		changed = true
	}
	if existing["backend"] != "dolt" {
		existing["backend"] = "dolt"
		changed = true
	}
	if existing["dolt_mode"] != "server" {
		existing["dolt_mode"] = "server"
		changed = true
	}
	// Fix wrong dolt_database values (not just empty). After a crash or rig
	// addition, metadata.json can end up pointing to the wrong database name
	// (e.g., "beads_gt" instead of "gastown"), causing PROJECT IDENTITY MISMATCH
	// errors that are hard to diagnose and recover from. (gas-tc4)
	if existing["dolt_database"] == nil || existing["dolt_database"] == "" {
		existing["dolt_database"] = effectiveDB
		changed = true
	} else if dbStr, ok := existing["dolt_database"].(string); ok && dbStr != effectiveDB {
		// The existing value differs from what we'd write. When the caller
		// provided an explicit dbName (from EnsureAllMetadata, which resolves
		// the canonical name from rigs.json), always correct. When no explicit
		// dbName was given (effectiveDB == rigName), only correct if the
		// existing value is not a real database — this prevents flip-flop
		// between "at" and "atomize" when two code paths disagree. (gt-9c4)
		if explicitDB || !DatabaseExists(townRoot, dbStr) {
			fmt.Fprintf(os.Stderr, "Warning: metadata.json dolt_database was %q, correcting to %q (identity mismatch repair)\n", dbStr, effectiveDB)
			existing["dolt_database"] = effectiveDB
			changed = true
		}
	}

	// Ensure server connection fields match the authoritative config.
	// bd reads dolt_server_host and dolt_server_port from metadata.json to
	// connect to the Dolt server. Stale values (e.g., port 13729 from a
	// previous bd init) cause "connection refused" errors.
	wantHost := config.EffectiveHost()
	wantPort := float64(config.Port) // JSON numbers are float64
	if existing["dolt_server_host"] != wantHost {
		existing["dolt_server_host"] = wantHost
		changed = true
	}
	if existing["dolt_server_port"] != wantPort {
		existing["dolt_server_port"] = wantPort
		changed = true
	}

	// Fast path: avoid rewriting metadata.json when already correct.
	if !changed {
		return nil
	}

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling metadata: %w", err)
	}

	if err := atomicfile.WriteFile(metadataPath, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("writing metadata.json: %w", err)
	}

	return nil
}

// buildRigPrefixMap reads rigs.json and returns a map from Dolt database name
// (beads prefix without the trailing hyphen) to the rig directory name.
// Example: {"be": "beads_el", "sw": "sooper_whisper"}.
// Rigs where the database name equals the directory name are not included.
func buildRigPrefixMap(townRoot string) map[string]string {
	result := make(map[string]string)
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	data, err := os.ReadFile(rigsPath)
	if err != nil {
		return result
	}
	var parsed struct {
		Rigs map[string]struct {
			Beads struct {
				Prefix string `json:"prefix"`
			} `json:"beads"`
		} `json:"rigs"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return result
	}
	for rigName, info := range parsed.Rigs {
		prefix := strings.TrimSuffix(info.Beads.Prefix, "-")
		if prefix != "" && prefix != rigName {
			result[prefix] = rigName
		}
	}
	return result
}

// EnsureAllMetadata updates metadata.json for all rig databases known to the
// Dolt server. This is the fix for the split-brain problem where worktrees
// each have their own isolated database.
//
// For rigs that use a short DB prefix (e.g. database "be" for the "beads_el"
// rig), EnsureAllMetadata resolves the rig name from rigs.json and writes the
// correct dolt_database value ("be") so that convoy event polling connects to
// the right database instead of a non-existent "beads_el" database.
func EnsureAllMetadata(townRoot string) (updated []string, errs []error) {
	databases, err := ListDatabases(townRoot)
	if err != nil {
		return nil, []error{fmt.Errorf("listing databases: %w", err)}
	}

	// Map from DB prefix to rig directory name, e.g. "be" -> "beads_el".
	// Merge routes.jsonl (routes) and rigs.json (prefixes); rigs.json wins on
	// conflict. Rigs where db-name == rig-dir-name are not in this map and fall
	// through to the default behavior (rigName = dbName).
	dbToRig := buildDatabaseToRigMap(townRoot)
	for k, v := range buildRigPrefixMap(townRoot) {
		dbToRig[k] = v
	}

	// Group candidate database names by rig. When routes.jsonl and rigs.json
	// use different prefixes for the same rig (e.g. "gas" vs "gt" both map to
	// "gastown"), multiple databases may exist for the same rig. Processing
	// them all causes oscillation: each one overwrites the other's
	// dolt_database correction on every startup. (gas-ar0)
	rigCandidates := make(map[string][]string) // rig -> candidate db names
	for _, dbName := range databases {
		rigName := dbName
		if mapped, ok := dbToRig[dbName]; ok {
			rigName = mapped
		}
		if dbName == "hq" {
			rigName = "hq"
		}
		rigCandidates[rigName] = append(rigCandidates[rigName], dbName)
	}

	for rigName, candidates := range rigCandidates {
		// When multiple databases map to the same rig, choose one effective
		// DB name: prefer whatever is already in metadata.json (if it's among
		// the valid candidates) to avoid spurious mismatch warnings. Fall back
		// to the first candidate (alphabetical, from os.ReadDir ordering).
		dbName := candidates[0]
		if len(candidates) > 1 {
			dbName = pickDBForRig(townRoot, rigName, candidates)
		}
		// Pass dbName explicitly so EnsureMetadata writes the correct
		// dolt_database value ("be") rather than the rig dir name ("beads_el").
		if err := EnsureMetadata(townRoot, rigName, dbName); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dbName, err))
		} else {
			updated = append(updated, dbName)
		}
	}

	return updated, errs
}

// pickDBForRig selects which database name to use for a rig when multiple
// candidates exist. Prefers the value already in metadata.json to avoid
// oscillating corrections between two valid aliases for the same rig.
func pickDBForRig(townRoot, rigName string, candidates []string) string {
	beadsDir := FindRigBeadsDir(townRoot, rigName)
	if beadsDir != "" {
		if data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json")); err == nil {
			var meta map[string]interface{}
			if json.Unmarshal(data, &meta) == nil {
				if existingDB, _ := meta["dolt_database"].(string); existingDB != "" {
					for _, c := range candidates {
						if c == existingDB {
							return c // Already correct — no repair needed
						}
					}
				}
			}
		}
	}
	return candidates[0] // Default: first (alphabetical from os.ReadDir)
}

// buildDatabaseToRigMap loads routes.jsonl and builds a map from database name
// (prefix without hyphen) to rig name (first component of the path).
// For example: "bd" -> "beads", "gt" -> "gastown", "sw" -> "sallaWork"
func buildDatabaseToRigMap(townRoot string) map[string]string {
	result := make(map[string]string)
	beadsDir := filepath.Join(townRoot, ".beads")
	routes, err := beads.LoadRoutes(beadsDir)
	if err != nil {
		return result // Return empty map on error
	}
	for _, route := range routes {
		// Extract rig name from path (first component before "/")
		// e.g., "beads/mayor/rig" -> "beads", "gastown/mayor/rig" -> "gastown"
		prefix := strings.TrimSuffix(route.Prefix, "-")
		parts := strings.Split(route.Path, "/")
		if len(parts) > 0 && parts[0] != "" && parts[0] != "." {
			result[prefix] = parts[0]
		}
	}
	return result
}

// FindRigBeadsDir returns the .beads directory path for a rig (read-only lookup).
// For "hq", returns <townRoot>/.beads.
// For other rigs, returns <townRoot>/<rigName>/mayor/rig/.beads if it exists,
// otherwise <townRoot>/<rigName>/.beads if it exists,
// otherwise <townRoot>/<rigName>/mayor/rig/.beads (for creation by caller).
//
// WARNING: This function has a TOCTOU race — the returned directory may change
// state between the Stat check and the caller's operation. For write operations
// that need the directory to exist, use FindOrCreateRigBeadsDir instead.
// For read-only operations, handle errors on the returned path gracefully.
func FindRigBeadsDir(townRoot, rigName string) string {
	if townRoot == "" || rigName == "" {
		return ""
	}
	if rigName == "hq" {
		return filepath.Join(townRoot, ".beads")
	}

	// Prefer mayor/rig/.beads (canonical location for tracked beads)
	mayorBeads := filepath.Join(townRoot, rigName, "mayor", "rig", ".beads")
	if _, err := os.Stat(mayorBeads); err == nil {
		return mayorBeads
	}

	// Fall back to rig-root .beads
	rigBeads := filepath.Join(townRoot, rigName, ".beads")
	if _, err := os.Stat(rigBeads); err == nil {
		return rigBeads
	}

	// Neither exists; return rig-root path (consistent with FindOrCreateRigBeadsDir)
	return rigBeads
}

// FindOrCreateRigBeadsDir atomically resolves and ensures the .beads directory
// exists for a rig. Unlike FindRigBeadsDir, this combines directory resolution
// with creation to avoid TOCTOU races where the directory state changes between
// the existence check and the caller's write operation.
//
// Use this for write operations (EnsureMetadata, etc.) where the directory must
// exist. Use FindRigBeadsDir for read-only lookups where graceful failure on
// missing directories is acceptable.
func FindOrCreateRigBeadsDir(townRoot, rigName string) (string, error) {
	if townRoot == "" {
		return "", fmt.Errorf("townRoot cannot be empty")
	}
	if rigName == "" {
		return "", fmt.Errorf("rigName cannot be empty")
	}
	if rigName == "hq" {
		dir := filepath.Join(townRoot, ".beads")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("creating HQ beads dir: %w", err)
		}
		return dir, nil
	}

	// Check mayor/rig/.beads first (canonical location).
	// Use MkdirAll as an idempotent existence check+create to close the
	// TOCTOU window between os.Stat and the caller's file operations.
	mayorBeads := filepath.Join(townRoot, rigName, "mayor", "rig", ".beads")
	if _, err := os.Stat(mayorBeads); err == nil {
		// Ensure it still exists (no-op if present, recreates if deleted)
		if err := os.MkdirAll(mayorBeads, 0755); err != nil {
			return "", fmt.Errorf("ensuring mayor beads dir: %w", err)
		}
		return mayorBeads, nil
	}

	// Check rig-root .beads
	rigBeads := filepath.Join(townRoot, rigName, ".beads")
	if _, err := os.Stat(rigBeads); err == nil {
		if err := os.MkdirAll(rigBeads, 0755); err != nil {
			return "", fmt.Errorf("ensuring rig beads dir: %w", err)
		}
		return rigBeads, nil
	}

	// Neither exists — create rig-root .beads (NOT mayor path).
	// The mayor/rig/.beads path should only be used when the source repo
	// has tracked beads (checked out via git clone). Creating it here would
	// cause InitBeads to misdetect an untracked repo as having tracked beads,
	// taking the redirect early-return and skipping config.yaml creation
	// (see rig/manager.go InitBeads).
	if err := os.MkdirAll(rigBeads, 0755); err != nil {
		return "", fmt.Errorf("creating beads dir: %w", err)
	}

	return rigBeads, nil
}

// GetActiveConnectionCount queries the Dolt server to get the number of active connections.
// Uses `dolt sql` to query information_schema.PROCESSLIST, which avoids needing
// a MySQL driver dependency. Returns 0 if the server is unreachable or the query fails.
func GetActiveConnectionCount(townRoot string) (int, error) {
	config := DefaultConfig(townRoot)

	// Use dolt sql-client to query the server with a timeout to prevent
	// hanging indefinitely if the Dolt server is unresponsive.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Always connect as a TCP client to the running server, even for local servers.
	// Without explicit --host/--port, dolt sql runs in embedded mode which loads all
	// databases into memory — causing OOM kills on large data dirs.
	// Note: --host, --port, --user, --no-tls are dolt GLOBAL args and must come
	// BEFORE the "sql" subcommand.
	fullArgs := []string{
		"--host", config.EffectiveHost(),
		"--port", strconv.Itoa(config.Port),
		"--user", config.User,
		"--no-tls",
		"sql",
		"-r", "csv",
		"-q", "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST",
	}
	cmd := exec.CommandContext(ctx, "dolt", fullArgs...)
	// GH#2537: Set cmd.Dir to the server's data directory to prevent dolt from
	// creating stray .doltcfg/privileges.db files in the caller's CWD. Even in
	// TCP client mode, dolt may auto-create .doltcfg/ in the working directory.
	cmd.Dir = config.DataDir
	// Always set DOLT_CLI_PASSWORD to prevent interactive password prompt.
	// When empty, dolt connects without a password (which is the default for local servers).
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD="+config.Password)
	setProcessGroup(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("querying connection count: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	// Parse CSV output: "cnt\n5\n"
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected output from connection count query: %s", string(output))
	}
	count, err := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		return 0, fmt.Errorf("parsing connection count %q: %w", lines[len(lines)-1], err)
	}

	return count, nil
}

// HasConnectionCapacity checks whether the Dolt server has capacity for new connections.
// Returns true if the active connection count is below the threshold (80% of max_connections).
// Returns false with error if the connection count cannot be determined — fail closed
// to prevent connection storms that cause read-only mode (gt-lfc0d).
func HasConnectionCapacity(townRoot string) (bool, int, error) {
	config := DefaultConfig(townRoot)
	maxConn := config.MaxConnections
	if maxConn <= 0 {
		maxConn = 1000 // Dolt default
	}

	active, err := GetActiveConnectionCount(townRoot)
	if err != nil {
		// Fail closed: if we can't check, the server may be overloaded
		return false, 0, err
	}

	// Use 80% threshold to leave headroom for existing operations
	threshold := (maxConn * 80) / 100
	if threshold < 1 {
		threshold = 1
	}

	return active < threshold, active, nil
}

// HealthMetrics holds resource monitoring data for the Dolt server.
type HealthMetrics struct {
	// Connections is the number of active connections (from information_schema.PROCESSLIST).
	Connections int `json:"connections"`

	// MaxConnections is the configured maximum connections.
	MaxConnections int `json:"max_connections"`

	// ConnectionPct is the percentage of max connections in use.
	ConnectionPct float64 `json:"connection_pct"`

	// DiskUsageBytes is the total size of the .dolt-data/ directory.
	DiskUsageBytes int64 `json:"disk_usage_bytes"`

	// DiskUsageHuman is a human-readable disk usage string.
	DiskUsageHuman string `json:"disk_usage_human"`

	// QueryLatency is the time taken for a SELECT active_branch() round-trip.
	// Note: json.Marshal emits nanoseconds for time.Duration. Consumers should use
	// ServerHealth.LatencyMs (int64 milliseconds) for JSON output instead.
	QueryLatency time.Duration `json:"query_latency_ns"`

	// ReadOnly indicates whether the server is in read-only mode.
	// When true, the server accepts reads but rejects all writes.
	ReadOnly bool `json:"read_only"`

	// LastCommitAge is the time since the most recent Dolt commit across all databases.
	// A large gap (>1 hour) may indicate the server was down or writes are failing.
	// Note: json.Marshal emits nanoseconds for time.Duration. Consumers should use
	// ServerHealth.LastCommitAgeSec (float64 seconds) for JSON output instead.
	LastCommitAge time.Duration `json:"last_commit_age_ns"`

	// LastCommitDB is the database that had the most recent commit.
	LastCommitDB string `json:"last_commit_db,omitempty"`

	// Healthy indicates whether the server is within acceptable resource limits.
	Healthy bool `json:"healthy"`

	// Warnings contains any degradation warnings (non-fatal).
	Warnings []string `json:"warnings,omitempty"`
}

// GetHealthMetrics collects resource monitoring metrics from the Dolt server.
// Returns partial metrics if some checks fail — always returns what it can.
func GetHealthMetrics(townRoot string) *HealthMetrics {
	config := DefaultConfig(townRoot)
	metrics := &HealthMetrics{
		Healthy:        true,
		MaxConnections: config.MaxConnections,
	}
	if metrics.MaxConnections <= 0 {
		metrics.MaxConnections = 1000 // Dolt default
	}

	// 1. Query latency: time a SELECT active_branch()
	latency, err := MeasureQueryLatency(townRoot)
	if err == nil {
		metrics.QueryLatency = latency
		if latency > 1*time.Second {
			metrics.Warnings = append(metrics.Warnings,
				fmt.Sprintf("query latency %v exceeds 1s threshold — server may be under stress", latency.Round(time.Millisecond)))
		}
	}

	// 2. Connection count
	connCount, err := GetActiveConnectionCount(townRoot)
	if err == nil {
		metrics.Connections = connCount
		metrics.ConnectionPct = float64(connCount) / float64(metrics.MaxConnections) * 100
		if metrics.ConnectionPct >= 80 {
			metrics.Healthy = false
			metrics.Warnings = append(metrics.Warnings,
				fmt.Sprintf("connection count %d is %.0f%% of max %d — approaching limit",
					connCount, metrics.ConnectionPct, metrics.MaxConnections))
		}
	}

	// 3. Disk usage
	diskBytes := dirSize(config.DataDir)
	metrics.DiskUsageBytes = diskBytes
	metrics.DiskUsageHuman = formatBytes(diskBytes)

	// 4. Read-only probe: attempt a test write
	readOnly, _ := CheckReadOnly(townRoot)
	metrics.ReadOnly = readOnly
	if readOnly {
		metrics.Healthy = false
		metrics.Warnings = append(metrics.Warnings,
			"server is in READ-ONLY mode — requires restart to recover")
	}

	// 5. Commit freshness: check the most recent commit across all databases.
	// A gap >1 hour suggests writes are failing or the server was recently down.
	if commitAge, commitDB, err := GetLastCommitAge(townRoot); err == nil {
		metrics.LastCommitAge = commitAge
		metrics.LastCommitDB = commitDB
		if commitAge > 1*time.Hour {
			metrics.Warnings = append(metrics.Warnings,
				fmt.Sprintf("last Dolt commit was %v ago (db: %s) — possible commit gap",
					commitAge.Round(time.Minute), commitDB))
		}
	}

	return metrics
}

// CheckReadOnly probes the Dolt server to detect read-only state by attempting
// a test write. The server can enter read-only mode under concurrent write load
// ("cannot update manifest: database is read only") and will NOT self-recover.
// Returns (true, nil) if read-only, (false, nil) if writable, (false, err) on probe failure.
func CheckReadOnly(townRoot string) (bool, error) {
	config := DefaultConfig(townRoot)
	running, _, err := IsRunning(townRoot)
	if err != nil {
		return false, fmt.Errorf("checking Dolt server before write probe: %w", err)
	}
	if !running {
		if _, err := ListDatabases(townRoot); err != nil {
			return false, fmt.Errorf("listing databases for write probe: %w", err)
		}
		return false, nil
	}

	// The probe runs against the server, so select its target from the server's
	// live catalog. A valid directory can exist on disk without being loaded.
	databases, err := listDatabasesRemote(config)
	if err != nil {
		return false, fmt.Errorf("listing databases for write probe: %w", err)
	}
	if len(databases) == 0 {
		return false, nil // Can't probe without a database
	}

	db := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Attempt a write operation: create a temp table, write a row, drop it.
	// If the server is in read-only mode, this will fail with a characteristic error.
	query := fmt.Sprintf(
		"USE `%s`; CREATE TABLE IF NOT EXISTS `__gt_health_probe` (v INT PRIMARY KEY); REPLACE INTO `__gt_health_probe` VALUES (1); DROP TABLE IF EXISTS `__gt_health_probe`",
		db,
	)
	// DDL probe (CREATE/REPLACE/DROP) must land on the running server; embedded
	// mode would mutate disk without notifying the live catalog (#3518/#3641).
	cmd := buildServerSQLCmd(ctx, config, "-q", query)

	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if IsReadOnlyError(msg) {
			return true, nil
		}
		return false, fmt.Errorf("write probe failed: %w (%s)", err, msg)
	}

	return false, nil
}

// IsReadOnlyError checks if an error message indicates a Dolt read-only state.
// The characteristic error is "cannot update manifest: database is read only".
func IsReadOnlyError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "read only") ||
		strings.Contains(lower, "read-only") ||
		strings.Contains(lower, "readonly")
}

// RecoverReadOnly detects a read-only Dolt server, restarts it, and verifies
// recovery. This is the gt-level counterpart to the daemon's auto-recovery:
// when a gt command (spawn, done, etc.) encounters persistent read-only errors,
// it can call this to attempt recovery without waiting for the daemon's 30s loop.
// Returns nil if recovery succeeded, an error if recovery failed or wasn't needed.
func RecoverReadOnly(townRoot string) error {
	readOnly, err := CheckReadOnly(townRoot)
	if err != nil {
		return fmt.Errorf("read-only probe failed: %w", err)
	}
	if !readOnly {
		return nil // Server is writable, no recovery needed
	}

	fmt.Printf("Dolt server is in read-only mode, attempting recovery...\n")

	// Stop the server
	if err := Stop(townRoot); err != nil {
		// Server might already be stopped or unreachable
		style.PrintWarning("stop returned error (proceeding with restart): %v", err)
	}

	// Brief pause for cleanup
	time.Sleep(1 * time.Second)

	// Restart the server
	if err := Start(townRoot); err != nil {
		return fmt.Errorf("failed to restart Dolt server: %w", err)
	}

	// Verify recovery with exponential backoff (server may need time to become writable)
	const maxAttempts = 5
	const baseBackoff = 500 * time.Millisecond
	const maxBackoff = 8 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		backoff := baseBackoff
		for i := 1; i < attempt; i++ {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
				break
			}
		}
		time.Sleep(backoff)

		readOnly, err = CheckReadOnly(townRoot)
		if err != nil {
			if attempt == maxAttempts {
				return fmt.Errorf("post-restart probe failed after %d attempts: %w", maxAttempts, err)
			}
			continue
		}
		if !readOnly {
			fmt.Printf("Dolt server recovered from read-only state\n")
			return nil
		}
	}

	return fmt.Errorf("Dolt server still read-only after restart (%d verification attempts)", maxAttempts)
}

// doltSQLWithRecovery executes a SQL statement with retry logic and, if retries
// are exhausted due to read-only errors, attempts server restart before a final retry.
// This is the gt-level recovery path for polecat management operations (spawn, done).
func doltSQLWithRecovery(townRoot, rigDB, query string) error {
	err := doltSQLWithRetry(townRoot, rigDB, query)
	if err == nil {
		return nil
	}

	// If the final error is a read-only error, attempt recovery
	if !IsReadOnlyError(err.Error()) {
		return err
	}

	// Attempt server recovery
	if recoverErr := RecoverReadOnly(townRoot); recoverErr != nil {
		return fmt.Errorf("read-only recovery failed: %w (original: %v)", recoverErr, err)
	}

	// Retry the operation after recovery
	if retryErr := doltSQL(townRoot, rigDB, query); retryErr != nil {
		return fmt.Errorf("operation failed after read-only recovery: %w", retryErr)
	}

	return nil
}

// MeasureQueryLatency times a SELECT active_branch() query against the Dolt server.
// Per Tim Sehn (Dolt CEO): active_branch() is a lightweight probe that won't block
// behind queued queries, unlike SELECT 1 which goes through the full query executor.
// Uses a direct TCP connection via the Go MySQL driver to measure actual query
// latency, not subprocess startup time.
func MeasureQueryLatency(townRoot string) (time.Duration, error) {
	config := DefaultConfig(townRoot)

	dsn := fmt.Sprintf("%s@tcp(%s:%d)/", config.User, config.EffectiveHost(), config.Port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return 0, fmt.Errorf("opening mysql connection: %w", err)
	}
	defer db.Close()

	db.SetConnMaxLifetime(5 * time.Second)
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	var branch string
	err = db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch)
	elapsed := time.Since(start)

	if err != nil {
		return 0, fmt.Errorf("SELECT active_branch() failed: %w", err)
	}

	return elapsed, nil
}

// GetLastCommitAge returns the age and database name of the most recent Dolt commit
// across all databases. This detects commit gaps — periods where no writes persisted.
//
// Uses database/sql (like MeasureQueryLatency) rather than dolt subprocess to avoid
// subprocess startup overhead dominating the measurement.
func GetLastCommitAge(townRoot string) (time.Duration, string, error) {
	config := DefaultConfig(townRoot)

	dsn := fmt.Sprintf("%s@tcp(%s:%d)/", config.User, config.EffectiveHost(), config.Port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return 0, "", fmt.Errorf("opening mysql connection: %w", err)
	}
	defer db.Close()

	db.SetConnMaxLifetime(5 * time.Second)
	db.SetMaxOpenConns(1)

	databases, err := ListDatabases(townRoot)
	if err != nil || len(databases) == 0 {
		return 0, "", fmt.Errorf("listing databases: %w", err)
	}

	var mostRecent time.Time
	var mostRecentDB string

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for _, dbName := range databases {
		var dateStr string
		query := fmt.Sprintf("SELECT MAX(date) FROM `%s`.dolt_log LIMIT 1", dbName)
		if err := db.QueryRowContext(ctx, query).Scan(&dateStr); err != nil {
			continue // Skip databases that fail (e.g., no dolt_log)
		}
		// Dolt's dolt_log.date is DATETIME(6) (microsecond precision). Without
		// parseTime=true in the DSN, the Go MySQL driver returns this as a string
		// like "2025-03-28 12:34:56.123456". Go's ".999" fractional format accepts
		// any number of trailing digits (1-9), correctly parsing both millisecond
		// and microsecond timestamps. RFC3339 fallback handles version differences.
		t, err := time.Parse("2006-01-02 15:04:05.999", dateStr)
		if err != nil {
			t, err = time.Parse(time.RFC3339, dateStr)
			if err != nil {
				continue
			}
		}
		if t.After(mostRecent) {
			mostRecent = t
			mostRecentDB = dbName
		}
	}

	if mostRecent.IsZero() {
		return 0, "", fmt.Errorf("no commits found in any database")
	}

	return time.Since(mostRecent), mostRecentDB, nil
}

// dirSize returns the total size of a directory tree in bytes.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// formatBytes returns a human-readable size string.
func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// moveDir moves a directory from src to dest. It first tries os.Rename for
// efficiency, but falls back to copy+delete if src and dest are on different
// filesystems (which causes EXDEV error on rename).
func moveDir(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	// Cross-filesystem: copy then delete source
	if runtime.GOOS == "windows" {
		cmd := exec.Command("robocopy", src, dest, "/E", "/MOVE", "/R:1", "/W:1")
		setProcessGroup(cmd)
		if err := cmd.Run(); err != nil {
			// robocopy returns 1 for success with copies
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() <= 7 {
				return nil
			}
			return fmt.Errorf("robocopy: %w", err)
		}
		return nil
	}
	cmd := exec.Command("cp", "-a", src, dest)
	setProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copying directory: %w", err)
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("removing source after copy: %w", err)
	}
	return nil
}

// serverExecSQL executes a SQL statement against the Dolt server without targeting
// a specific database. Used for server-level commands like CREATE DATABASE.
//
// Always connects via explicit --host/--port flags to ensure the command goes
// through the running sql-server process. Without these flags, `dolt sql` runs
// in embedded mode (even from the data directory), which creates databases on
// applyWaitTimeoutFn is the SQL-exec seam used by applyWaitTimeout. Tests
// override it to verify the policy decision (skip vs. send) and the
// query-formatting without spawning a real dolt subprocess.
var applyWaitTimeoutFn = serverExecSQL

// applyWaitTimeout sets MySQL's `wait_timeout` server variable on the running
// Dolt server so idle connections close in seconds rather than Dolt's 8-hour
// default. Under sustained load this is the difference between healthy
// connection turnover and a death spiral that exhausts max_connections.
// See gh-3623.
//
// Best-effort: a failure here is logged but does not fail the start, since
// the server is already up and serving traffic. Callers can still operate;
// they just inherit the long Dolt default.
func applyWaitTimeout(townRoot string, config *Config) {
	if config.WaitTimeoutSec <= 0 {
		return
	}
	query := buildWaitTimeoutQuery(config.WaitTimeoutSec)
	if err := applyWaitTimeoutFn(townRoot, query); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not set Dolt wait_timeout=%ds: %v\n", config.WaitTimeoutSec, err)
	}
}

// buildWaitTimeoutQuery returns the SET GLOBAL statement applyWaitTimeout
// dispatches. Extracted for direct testability.
func buildWaitTimeoutQuery(seconds int) string {
	return fmt.Sprintf("SET GLOBAL wait_timeout = %d", seconds)
}

// applyTimeZoneFn is the SQL-exec seam used by applyTimeZone. Tests override
// it to verify policy + query-formatting without spawning a real dolt subprocess.
var applyTimeZoneFn = serverExecSQL

// applyTimeZone sets MySQL's `time_zone` server variable on the running Dolt
// server so server-side `NOW()` and `CURRENT_TIMESTAMP` agree with Go-side
// `time.Now().UTC()` writes. Without this override Dolt inherits the host TZ,
// which makes ad-hoc `dolt sql` queries against `created_at`/`closed_at`
// produce nonsense diffs (e.g. wisps appear hours in the future). The reaper
// itself binds Go-side UTC values and is unaffected, but humans triaging
// wisp lifecycle issues are routinely misled — see hq-57jr8.
//
// Best-effort: a failure here is logged but does not fail the start.
func applyTimeZone(townRoot string, config *Config) {
	if config.TimeZone == "" {
		return
	}
	query := buildTimeZoneQuery(config.TimeZone)
	if err := applyTimeZoneFn(townRoot, query); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not set Dolt time_zone=%q: %v\n", config.TimeZone, err)
	}
}

// buildTimeZoneQuery returns the SET GLOBAL statement applyTimeZone dispatches.
// Extracted for direct testability.
func buildTimeZoneQuery(tz string) string {
	return fmt.Sprintf("SET GLOBAL time_zone = '%s'", tz)
}

// disk but does NOT register them with the live server catalog. This caused
// "database not found" errors during gt rig add.
func serverExecSQL(townRoot, query string) error {
	config := DefaultConfig(townRoot)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := buildServerSQLCmd(ctx, config, "-q", query)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// buildServerSQLCmd constructs a dolt sql command that always connects to the
// running sql-server via explicit --host/--port flags. Unlike buildDoltSQLCmd,
// which omits connection flags for local servers (relying on dolt auto-detection),
// this function ensures the command goes through the live server process.
// This is critical for DDL operations (CREATE/DROP DATABASE) that must modify
// the server's in-memory catalog, not just the filesystem.
//
// Dolt requires --host, --port, --user, --no-tls as global flags (before the
// subcommand), not as subcommand flags. The order is:
//
//	dolt --host=H --port=P --user=U --no-tls sql -q "..."
func buildServerSQLCmd(ctx context.Context, config *Config, args ...string) *exec.Cmd {
	// Global connection flags must come before the "sql" subcommand.
	// Always pass --password to prevent dolt from prompting on stdin
	// (which fails with "inappropriate ioctl" in non-TTY environments).
	password := config.Password
	fullArgs := []string{
		"--host", config.EffectiveHost(),
		"--port", strconv.Itoa(config.Port),
		"--user", config.User,
		"--password", password,
		"--no-tls",
		"sql",
	}
	fullArgs = append(fullArgs, args...)

	cmd := exec.CommandContext(ctx, "dolt", fullArgs...)
	cmd.Dir = config.DataDir
	setProcessGroup(cmd)

	return cmd
}

// waitForCatalog polls the Dolt server until the named database is visible in the
// in-memory catalog. This bridges the race between CREATE DATABASE returning and the
// catalog being updated — without this, immediate USE/query operations can fail with
// "Unknown database". Uses exponential backoff: 100ms, 200ms, 400ms, 800ms, 1.6s.
// Only retries on catalog-race errors ("Unknown database"); returns immediately for
// other failures (e.g., server crash, binary missing).
func waitForCatalog(townRoot, dbName string) error {
	const maxAttempts = 5
	const baseBackoff = 100 * time.Millisecond
	const maxBackoff = 2 * time.Second

	query := fmt.Sprintf("USE %s", dbName)
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := serverExecSQL(townRoot, query); err != nil {
			lastErr = err
			// Only retry catalog-race errors; fail fast on other errors
			// (connection refused, binary missing, etc.)
			errStr := err.Error()
			if !strings.Contains(errStr, "Unknown database") && !strings.Contains(errStr, "database not found") {
				return fmt.Errorf("database %q probe failed (non-retryable): %w", dbName, err)
			}
			if attempt < maxAttempts {
				backoff := baseBackoff
				for i := 1; i < attempt; i++ {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
						break
					}
				}
				time.Sleep(backoff)
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("database %q not visible after %d attempts: %w", dbName, maxAttempts, lastErr)
}

// doltSQL executes a SQL statement against a specific rig database on the Dolt server.
// Uses explicit --host/--port flags to connect to the running server (same rationale
// as serverExecSQL — embedded mode doesn't share the server's catalog).
// The USE prefix selects the database since --use-db is not available on all dolt versions.
func doltSQL(townRoot, rigDB, query string) error {
	config := DefaultConfig(townRoot)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Prepend USE <db> to select the target database.
	fullQuery := fmt.Sprintf("USE %s; %s", rigDB, query)
	cmd := buildServerSQLCmd(ctx, config, "-q", fullQuery)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// doltSQLWithRetry executes a SQL statement with exponential backoff on transient errors.
func doltSQLWithRetry(townRoot, rigDB, query string) error {
	const maxRetries = 5
	const baseBackoff = 500 * time.Millisecond
	const maxBackoff = 15 * time.Second

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := doltSQL(townRoot, rigDB, query); err != nil {
			lastErr = err
			if !isDoltRetryableError(err) {
				return err
			}
			if attempt < maxRetries {
				backoff := baseBackoff
				for i := 1; i < attempt; i++ {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
						break
					}
				}
				time.Sleep(backoff)
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}

// isDoltRetryableError returns true if the error is a transient Dolt failure worth retrying.
// Covers manifest lock contention, read-only mode, optimistic lock failures, timeouts,
// and catalog propagation delays after CREATE DATABASE.
func isDoltRetryableError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is read only") ||
		strings.Contains(msg, "cannot update manifest") ||
		strings.Contains(msg, "optimistic lock") ||
		strings.Contains(msg, "serialization failure") ||
		strings.Contains(msg, "lock wait timeout") ||
		strings.Contains(msg, "try restarting transaction") ||
		strings.Contains(msg, "Unknown database")
}

// CommitServerWorkingSet stages all pending changes and commits them on the current branch via SQL.
// This flushes the Dolt working set to HEAD so that DOLT_BRANCH (which forks from
// HEAD, not the working set) will include all recent writes. Critical for the sling
// flow where BD_DOLT_AUTO_COMMIT=off leaves writes in working set only.
//
// NOTE: This flushes ALL pending working set changes on the target branch, not just
// those from a specific polecat. In batch sling, polecat B's flush may capture
// polecat A's writes. This is benign because beads are keyed by unique ID, so
// duplicate data across branches merges cleanly.
func CommitServerWorkingSet(townRoot, rigDB, message string) error {
	if err := doltSQLWithRecovery(townRoot, rigDB, "CALL DOLT_ADD('-A')"); err != nil {
		return fmt.Errorf("staging working set in %s: %w", rigDB, err)
	}
	escaped := strings.ReplaceAll(message, "'", "''")
	query := fmt.Sprintf("CALL DOLT_COMMIT('--allow-empty', '-m', '%s')", escaped)
	if err := doltSQLWithRecovery(townRoot, rigDB, query); err != nil {
		return fmt.Errorf("committing working set in %s: %w", rigDB, err)
	}
	return nil
}

// doltSQLScript executes a multi-statement SQL script via a temp file.
// Uses `dolt sql --file` for reliable multi-statement execution within a
// single connection, preserving DOLT_CHECKOUT state across statements.
func doltSQLScript(townRoot, script string) error {
	config := DefaultConfig(townRoot)

	tmpFile, err := os.CreateTemp("", "dolt-script-*.sql")
	if err != nil {
		return fmt.Errorf("creating temp SQL file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(script); err != nil {
		tmpFile.Close()
		return fmt.Errorf("writing SQL script: %w", err)
	}
	tmpFile.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Scripts typically contain DDL (CREATE TABLE, etc.) for rig workload schemas;
	// they must execute against the running server's catalog, not embedded-mode
	// disk-only state. This is the root cause of #3641 — MR-bead script ran
	// embedded, so later INSERTs over port 3307 hit "no database selected".
	cmd := buildServerSQLCmd(ctx, config, "--file", tmpFile.Name())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// doltSQLScriptWithRetry executes a SQL script with exponential backoff on transient errors.
// Callers must ensure scripts are idempotent, as partial execution may have occurred
// before the retry. Uses the same retry classification as doltSQLWithRetry but with
// fewer retries and shorter backoff since multi-statement scripts are more expensive.
func doltSQLScriptWithRetry(townRoot, script string) error {
	const maxRetries = 3
	const baseBackoff = 500 * time.Millisecond
	const maxBackoff = 8 * time.Second

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := doltSQLScript(townRoot, script); err != nil {
			lastErr = err
			if !isDoltRetryableError(err) {
				return err
			}
			if attempt < maxRetries {
				backoff := baseBackoff
				for i := 1; i < attempt; i++ {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
						break
					}
				}
				time.Sleep(backoff)
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}
