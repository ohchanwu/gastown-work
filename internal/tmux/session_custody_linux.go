//go:build linux

package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	linuxProcRoot                               = "/proc"
	sessionCustodyReadyProbe                    = 300 * time.Millisecond
	linuxCustodyHandshakeTimeout                = 3 * time.Second
	linuxCustodyReapTimeout                     = 3 * time.Second
	linuxCustodyInitExitTimeout                 = 3 * time.Second
	linuxCustodyBrokerShutdownTimeout           = 2 * time.Second
	linuxCustodyCooperativeShutdownTimeout      = linuxCustodyReapTimeout + linuxCustodyBrokerShutdownTimeout + time.Second
	linuxCustodySupervisorExitTimeout           = 10 * time.Second
	linuxCustodyReadyFD                         = 3
	linuxCustodyPermitFD                        = 4
	linuxCustodySupervisorLifeFD                = 5
	envLinuxSessionCustodyInit                  = "GT_INTERNAL_SESSION_CUSTODY_INIT"
	envLinuxSessionCustodyCommand               = "GT_INTERNAL_SESSION_CUSTODY_COMMAND"
	envLinuxSessionCustodyNamespaced            = "GT_INTERNAL_SESSION_CUSTODY_NAMESPACED"
	envLinuxSessionScratch                      = "GT_INTERNAL_SESSION_SCRATCH"
	linuxSessionScratchPrefix                   = "gastown-session-scratch-"
	linuxSessionScratchBytes                    = 256 * 1024 * 1024
	linuxSessionScratchInodes                   = 16 * 1024
	linuxSessionSharedMemoryBytes               = 16 * 1024 * 1024
	linuxSessionSharedMemoryInodes              = 1024
	linuxSessionDeviceBytes                     = 1024 * 1024
	linuxSessionDeviceInodes                    = 128
	linuxSessionCustodyReadyByte           byte = 'R'
	linuxSessionCustodyHardenedByte        byte = 'H'
)

type linuxSessionCustody struct {
	supervisorPID      int
	supervisorIdentity string
	supervisorFD       int
	initPID            int
	initIdentity       string
	initNamespace      uint64
	initFD             int
	cgroup             string
	prepared           bool
	committed          bool
	finalized          bool
}

type linuxCustodyKillBudgets struct {
	Init       time.Duration
	Broker     time.Duration
	Supervisor time.Duration
}

type linuxCustodyKillOps struct {
	revalidate   func() (linuxProcessStat, error)
	signal       func(int, unix.Signal) error
	waitTerminal func(context.Context, int, string) error
	waitGone     func(context.Context, int, string) error
}

type linuxCustodyServiceResult struct {
	name string
	err  error
}

type linuxCustodyMountSource struct {
	path      string
	fd        int
	directory bool
}

func sessionCustodyLaunchSupported() bool {
	return runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"
}

func withoutEnvironmentKeys(env []string, keys ...string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		keep := true
		for _, key := range keys {
			if strings.HasPrefix(entry, key+"=") {
				keep = false
				break
			}
		}
		if keep {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

type linuxCustodyLaunch struct {
	child          *exec.Cmd
	wait           <-chan error
	pidfd          int
	ready          *os.File
	permit         *os.File
	life           *os.File
	broker         *os.File
	tmux           *os.File
	control        *os.File
	proxyExpected  bool
	proxies        linuxCustodyProxySet
	cgroup         string
	previousCgroup string
	scratch        string
}

func startLinuxCustodyWait(child *exec.Cmd) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- child.Wait()
		close(result)
	}()
	return result
}

func waitLinuxCustodyProcess(launch *linuxCustodyLaunch, timeout time.Duration) error {
	if launch == nil || launch.wait == nil {
		return errors.New("session custody process has no wait handle")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-launch.wait:
		launch.wait = nil
		return err
	case <-timer.C:
		return fmt.Errorf("session custody process was not reaped: %w", context.DeadlineExceeded)
	}
}

func newLinuxUncontainedCustodyCommand(command string) (*exec.Cmd, *int) {
	child := exec.Command("/bin/sh", "-lc", command)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = withoutEnvironmentKeys(os.Environ(), EnvSessionCustody)
	pidfd := -1
	child.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: unix.SIGKILL,
		PidFD:     &pidfd,
	}
	return child, &pidfd
}

func pinLinuxSessionTmuxExecutable() (*os.File, error) {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return nil, fmt.Errorf("resolving trusted tmux executable: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing trusted tmux executable: %w", err)
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("pinning trusted tmux executable: %w", err)
	}
	file := os.NewFile(uintptr(fd), "session-broker-tmux-executable")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("pinning trusted tmux executable returned no file")
	}
	return file, nil
}

func startLinuxCustodyInitCommand(command string, namespaced bool) (_ *linuxCustodyLaunch, retErr error) {
	previousCgroup, err := linuxCgroupDirectoryForPID(os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("capturing supervisor cgroup receipt: %w", err)
	}
	controlFD, err := unix.Open(previousCgroup, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("pinning supervisor control cgroup: %w", err)
	}
	controlCgroup := os.NewFile(uintptr(controlFD), "session-broker-control-cgroup")
	if controlCgroup == nil {
		_ = unix.Close(controlFD)
		return nil, errors.New("pinning supervisor control cgroup returned no file")
	}
	scratch, err := os.MkdirTemp("", linuxSessionScratchPrefix)
	if err != nil {
		_ = controlCgroup.Close()
		return nil, fmt.Errorf("creating bounded session scratch mountpoint: %w", err)
	}
	if err := os.Chmod(scratch, 0o700); err != nil {
		_ = os.Remove(scratch)
		_ = controlCgroup.Close()
		return nil, fmt.Errorf("securing bounded session scratch mountpoint: %w", err)
	}
	defer func() {
		if retErr != nil {
			if scratch != "" {
				_ = os.Remove(scratch)
			}
			if controlCgroup != nil {
				_ = controlCgroup.Close()
			}
		}
	}()
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = readyReader.Close()
			_ = readyWriter.Close()
		}
	}()
	permitReader, permitWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = permitReader.Close()
			_ = permitWriter.Close()
		}
	}()
	lifeReader, lifeWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = lifeReader.Close()
			_ = lifeWriter.Close()
		}
	}()
	brokerPair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("creating session custody broker socket: %w", err)
	}
	brokerServer := os.NewFile(uintptr(brokerPair[0]), "session-custody-broker-server")
	brokerClient := os.NewFile(uintptr(brokerPair[1]), "session-custody-broker-client")
	defer func() {
		if retErr != nil {
			if brokerServer != nil {
				_ = brokerServer.Close()
			}
			if brokerClient != nil {
				_ = brokerClient.Close()
			}
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	tmuxExecutable, err := pinLinuxSessionTmuxExecutable()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil && tmuxExecutable != nil {
			_ = tmuxExecutable.Close()
		}
	}()
	child := exec.Command(executable, "session-custody-init")
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.ExtraFiles = []*os.File{readyWriter, permitReader, lifeReader, brokerClient}
	child.Env = append(
		withoutEnvironmentKeys(os.Environ(), envLinuxSessionCustodyInit, envLinuxSessionCustodyCommand, envLinuxSessionCustodyNamespaced, envLinuxSessionScratch),
		envLinuxSessionCustodyInit+"=1",
		envLinuxSessionCustodyCommand+"="+command,
		envLinuxSessionScratch+"="+scratch,
	)
	if namespaced {
		child.Env = append(child.Env, envLinuxSessionCustodyNamespaced+"=1")
	}
	pidfd := -1
	child.SysProcAttr = &syscall.SysProcAttr{
		PidFD: &pidfd,
	}
	if namespaced {
		child.SysProcAttr.Cloneflags = unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWIPC
		child.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
		child.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
		child.SysProcAttr.GidMappingsEnableSetgroups = false
	}
	if err := child.Start(); err != nil {
		return nil, err
	}
	launch := &linuxCustodyLaunch{
		child: child, wait: startLinuxCustodyWait(child), pidfd: pidfd,
		ready: readyReader, permit: permitWriter, life: lifeWriter, broker: brokerServer,
		tmux:           tmuxExecutable,
		control:        controlCgroup,
		proxyExpected:  namespaced,
		previousCgroup: previousCgroup,
		scratch:        scratch,
	}
	brokerServer = nil
	tmuxExecutable = nil
	controlCgroup = nil
	scratch = ""
	_ = readyWriter.Close()
	_ = permitReader.Close()
	_ = lifeReader.Close()
	_ = brokerClient.Close()
	brokerClient = nil
	if pidfd < 0 {
		return nil, errors.Join(errors.New("kernel did not return a pidfd for session custody"), closeLinuxCustodyLaunch(launch, true))
	}
	cgroup, err := prepareLinuxSessionCgroup(child.Process.Pid, os.Getpid())
	if err != nil {
		return nil, errors.Join(fmt.Errorf("preparing bounded session resources: %w", err), closeLinuxCustodyLaunch(launch, true))
	}
	launch.cgroup = cgroup
	return launch, nil
}

func startLinuxCustodyCommand(command string, namespaced bool) (*linuxCustodyLaunch, error) {
	if namespaced {
		return startLinuxCustodyInitCommand(command, true)
	}
	child, pidfd := newLinuxUncontainedCustodyCommand(command)
	if err := child.Start(); err != nil {
		return nil, err
	}
	launch := &linuxCustodyLaunch{child: child, wait: startLinuxCustodyWait(child), pidfd: *pidfd}
	if *pidfd < 0 {
		return nil, errors.Join(errors.New("kernel did not return a pidfd for session custody"), closeLinuxCustodyLaunch(launch, true))
	}
	return launch, nil
}

type linuxCustodyStarter func(string, bool) (*linuxCustodyLaunch, error)
type linuxNamespaceValidator func(string, int, int) (string, uint64, error)

func closeLinuxCustodyLaunch(launch *linuxCustodyLaunch, terminate bool) error {
	return closeLinuxCustodyLaunchWithTimeout(launch, terminate, linuxCustodyReapTimeout)
}

func closeLinuxCustodyLaunchWithTimeout(launch *linuxCustodyLaunch, terminate bool, reapTimeout time.Duration) error {
	if launch == nil {
		return nil
	}
	var errs []error
	if terminate && launch.child != nil {
		if launch.pidfd >= 0 {
			errs = append(errs, unix.PidfdSendSignal(launch.pidfd, unix.SIGKILL, nil, 0))
		} else {
			errs = append(errs, launch.child.Process.Kill())
		}
		waitErr := waitLinuxCustodyProcess(launch, reapTimeout)
		errs = append(errs, waitErr)
	}
	if launch.ready != nil {
		errs = append(errs, launch.ready.Close())
		launch.ready = nil
	}
	if launch.permit != nil {
		errs = append(errs, launch.permit.Close())
		launch.permit = nil
	}
	if terminate && launch.life != nil {
		errs = append(errs, launch.life.Close())
		launch.life = nil
	}
	if terminate && launch.broker != nil {
		errs = append(errs, launch.broker.Close())
		launch.broker = nil
	}
	if terminate && launch.tmux != nil {
		errs = append(errs, launch.tmux.Close())
		launch.tmux = nil
	}
	if terminate && launch.control != nil {
		errs = append(errs, launch.control.Close())
		launch.control = nil
	}
	if terminate {
		errs = append(errs, launch.proxies.Close())
	}
	if terminate && launch.pidfd >= 0 {
		errs = append(errs, unix.Close(launch.pidfd))
		launch.pidfd = -1
	}
	if terminate && launch.cgroup != "" {
		if current, err := linuxCgroupDirectoryForPID(os.Getpid()); err == nil && current == launch.cgroup {
			errs = append(errs, restoreLinuxProcessCgroup(launch.previousCgroup, os.Getpid()))
		}
		errs = append(errs, clearLinuxSessionCgroupReceipt(&launch.cgroup, removeLinuxSessionCgroup))
	}
	if terminate && launch.scratch != "" {
		err := os.Remove(launch.scratch)
		if err == nil || os.IsNotExist(err) {
			launch.scratch = ""
		} else {
			errs = append(errs, fmt.Errorf("removing bounded session scratch receipt: %w", err))
		}
	}
	return errors.Join(errs...)
}

func waitLinuxCustodyReady(launch *linuxCustodyLaunch) error {
	if launch == nil || launch.ready == nil {
		return errors.New("session custody init has no readiness pipe")
	}
	return waitLinuxCustodyProtocolByte(launch.ready, linuxSessionCustodyReadyByte, "readiness")
}

func waitLinuxCustodyHardened(launch *linuxCustodyLaunch) error {
	if launch == nil || launch.ready == nil {
		return errors.New("session custody init has no hardening pipe")
	}
	return waitLinuxCustodyProtocolByte(launch.ready, linuxSessionCustodyHardenedByte, "hardening")
}

func waitLinuxCustodyProtocolByte(file *os.File, expected byte, phase string) error {
	type readResult struct {
		value byte
		err   error
	}
	result := make(chan readResult, 1)
	go func() {
		var value [1]byte
		_, err := io.ReadFull(file, value[:])
		result <- readResult{value: value[0], err: err}
	}()
	timer := time.NewTimer(linuxCustodyHandshakeTimeout)
	defer timer.Stop()
	select {
	case received := <-result:
		if received.err != nil {
			return fmt.Errorf("session custody init %s: %w", phase, received.err)
		}
		if received.value != expected {
			return fmt.Errorf("session custody init sent invalid %s byte %d", phase, received.value)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out waiting for session custody init %s", phase)
	}
}

func writeLinuxCustodyProtocolByte(file *os.File, value byte) error {
	if file == nil {
		return errors.New("session custody protocol descriptor is unavailable")
	}
	written, err := file.Write([]byte{value})
	if err != nil {
		return err
	}
	if written != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func launchLinuxCustodyCommand(command string, start linuxCustodyStarter) (*linuxCustodyLaunch, bool, error) {
	return launchLinuxCustodyCommandValidated(command, start, validateLinuxNamespaceInit)
}

func launchLinuxCustodyCommandValidated(command string, start linuxCustodyStarter, validate linuxNamespaceValidator) (*linuxCustodyLaunch, bool, error) {
	launch, err := start(command, true)
	if err != nil {
		return nil, false, fmt.Errorf("starting contained session custody: %w", err)
	}
	if err := waitLinuxCustodyReady(launch); err != nil {
		return nil, false, errors.Join(ErrSessionCustodyUnsupported, err, closeLinuxCustodyLaunch(launch, true))
	}
	if _, _, err := validate(linuxProcRoot, os.Getpid(), launch.child.Process.Pid); err != nil {
		return nil, false, errors.Join(err, closeLinuxCustodyLaunch(launch, true))
	}
	if launch.scratch != "" {
		if err := os.Remove(launch.scratch); err != nil && !os.IsNotExist(err) {
			return nil, false, errors.Join(fmt.Errorf("unlinking hardened session scratch mountpoint: %w", err), closeLinuxCustodyLaunch(launch, true))
		}
		launch.scratch = ""
	}
	if err := writeLinuxCustodyProtocolByte(launch.permit, 1); err != nil {
		return nil, false, errors.Join(err, closeLinuxCustodyLaunch(launch, true))
	}
	if launch.proxyExpected {
		if launch.broker == nil {
			return nil, false, errors.Join(errors.New("session custody proxy handoff has no broker endpoint"), closeLinuxCustodyLaunch(launch, true))
		}
		if err := waitLinuxCustodyProxySet(launch, linuxCustodyHandshakeTimeout); err != nil {
			return nil, false, errors.Join(ErrSessionCustodyUnsupported, err, closeLinuxCustodyLaunch(launch, true))
		}
	}
	if err := waitLinuxCustodyHardened(launch); err != nil {
		return nil, false, errors.Join(ErrSessionCustodyUnsupported, err, closeLinuxCustodyLaunch(launch, true))
	}
	if err := writeLinuxCustodyProtocolByte(launch.life, 1); err != nil {
		return nil, false, errors.Join(errors.New("session custody supervisor liveness handshake failed"), err, closeLinuxCustodyLaunch(launch, true))
	}
	if err := closeLinuxCustodyLaunch(launch, false); err != nil {
		return nil, false, errors.Join(err, closeLinuxCustodyLaunch(launch, true))
	}
	return launch, true, nil
}

func runSessionCustodyInit() (bool, error) {
	if os.Getenv(envLinuxSessionCustodyInit) != "1" {
		return false, nil
	}
	command := os.Getenv(envLinuxSessionCustodyCommand)
	if strings.TrimSpace(command) == "" {
		return true, errors.New("session custody init command is empty")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := validateLinuxSessionBrokerEndpoint(linuxSessionBrokerFD); err != nil {
		return true, err
	}
	ready := os.NewFile(linuxCustodyReadyFD, "session-custody-ready")
	permit := os.NewFile(linuxCustodyPermitFD, "session-custody-permit")
	life := os.NewFile(linuxCustodySupervisorLifeFD, "session-custody-supervisor-life")
	if ready == nil || permit == nil || life == nil {
		return true, errors.New("session custody init protocol descriptors are unavailable")
	}
	unix.CloseOnExec(int(life.Fd()))
	var proxyPorts *linuxCustodyProxyPorts
	if os.Getenv(envLinuxSessionCustodyNamespaced) == "1" {
		if err := prepareLinuxCustodyNamespace(os.Getenv(envLinuxSessionScratch)); err != nil {
			return true, err
		}
	}
	if err := writeLinuxCustodyProtocolByte(ready, linuxSessionCustodyReadyByte); err != nil {
		return true, err
	}
	var permission [1]byte
	if _, err := io.ReadFull(permit, permission[:]); err != nil {
		return true, fmt.Errorf("waiting for session custody launch permit: %w", err)
	}
	if err := permit.Close(); err != nil {
		return true, err
	}
	if os.Getenv(envLinuxSessionCustodyNamespaced) == "1" {
		ports, err := createAndSendLinuxCustodyProxyListeners(linuxSessionBrokerFD)
		if err != nil {
			return true, err
		}
		proxyPorts = &ports
		if err := dropLinuxCustodyCapabilities(); err != nil {
			return true, err
		}
	}
	if err := installLinuxCustodySeccomp(); err != nil {
		return true, err
	}
	go func() {
		var value [1]byte
		for {
			if _, err := life.Read(value[:]); err != nil {
				// A PID namespace protects its PID 1 from signals sent inside
				// that namespace, including a self-directed SIGKILL. Exiting the
				// trusted init itself makes the kernel tear down every remaining
				// process in the namespace when the supervisor disappears.
				os.Exit(125)
			}
		}
	}()
	if err := setLinuxCustodyNonDumpable(); err != nil {
		return true, err
	}
	if err := applyLinuxSessionResourceLimits(); err != nil {
		return true, err
	}
	if err := writeLinuxCustodyProtocolByte(ready, linuxSessionCustodyHardenedByte); err != nil {
		return true, err
	}
	if err := ready.Close(); err != nil {
		return true, err
	}
	return true, runLinuxCustodyWorkload(command, proxyPorts)
}

func decodeLinuxCustodyAllowedPaths(raw, scratch string) (_ []linuxCustodyMountSource, retErr error) {
	if len(raw) == 0 || len(raw) > maxSessionCustodyPathsBytes {
		return nil, errors.New("session custody filesystem allowlist is missing or oversized")
	}
	var paths []string
	if err := json.Unmarshal([]byte(raw), &paths); err != nil {
		return nil, fmt.Errorf("decoding session custody filesystem allowlist: %w", err)
	}
	if len(paths) == 0 || len(paths) > maxSessionCustodyPaths {
		return nil, fmt.Errorf("session custody filesystem allowlist has %d paths", len(paths))
	}
	unique := make(map[string]struct{}, len(paths))
	validated := make([]linuxCustodyMountSource, 0, len(paths))
	defer func() {
		if retErr != nil {
			closeLinuxCustodyMountSources(validated)
		}
	}()
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return nil, fmt.Errorf("session custody filesystem allowlist path is invalid: %q", path)
		}
		if linuxCustodyPathContains(path, scratch) || linuxCustodyPathContains(scratch, path) {
			return nil, fmt.Errorf("session custody filesystem allowlist overlaps scratch storage: %q", path)
		}
		if _, exists := unique[path]; exists {
			continue
		}
		source, err := pinLinuxCustodyMountSource(path)
		if err != nil {
			return nil, fmt.Errorf("pinning no-symlink session custody allowlist path %q: %w", path, err)
		}
		unique[path] = struct{}{}
		validated = append(validated, source)
	}
	sort.Slice(validated, func(i, j int) bool {
		if len(validated[i].path) == len(validated[j].path) {
			return validated[i].path < validated[j].path
		}
		return len(validated[i].path) < len(validated[j].path)
	})
	return validated, nil
}

func linuxCustodyPathContains(parent, child string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

func linuxCustodyDefaultReadOnlyPaths() []string {
	return []string{
		"/bin", "/sbin", "/usr", "/lib", "/lib64",
		"/etc/alternatives", "/etc/ca-certificates", "/etc/pki", "/etc/ssl",
		"/etc/group", "/etc/hosts", "/etc/localtime", "/etc/nsswitch.conf", "/etc/passwd", "/etc/resolv.conf",
	}
}

func pinLinuxCustodyMountSource(path string) (linuxCustodyMountSource, error) {
	rootFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return linuxCustodyMountSource{}, err
	}
	defer unix.Close(rootFD)
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_CLOEXEC),
		Resolve: uint64(unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS),
	}
	fd, err := unix.Openat2(rootFD, strings.TrimPrefix(path, "/"), how)
	if err != nil {
		return linuxCustodyMountSource{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return linuxCustodyMountSource{}, err
	}
	mode := stat.Mode & unix.S_IFMT
	if mode != unix.S_IFDIR && mode != unix.S_IFREG {
		_ = unix.Close(fd)
		return linuxCustodyMountSource{}, fmt.Errorf("unsupported mount source mode %#o", mode)
	}
	return linuxCustodyMountSource{path: path, fd: fd, directory: mode == unix.S_IFDIR}, nil
}

func closeLinuxCustodyMountSources(sources []linuxCustodyMountSource) {
	for index := range sources {
		if sources[index].fd >= 0 {
			_ = unix.Close(sources[index].fd)
			sources[index].fd = -1
		}
	}
}

func linuxCustodyMountTarget(root string, source linuxCustodyMountSource) (string, error) {
	target := filepath.Join(root, strings.TrimPrefix(source.path, string(filepath.Separator)))
	if source.directory {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return "", err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o600)
		if err != nil {
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
	}
	return target, nil
}

func bindLinuxCustodyPathReadOnly(root string, source linuxCustodyMountSource) error {
	target, err := linuxCustodyMountTarget(root, source)
	if err != nil {
		return fmt.Errorf("preparing session custody bind target for %q: %w", source.path, err)
	}
	flags := uintptr(unix.MS_BIND)
	if source.directory {
		flags |= unix.MS_REC
	}
	pinned := fmt.Sprintf("/proc/self/fd/%d", source.fd)
	if err := unix.Mount(pinned, target, "", flags, ""); err != nil {
		return fmt.Errorf("binding pinned session custody allowlist path %q: %w", source.path, err)
	}
	attr := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV}
	if err := unix.MountSetattr(unix.AT_FDCWD, target, unix.AT_RECURSIVE, attr); err != nil {
		return fmt.Errorf("hardening session custody allowlist path %q: %w", source.path, err)
	}
	return nil
}

func mountLinuxCustodyDevice(root, name string, flags int) error {
	fd, err := unix.Open(filepath.Join("/dev", name), flags|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening safe session device %s: %w", name, err)
	}
	defer unix.Close(fd)
	target := filepath.Join(root, "dev", name)
	file, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o666)
	if err != nil {
		return fmt.Errorf("creating safe session device target %s: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Mount(fmt.Sprintf("/proc/self/fd/%d", fd), target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("binding safe session device %s: %w", name, err)
	}
	return nil
}

func prepareLinuxCustodyDevices(root string) error {
	dev := filepath.Join(root, "dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return err
	}
	options := fmt.Sprintf("mode=0755,size=%d,nr_inodes=%d", linuxSessionDeviceBytes, linuxSessionDeviceInodes)
	if err := unix.Mount("tmpfs", dev, "tmpfs", unix.MS_NOSUID|unix.MS_NOEXEC|unix.MS_NODEV, options); err != nil {
		return fmt.Errorf("mounting private session device filesystem: %w", err)
	}
	for _, device := range []struct {
		name  string
		flags int
	}{{"null", unix.O_RDWR}, {"zero", unix.O_RDWR}, {"random", unix.O_RDONLY}, {"urandom", unix.O_RDONLY}} {
		if err := mountLinuxCustodyDevice(root, device.name, device.flags); err != nil {
			return err
		}
	}
	pts := filepath.Join(dev, "pts")
	if err := os.MkdirAll(pts, 0o755); err != nil {
		return err
	}
	ptsOptions := fmt.Sprintf("newinstance,ptmxmode=0666,mode=0620,gid=%d", os.Getgid())
	if err := unix.Mount("devpts", pts, "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, ptsOptions); err != nil {
		return fmt.Errorf("mounting private session devpts: %w", err)
	}
	for name, target := range map[string]string{
		"ptmx": "pts/ptmx", "fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2", "tty": "/proc/self/fd/0",
	} {
		if err := os.Symlink(target, filepath.Join(dev, name)); err != nil {
			return fmt.Errorf("creating private session device link %s: %w", name, err)
		}
	}
	sharedMemory := filepath.Join(dev, "shm")
	if err := os.MkdirAll(sharedMemory, 0o1777); err != nil {
		return err
	}
	sharedMemoryOptions := fmt.Sprintf("mode=1777,size=%d,nr_inodes=%d", linuxSessionSharedMemoryBytes, linuxSessionSharedMemoryInodes)
	if err := unix.Mount("tmpfs", sharedMemory, "tmpfs", unix.MS_NOSUID|unix.MS_NOEXEC|unix.MS_NODEV, sharedMemoryOptions); err != nil {
		return fmt.Errorf("mounting private bounded session shared memory: %w", err)
	}
	return nil
}

func prepareLinuxCustodyNamespace(scratch string) error {
	if !filepath.IsAbs(scratch) || !strings.HasPrefix(filepath.Base(scratch), linuxSessionScratchPrefix) {
		return errors.New("session custody scratch mountpoint is invalid")
	}
	allowedPaths, err := decodeLinuxCustodyAllowedPaths(os.Getenv(EnvSessionCustodyPaths), scratch)
	if err != nil {
		return err
	}
	defer closeLinuxCustodyMountSources(allowedPaths)
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("capturing session custody working directory: %w", err)
	}
	cwdAllowed := false
	for _, path := range allowedPaths {
		if linuxCustodyPathContains(path.path, cwd) {
			cwdAllowed = true
			break
		}
	}
	if !cwdAllowed {
		return fmt.Errorf("session custody working directory %q is not allowlisted", cwd)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making session custody mounts private: %w", err)
	}
	options := fmt.Sprintf("mode=0700,size=%d,nr_inodes=%d", linuxSessionScratchBytes, linuxSessionScratchInodes)
	if err := unix.Mount("tmpfs", scratch, "tmpfs", unix.MS_NOSUID|unix.MS_NOEXEC|unix.MS_NODEV, options); err != nil {
		return fmt.Errorf("mounting bounded session scratch storage: %w", err)
	}
	root := filepath.Join(scratch, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("creating session custody private root: %w", err)
	}
	defaultPaths := linuxCustodyDefaultReadOnlyPaths()
	mounted := make([]string, 0, len(defaultPaths)+len(allowedPaths))
	for _, path := range defaultPaths {
		if _, err := os.Lstat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat session custody mount source %q: %w", path, err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("canonicalizing default session custody path %q: %w", path, err)
		}
		source, err := pinLinuxCustodyMountSource(resolved)
		if err != nil {
			return fmt.Errorf("pinning no-symlink default session custody path %q: %w", path, err)
		}
		source.path = path
		covered := false
		for _, parent := range mounted {
			if linuxCustodyPathContains(parent, path) {
				covered = true
				break
			}
		}
		if !covered {
			if err := bindLinuxCustodyPathReadOnly(root, source); err != nil {
				_ = unix.Close(source.fd)
				return err
			}
			mounted = append(mounted, path)
		}
		_ = unix.Close(source.fd)
	}
	for _, source := range allowedPaths {
		covered := false
		for _, parent := range mounted {
			if linuxCustodyPathContains(parent, source.path) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		if err := bindLinuxCustodyPathReadOnly(root, source); err != nil {
			return err
		}
		mounted = append(mounted, source.path)
	}
	proc := filepath.Join(root, "proc")
	if err := os.MkdirAll(proc, 0o555); err != nil {
		return err
	}
	if err := unix.Mount("proc", proc, "proc", unix.MS_NOSUID|unix.MS_NOEXEC|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mounting private session custody procfs: %w", err)
	}
	if err := prepareLinuxCustodyDevices(root); err != nil {
		return err
	}
	for _, path := range []string{"/tmp", "/var/tmp", scratch} {
		if err := os.MkdirAll(filepath.Join(root, strings.TrimPrefix(path, "/")), 0o700); err != nil {
			return fmt.Errorf("creating private session writable path %q: %w", path, err)
		}
	}
	if err := unix.Chroot(root); err != nil {
		return fmt.Errorf("entering session custody private root: %w", err)
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("entering session custody working directory: %w", err)
	}
	if err := bringUpLinuxCustodyLoopback(); err != nil {
		return err
	}
	return nil
}

func dropLinuxCustodyCapabilities() error {
	for capability := 0; capability < 64; capability++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("dropping session custody capability %d: %w", capability, err)
		}
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("clearing session custody capabilities: %w", err)
	}
	return nil
}

var linuxCustodyAllowedEnvironment = map[string]struct{}{
	"PATH": {}, "SHELL": {}, "TERM": {}, "COLORTERM": {}, "LANG": {}, "LANGUAGE": {}, "TZ": {},
	"LC_ALL": {}, "LC_COLLATE": {}, "LC_CTYPE": {}, "LC_MESSAGES": {}, "LC_MONETARY": {}, "LC_NUMERIC": {}, "LC_TIME": {},
	"NO_COLOR": {}, "FORCE_COLOR": {},
	"BD_ACTOR": {}, "BD_BACKUP_ENABLED": {}, "BD_DOLT_AUTO_COMMIT": {}, "BD_OTEL_LOGS_URL": {}, "BD_OTEL_METRICS_URL": {},
	"BEADS_AGENT_NAME": {}, "BEADS_DOLT_AUTO_START": {}, "BEADS_DOLT_PORT": {}, "BEADS_DOLT_SERVER_HOST": {}, "BEADS_DOLT_SERVER_PORT": {},
	"GIT_AUTHOR_NAME": {}, "GIT_AUTHOR_EMAIL": {}, "GIT_COMMITTER_NAME": {}, "GIT_COMMITTER_EMAIL": {}, "GIT_CEILING_DIRECTORIES": {},
	"CLAUDE_CONFIG_DIR": {}, "CODEX_HOME": {}, "GEMINI_CONFIG_DIR": {}, "COPILOT_HOME": {},
	"ANTHROPIC_API_KEY": {}, "ANTHROPIC_AUTH_TOKEN": {}, "ANTHROPIC_CUSTOM_HEADERS": {},
	"ANTHROPIC_MODEL": {}, "ANTHROPIC_DEFAULT_HAIKU_MODEL": {}, "ANTHROPIC_DEFAULT_SONNET_MODEL": {}, "ANTHROPIC_DEFAULT_OPUS_MODEL": {},
	"OPENAI_API_KEY": {}, "GEMINI_API_KEY": {}, "GOOGLE_API_KEY": {}, "GH_TOKEN": {}, "GITHUB_TOKEN": {},
	"GT_AGENT": {}, "GT_CONTEXT_FILE": {}, "GT_DOLT_HOST": {}, "GT_DOLT_PORT": {}, "GT_NO_EMOJI": {}, "GT_NO_PAGER": {},
	"GT_DOG_NAME": {}, "GT_PROCESS_NAMES": {}, "GT_READY_PROMPT_PREFIX": {}, "GT_RIG": {}, "GT_ROLE": {}, "GT_ROOT": {}, "GT_RUN": {}, "GT_SCOPE": {}, "GT_SESSION": {}, "GT_THEME": {},
	"CLAUDECODE": {}, "CLAUDE_CODE_EFFORT_LEVEL": {}, "CLAUDE_CODE_SUBAGENT_MODEL": {},
	"CLAUDE_CODE_ENABLE_TELEMETRY": {}, "OTEL_METRICS_EXPORTER": {}, "OTEL_METRIC_EXPORT_INTERVAL": {},
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": {}, "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": {}, "OTEL_LOGS_EXPORTER": {},
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": {}, "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": {}, "OTEL_LOG_TOOL_DETAILS": {},
	"OTEL_LOG_TOOL_CONTENT": {}, "OTEL_LOG_USER_PROMPTS": {}, "OTEL_RESOURCE_ATTRIBUTES": {},
	"OPENCODE_PERMISSION": {}, "OPENCODE_CONFIG_CONTENT": {}, "NODE_OPTIONS": {},
}

func linuxCustodyWorkloadEnvironment(source []string) []string {
	filtered := make([]string, 0, len(source))
	for _, entry := range source {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, allowed := linuxCustodyAllowedEnvironment[key]; !allowed {
			continue
		}
		if key == "NODE_OPTIONS" {
			value = ""
		}
		filtered = append(filtered, key+"="+value)
	}
	return filtered
}

func runLinuxCustodyWorkload(command string, proxyPorts *linuxCustodyProxyPorts) error {
	env := linuxCustodyWorkloadEnvironment(os.Environ())
	if proxyPorts != nil {
		env = rewriteLinuxCustodyNetworkEnvironment(env, *proxyPorts)
	}
	if scratch := strings.TrimSpace(os.Getenv(envLinuxSessionScratch)); scratch != "" {
		env = append(env,
			"HOME="+scratch,
			"TMPDIR="+scratch,
			"XDG_CACHE_HOME="+filepath.Join(scratch, "cache"),
			"XDG_CONFIG_HOME="+filepath.Join(scratch, "config"),
			"XDG_DATA_HOME="+filepath.Join(scratch, "data"),
			"XDG_STATE_HOME="+filepath.Join(scratch, "state"),
		)
	}
	_, err := syscall.ForkExec(
		"/bin/sh",
		[]string{"/bin/sh", "-lc", command},
		&syscall.ProcAttr{Env: env, Files: []uintptr{0, 1, 2, ^uintptr(0), ^uintptr(0), ^uintptr(0), uintptr(linuxSessionBrokerFD)}},
	)
	if err != nil {
		return err
	}
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			for {
				time.Sleep(time.Hour)
			}
		}
		if err != nil {
			return err
		}
		_ = pid
	}
}

func runSessionWithCustody(_ string, command string, validate SessionBrokerValidator, detach SessionBrokerDetachPolicy, execute SessionBrokerExecutor) error {
	launch, contained, err := launchLinuxCustodyCommand(command, startLinuxCustodyCommand)
	if err != nil {
		return err
	}
	if !contained {
		for {
			time.Sleep(time.Hour)
			runtime.KeepAlive(launch)
		}
	}
	if launch.broker == nil || launch.tmux == nil || launch.control == nil || launch.proxies.HTTPS == nil {
		return errors.Join(errors.New("contained session is missing broker or proxy endpoints"), closeLinuxCustodyLaunch(launch, true))
	}
	brokerFD, dupErr := unix.Dup(int(launch.broker.Fd()))
	if dupErr != nil {
		return errors.Join(fmt.Errorf("retaining session broker endpoint: %w", dupErr), closeLinuxCustodyLaunch(launch, true))
	}
	if closeErr := launch.broker.Close(); closeErr != nil {
		_ = unix.Close(brokerFD)
		launch.broker = nil
		return errors.Join(fmt.Errorf("closing original session broker endpoint: %w", closeErr), closeLinuxCustodyLaunch(launch, true))
	}
	launch.broker = nil
	serviceContext, cancelServices := context.WithCancel(context.Background())
	defer cancelServices()
	serviceDone := make(chan linuxCustodyServiceResult, 2)
	go func() {
		serviceDone <- linuxCustodyServiceResult{name: "command broker", err: serveSessionBrokerWithPinnedTmux(serviceContext, "/proc/self/exe", launch.tmux, launch.control, brokerFD, validate, detach, execute)}
	}()
	go func() {
		serviceDone <- linuxCustodyServiceResult{name: "HTTPS proxy", err: serveHTTPSConnect(serviceContext, launch.proxies.HTTPS)}
	}()
	shutdownSignals := make(chan os.Signal, 1)
	signal.Notify(shutdownSignals, syscall.SIGTERM)
	defer signal.Stop(shutdownSignals)
	// Retain the namespace init pidfd and the supervisor-liveness writer until
	// exact teardown. The trusted init remains alive after reaping the workload,
	// so it is a stable direct-child custody boundary rather than an untrusted
	// PID-1 process or a signal-disposition-dependent zombie.
	timer := time.NewTimer(sessionCustodyReadyProbe)
	defer timer.Stop()
	initStopped := func(processErr error, remainingServices int, phase string) error {
		shutdownErr := shutdownLinuxCustodyServices(cancelServices, serviceDone, remainingServices, linuxCustodyBrokerShutdownTimeout)
		return errors.Join(
			finalizeLinuxCustodyInitExit(launch, processErr, phase),
			shutdownErr,
		)
	}
	select {
	case processErr := <-launch.wait:
		return initStopped(processErr, 2, "before readiness")
	case stopped := <-serviceDone:
		if stopped.err == nil {
			stopped.err = errors.New("service stopped without an error")
		}
		return errors.Join(
			fmt.Errorf("session %s stopped before readiness: %w", stopped.name, stopped.err),
			shutdownLinuxCustodyServices(cancelServices, serviceDone, 1, linuxCustodyBrokerShutdownTimeout),
			closeLinuxCustodyLaunch(launch, true),
		)
	case <-shutdownSignals:
		return shutdownLinuxCustodySupervisor(launch, cancelServices, serviceDone, 2, linuxCustodyBrokerShutdownTimeout, linuxCustodyReapTimeout)
	case <-timer.C:
	}
	for {
		select {
		case processErr := <-launch.wait:
			return initStopped(processErr, 2, "during service")
		case stopped := <-serviceDone:
			if stopped.err == nil {
				stopped.err = errors.New("service stopped without an error")
			}
			return errors.Join(
				fmt.Errorf("session %s stopped: %w", stopped.name, stopped.err),
				shutdownLinuxCustodyServices(cancelServices, serviceDone, 1, linuxCustodyBrokerShutdownTimeout),
				closeLinuxCustodyLaunch(launch, true),
			)
		case <-shutdownSignals:
			return shutdownLinuxCustodySupervisor(launch, cancelServices, serviceDone, 2, linuxCustodyBrokerShutdownTimeout, linuxCustodyReapTimeout)
		case <-time.After(time.Hour):
		}
		if launch.life == nil {
			return errors.New("session custody lost its supervisor liveness handle")
		}
		runtime.KeepAlive(launch)
	}
}

func finalizeLinuxCustodyInitExit(launch *linuxCustodyLaunch, processErr error, phase string) error {
	if launch == nil {
		return errors.New("session custody init exit has no launch state")
	}
	launch.wait = nil
	launch.child = nil
	if processErr == nil {
		processErr = errors.New("process exited without an error")
	}
	return errors.Join(
		fmt.Errorf("session custody init stopped %s: %w", phase, processErr),
		closeLinuxCustodyLaunch(launch, true),
	)
}

func shutdownLinuxCustodyServices(
	cancel context.CancelFunc,
	results <-chan linuxCustodyServiceResult,
	remaining int,
	timeout time.Duration,
) error {
	if cancel == nil || results == nil || remaining < 0 || timeout <= 0 {
		return errors.New("invalid session service shutdown request")
	}
	cancel()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var errs []error
	for range remaining {
		select {
		case result := <-results:
			if result.err != nil {
				errs = append(errs, fmt.Errorf("stopping session %s: %w", result.name, result.err))
			}
		case <-timer.C:
			errs = append(errs, fmt.Errorf("session services did not stop within %s: %w", timeout, context.DeadlineExceeded))
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}

func shutdownLinuxCustodySupervisor(
	launch *linuxCustodyLaunch,
	cancel context.CancelFunc,
	results <-chan linuxCustodyServiceResult,
	remaining int,
	serviceTimeout time.Duration,
	reapTimeout time.Duration,
) error {
	if cancel != nil {
		cancel()
	}
	var reapErr error
	if reapTimeout <= 0 {
		reapErr = errors.New("session custody supervisor init reap timeout must be positive")
	} else if launch == nil || launch.child == nil || launch.wait == nil {
		reapErr = errors.New("session custody supervisor lost its init reap handle")
	} else {
		var signalErr error
		if launch.pidfd >= 0 {
			signalErr = unix.PidfdSendSignal(launch.pidfd, unix.SIGKILL, nil, 0)
		} else if launch.child.Process != nil {
			signalErr = launch.child.Process.Kill()
		} else {
			signalErr = errors.New("session custody init has no process handle")
		}
		if errors.Is(signalErr, unix.ESRCH) || errors.Is(signalErr, os.ErrProcessDone) {
			signalErr = nil
		}

		waitErr := waitLinuxCustodyProcess(launch, reapTimeout)
		var exitErr *exec.ExitError
		if waitErr == nil || errors.As(waitErr, &exitErr) {
			launch.child = nil
			waitErr = nil
		}
		reapErr = errors.Join(signalErr, waitErr)
	}
	serviceErr := shutdownLinuxCustodyServices(cancel, results, remaining, serviceTimeout)
	cleanupErr := closeLinuxCustodyLaunch(launch, true)
	return errors.Join(reapErr, serviceErr, cleanupErr)
}

func linuxDirectChildren(procRoot string, pid int) ([]int, error) {
	taskRoot := fmt.Sprintf("%s/%d/task", procRoot, pid)
	tasks, err := os.ReadDir(taskRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errProcessNotFound
		}
		return nil, err
	}
	childrenByPID := make(map[int]struct{})
	for _, task := range tasks {
		tid, err := strconv.Atoi(task.Name())
		if err != nil || tid <= 0 {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("%s/%d/children", taskRoot, tid))
		if err != nil {
			// A Go runtime thread may exit between the task-directory scan and
			// this read. Its surviving children are reparented within the thread
			// group and will appear under another task; other failures are not
			// safe to ignore.
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, field := range strings.Fields(string(data)) {
			child, err := strconv.Atoi(field)
			if err != nil || child <= 0 {
				return nil, fmt.Errorf("invalid direct child PID %q", field)
			}
			childrenByPID[child] = struct{}{}
		}
	}
	children := make([]int, 0, len(childrenByPID))
	for child := range childrenByPID {
		children = append(children, child)
	}
	sort.Ints(children)
	return children, nil
}

func linuxUniqueDirectChildInCgroup(children []int, target string, cgroupOf func(int) (string, error)) (int, error) {
	if target == "" || cgroupOf == nil {
		return 0, errors.New("session child cgroup evidence is unavailable")
	}
	var matches []int
	for _, child := range children {
		cgroup, err := cgroupOf(child)
		if err != nil {
			return 0, fmt.Errorf("reading direct child %d cgroup: %w", child, err)
		}
		if cgroup == target {
			matches = append(matches, child)
		}
	}
	if len(matches) != 1 {
		return 0, fmt.Errorf("pane supervisor has %d direct children in the owned session cgroup, want one namespace init", len(matches))
	}
	return matches[0], nil
}

func linuxNamespacePIDs(procRoot string, pid int) ([]int, error) {
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/status", procRoot, pid))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errProcessNotFound
		}
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
		pids := make([]int, 0, len(fields))
		for _, field := range fields {
			value, err := strconv.Atoi(field)
			if err != nil || value <= 0 {
				return nil, fmt.Errorf("invalid namespace PID %q", field)
			}
			pids = append(pids, value)
		}
		if len(pids) == 0 {
			return nil, errors.New("process status has an empty NSpid field")
		}
		return pids, nil
	}
	return nil, errors.New("process status has no NSpid field")
}

func linuxPIDNamespaceInode(procRoot string, pid int) (uint64, error) {
	info, err := os.Stat(fmt.Sprintf("%s/%d/ns/pid", procRoot, pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, errProcessNotFound
		}
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return 0, errors.New("PID namespace has no inode identity")
	}
	return stat.Ino, nil
}

func validateLinuxNamespaceInit(procRoot string, supervisorPID, initPID int) (string, uint64, error) {
	stat, err := readLinuxProcessStatAt(procRoot, initPID)
	if err != nil {
		return "", 0, err
	}
	if stat.parentPID != supervisorPID {
		return "", 0, errors.New("session namespace init is not the pane supervisor's direct child")
	}
	nspids, err := linuxNamespacePIDs(procRoot, initPID)
	if err != nil {
		return "", 0, err
	}
	if len(nspids) < 2 || nspids[len(nspids)-1] != 1 {
		return "", 0, ErrSessionCustodyUnsupported
	}
	namespace, err := linuxPIDNamespaceInode(procRoot, initPID)
	if err != nil {
		return "", 0, err
	}
	return stat.startTime, namespace, nil
}

func readLinuxProcessStatAt(procRoot string, pid int) (linuxProcessStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", procRoot, pid))
	if err != nil {
		if os.IsNotExist(err) {
			return linuxProcessStat{}, errProcessNotFound
		}
		return linuxProcessStat{}, err
	}
	return parseLinuxProcessStat(data)
}

func retainSessionCustody(custody string, panePID int) (sessionCustodyHandle, error) {
	if !validSessionGenerationRe.MatchString(custody) {
		return nil, ErrSessionCustodyUnsupported
	}
	supervisorStat, err := readLinuxProcessStat(panePID)
	if err != nil || linuxProcessStateTerminal(supervisorStat.state) {
		return nil, errors.Join(ErrSessionCustodyUnsupported, err)
	}
	supervisorFD, err := unix.PidfdOpen(panePID, 0)
	if err != nil {
		return nil, errors.Join(ErrSessionCustodyUnsupported, err)
	}
	reject := func(primary error, initFD int) (sessionCustodyHandle, error) {
		var initCloseErr error
		if initFD >= 0 {
			initCloseErr = unix.Close(initFD)
		}
		return nil, errors.Join(ErrSessionCustodyUnsupported, primary, initCloseErr, unix.Close(supervisorFD))
	}
	supervisorCgroup, err := linuxCgroupDirectoryForPID(panePID)
	if err != nil {
		return reject(err, -1)
	}
	children, err := linuxDirectChildren(linuxProcRoot, panePID)
	if err != nil {
		return reject(err, -1)
	}
	initPID, err := linuxUniqueDirectChildInCgroup(children, supervisorCgroup, linuxCgroupDirectoryForPID)
	if err != nil {
		return reject(err, -1)
	}
	initIdentity, namespace, err := validateLinuxNamespaceInit(linuxProcRoot, panePID, initPID)
	if err != nil {
		return reject(err, -1)
	}
	initFD, err := unix.PidfdOpen(initPID, 0)
	if err != nil {
		return reject(err, -1)
	}
	confirmedSupervisor, err := readLinuxProcessStat(panePID)
	if err != nil || confirmedSupervisor.startTime != supervisorStat.startTime || linuxProcessStateTerminal(confirmedSupervisor.state) {
		return reject(errors.Join(ErrSessionGenerationChanged, err), initFD)
	}
	confirmedChildren, err := linuxDirectChildren(linuxProcRoot, panePID)
	if err != nil {
		return reject(errors.Join(ErrSessionGenerationChanged, err), initFD)
	}
	confirmedInitPID, err := linuxUniqueDirectChildInCgroup(confirmedChildren, supervisorCgroup, linuxCgroupDirectoryForPID)
	if err != nil || confirmedInitPID != initPID {
		return reject(errors.Join(ErrSessionGenerationChanged, err), initFD)
	}
	confirmedIdentity, confirmedNamespace, err := validateLinuxNamespaceInit(linuxProcRoot, panePID, initPID)
	if err != nil || confirmedIdentity != initIdentity || confirmedNamespace != namespace {
		return reject(errors.Join(ErrSessionGenerationChanged, err), initFD)
	}
	cgroup, err := linuxCgroupDirectoryForPID(initPID)
	if err != nil || cgroup != supervisorCgroup || !strings.HasPrefix(filepath.Base(cgroup), linuxSessionCgroupPrefix) {
		return reject(errors.Join(ErrSessionCustodyUnsupported, errors.New("trusted namespace init lacks bounded session cgroup"), err), initFD)
	}
	return &linuxSessionCustody{
		supervisorPID:      panePID,
		supervisorIdentity: supervisorStat.startTime,
		supervisorFD:       supervisorFD,
		initPID:            initPID,
		initIdentity:       initIdentity,
		initNamespace:      namespace,
		initFD:             initFD,
		cgroup:             cgroup,
	}, nil
}

func (custody *linuxSessionCustody) revalidate() (linuxProcessStat, error) {
	if custody == nil || custody.supervisorFD < 0 || custody.initFD < 0 {
		return linuxProcessStat{}, ErrSessionCustodyUnsupported
	}
	supervisor, err := readLinuxProcessStat(custody.supervisorPID)
	if err != nil || supervisor.startTime != custody.supervisorIdentity || linuxProcessStateTerminal(supervisor.state) {
		return linuxProcessStat{}, errors.Join(ErrSessionGenerationChanged, err)
	}
	identity, namespace, err := validateLinuxNamespaceInit(linuxProcRoot, custody.supervisorPID, custody.initPID)
	if err != nil || identity != custody.initIdentity || namespace != custody.initNamespace {
		return linuxProcessStat{}, errors.Join(ErrSessionGenerationChanged, err)
	}
	return readLinuxProcessStat(custody.initPID)
}

func (custody *linuxSessionCustody) Freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := custody.revalidate(); err != nil {
		return err
	}
	// PID namespace membership is inherited at clone time and cannot migrate
	// outward. No stop-the-world mutation is needed before the final checks.
	custody.prepared = true
	return nil
}

func (custody *linuxSessionCustody) Thaw() error {
	if custody != nil && !custody.committed {
		custody.prepared = false
	}
	return nil
}

func waitLinuxProcessTerminal(ctx context.Context, pid int, identity string) error {
	for {
		stat, err := readLinuxProcessStat(pid)
		if errors.Is(err, errProcessNotFound) || os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if stat.startTime != identity {
			return ErrSessionGenerationChanged
		}
		if linuxProcessStateTerminal(stat.state) {
			return nil
		}
		if err := waitForContext(ctx, processExitPollInterval); err != nil {
			return err
		}
	}
}

func waitLinuxProcessGone(ctx context.Context, pid int, identity string) error {
	for {
		stat, err := readLinuxProcessStat(pid)
		if errors.Is(err, errProcessNotFound) || os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if stat.startTime != identity {
			return ErrSessionGenerationChanged
		}
		if err := waitForContext(ctx, processExitPollInterval); err != nil {
			return err
		}
	}
}

func (custody *linuxSessionCustody) Kill(ctx context.Context) (bool, error) {
	committed, killErr := custody.KillBeforeParentRelease(ctx)
	finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), linuxCustodySupervisorExitTimeout)
	defer cancelFinalize()
	return committed, errors.Join(killErr, custody.FinalizeAfterParentRelease(finalizeCtx))
}

func (custody *linuxSessionCustody) KillBeforeParentRelease(ctx context.Context) (bool, error) {
	return custody.killBeforeParentReleaseWithBudgets(
		ctx,
		linuxCustodyKillBudgets{
			Init:       linuxCustodyInitExitTimeout,
			Broker:     linuxCustodyCooperativeShutdownTimeout,
			Supervisor: linuxCustodySupervisorExitTimeout,
		},
		linuxCustodyKillOps{
			revalidate: custody.revalidate,
			signal: func(fd int, signal unix.Signal) error {
				return unix.PidfdSendSignal(fd, signal, nil, 0)
			},
			waitTerminal: waitLinuxProcessTerminal,
			waitGone:     waitLinuxProcessGone,
		},
	)
}

func (custody *linuxSessionCustody) FinalizeAfterParentRelease(ctx context.Context) error {
	return custody.finalizeAfterParentReleaseWithOps(ctx, waitLinuxProcessGone, removeLinuxSessionCgroupWithContext)
}

func (custody *linuxSessionCustody) ParentReleaseFinalizationPending() bool {
	return custody != nil && custody.committed && !custody.finalized
}

func (custody *linuxSessionCustody) killWithBudgets(
	ctx context.Context,
	budgets linuxCustodyKillBudgets,
	ops linuxCustodyKillOps,
) (bool, error) {
	committed, killErr := custody.killBeforeParentReleaseWithBudgets(ctx, budgets, ops)
	finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), budgets.Supervisor)
	defer cancelFinalize()
	finalizeErr := custody.finalizeAfterParentReleaseWithOps(finalizeCtx, ops.waitGone, removeLinuxSessionCgroupWithContext)
	return committed, errors.Join(killErr, finalizeErr)
}

func (custody *linuxSessionCustody) killBeforeParentReleaseWithBudgets(
	ctx context.Context,
	budgets linuxCustodyKillBudgets,
	ops linuxCustodyKillOps,
) (bool, error) {
	if custody == nil || !custody.prepared {
		return false, errors.New("session PID namespace custody is not prepared")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if budgets.Init <= 0 || budgets.Broker <= 0 || budgets.Supervisor <= 0 {
		return false, errors.New("session custody teardown budgets must be positive")
	}
	if ops.revalidate == nil || ops.signal == nil || ops.waitTerminal == nil || ops.waitGone == nil {
		return false, errors.New("session custody teardown operations are unavailable")
	}
	initStat, err := ops.revalidate()
	if err != nil {
		return false, err
	}
	var errs []error
	waitPhase := func(wait func(context.Context, int, string) error, timeout time.Duration, pid int, identity, phase string) error {
		phaseCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := wait(phaseCtx, pid, identity); err != nil {
			return fmt.Errorf("%s: %w", phase, err)
		}
		return nil
	}
	if !linuxProcessStateTerminal(initStat.state) {
		if err := ops.signal(custody.initFD, unix.SIGKILL); err != nil {
			if !errors.Is(err, unix.ESRCH) {
				errs = append(errs, fmt.Errorf("signaling trusted namespace init: %w", err))
			}
		} else {
			custody.committed = true
			errs = append(errs, waitPhase(ops.waitTerminal, budgets.Init, custody.initPID, custody.initIdentity, "verifying trusted namespace init exit"))
		}
	}
	forceSupervisor := false
	if err := ops.signal(custody.supervisorFD, unix.SIGTERM); err != nil {
		if !errors.Is(err, unix.ESRCH) {
			errs = append(errs, fmt.Errorf("requesting session broker shutdown: %w", err))
			forceSupervisor = true
		}
	} else {
		custody.committed = true
		brokerErr := waitPhase(ops.waitTerminal, budgets.Broker, custody.supervisorPID, custody.supervisorIdentity, "waiting for session broker shutdown")
		errs = append(errs, brokerErr)
		forceSupervisor = brokerErr != nil
	}
	if forceSupervisor {
		if err := ops.signal(custody.supervisorFD, unix.SIGKILL); err != nil {
			if !errors.Is(err, unix.ESRCH) {
				errs = append(errs, fmt.Errorf("forcing session supervisor exit: %w", err))
			}
		} else {
			custody.committed = true
		}
	}
	if !custody.committed {
		errs = append(errs, ErrSessionGenerationChanged)
	}
	return custody.committed, errors.Join(errs...)
}

func (custody *linuxSessionCustody) finalizeAfterParentReleaseWithOps(
	ctx context.Context,
	waitGone func(context.Context, int, string) error,
	removeCgroup func(context.Context, string, time.Duration) error,
) error {
	if custody == nil || !custody.prepared || !custody.committed {
		return errors.New("session PID namespace custody has not committed teardown")
	}
	if waitGone == nil || removeCgroup == nil {
		return errors.New("session custody final reap operations are unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := waitGone(ctx, custody.supervisorPID, custody.supervisorIdentity); err != nil {
		return fmt.Errorf("verifying session supervisor reap: %w", err)
	}
	if custody.cgroup != "" {
		if err := clearLinuxSessionCgroupReceiptWithContext(ctx, &custody.cgroup, removeCgroup); err != nil {
			return err
		}
	}
	custody.finalized = true
	return nil
}

func (custody *linuxSessionCustody) Close() error {
	if custody == nil {
		return nil
	}
	var errs []error
	if custody.committed && !custody.finalized {
		errs = append(errs, errors.New("session custody final reap is unconfirmed; local ownership handles released"))
	}
	if custody.initFD >= 0 {
		fd := custody.initFD
		custody.initFD = -1
		errs = append(errs, unix.Close(fd))
	}
	if custody.supervisorFD >= 0 {
		fd := custody.supervisorFD
		custody.supervisorFD = -1
		errs = append(errs, unix.Close(fd))
	}
	if custody.finalized && custody.cgroup != "" {
		errs = append(errs, clearLinuxSessionCgroupReceipt(&custody.cgroup, removeLinuxSessionCgroup))
	}
	return errors.Join(errs...)
}
