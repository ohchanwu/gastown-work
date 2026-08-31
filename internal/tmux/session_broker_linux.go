//go:build linux

package tmux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	linuxSessionBrokerFD             = int(linuxCustodyBrokerFD)
	sessionBrokerDefaultDeadline     = 2 * time.Minute
	sessionBrokerWorkerStopTimeout   = 2 * time.Second
	sessionBrokerClientPollInterval  = 50 * time.Millisecond
	sessionBrokerMaxWorkers          = 8
	sessionBrokerDeniedExitCode      = 126
	sessionBrokerBusyExitCode        = 75
	sessionBrokerCanceledExitCode    = 125
	sessionBrokerCompletionFrameSize = 4
	sessionBrokerMaxStdinBytes       = 1 * 1024 * 1024
)

var errSessionBrokerStdinLimit = errors.New("session broker stdin exceeds byte limit")

type sessionBrokerQuotaReader struct {
	reader    io.Reader
	remaining int64
}

func (reader *sessionBrokerQuotaReader) Read(buffer []byte) (int, error) {
	if reader == nil || reader.reader == nil || reader.remaining < 0 {
		return 0, errors.New("invalid session broker stdin quota")
	}
	if reader.remaining == 0 {
		var extra [1]byte
		count, err := reader.reader.Read(extra[:])
		if count > 0 {
			return 0, errSessionBrokerStdinLimit
		}
		return 0, err
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	count, err := reader.reader.Read(buffer)
	reader.remaining -= int64(count)
	return count, err
}

// RunSessionBrokerClient routes this invocation through the inherited broker
// when descriptor 6 is the live SOCK_SEQPACKET endpoint. Ordinary invocations
// without that endpoint fall through to Cobra.
func RunSessionBrokerClient(args []string) (handled bool, exitCode int, err error) {
	socketType, err := unix.GetsockoptInt(linuxSessionBrokerFD, unix.SOL_SOCKET, unix.SO_TYPE)
	if errors.Is(err, unix.EBADF) || errors.Is(err, unix.ENOTSOCK) {
		return false, 0, nil
	}
	if err != nil {
		return true, 1, fmt.Errorf("checking inherited session broker: %w", err)
	}
	if socketType != unix.SOCK_SEQPACKET {
		return false, 0, nil
	}
	peer, err := unix.Getpeername(linuxSessionBrokerFD)
	if err != nil {
		return true, 1, fmt.Errorf("checking inherited session broker peer: %w", err)
	}
	if _, ok := peer.(*unix.SockaddrUnix); !ok {
		return true, 1, errors.New("inherited session broker is not an AF_UNIX endpoint")
	}
	stdin, closeStdin, err := sessionBrokerClientStdin(os.Stdin)
	if err != nil {
		return true, 1, err
	}
	if closeStdin != nil {
		defer closeStdin.Close()
	}
	exitCode, err = runSessionBrokerClientAtFD(
		linuxSessionBrokerFD,
		args,
		stdin,
		os.Stdout,
		os.Stderr,
		sessionBrokerDefaultDeadline,
	)
	return true, exitCode, err
}

// sessionBrokerClientStdin prevents an interactive terminal from keeping the
// trusted broker worker's stdin copier alive after a non-interactive gt command
// exits. Redirected files and pipes remain available, subject to the broker's
// byte quota, while an ordinary terminal is represented by an empty stream.
func sessionBrokerClientStdin(stdin *os.File) (*os.File, *os.File, error) {
	if stdin == nil {
		return nil, nil, errors.New("session broker stdin is unavailable")
	}
	info, err := stdin.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("checking session broker stdin: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return stdin, nil, nil
	}
	empty, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, fmt.Errorf("opening empty session broker stdin: %w", err)
	}
	return empty, empty, nil
}

func validateLinuxSessionBrokerEndpoint(fd int) error {
	socketType, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil {
		return fmt.Errorf("checking required session broker endpoint: %w", err)
	}
	if socketType != unix.SOCK_SEQPACKET {
		return fmt.Errorf("required session broker socket type is %d; want SOCK_SEQPACKET", socketType)
	}
	peer, err := unix.Getpeername(fd)
	if err != nil {
		return fmt.Errorf("checking required session broker peer: %w", err)
	}
	if _, ok := peer.(*unix.SockaddrUnix); !ok {
		return errors.New("required session broker is not an AF_UNIX endpoint")
	}
	return nil
}

func runSessionBrokerClientAtFD(
	brokerFD int,
	args []string,
	stdin, stdout, stderr *os.File,
	deadline time.Duration,
) (int, error) {
	if stdin == nil || stdout == nil || stderr == nil {
		return 1, errors.New("session broker stdio is unavailable")
	}
	if deadline <= 0 || deadline > time.Duration(sessionBrokerMaxDeadlineMS)*time.Millisecond {
		return 1, fmt.Errorf("session broker deadline %s is out of bounds", deadline)
	}
	request := sessionBrokerRequest{
		Version:    sessionBrokerProtocolVersion,
		DeadlineMS: uint32(deadline / time.Millisecond),
		Args:       append([]string(nil), args...),
	}
	frame, err := encodeSessionBrokerRequest(request)
	if err != nil {
		return 1, err
	}
	completionReader, completionWriter, err := os.Pipe()
	if err != nil {
		return 1, fmt.Errorf("creating session broker completion pipe: %w", err)
	}
	defer completionReader.Close()
	rights := unix.UnixRights(
		int(stdin.Fd()),
		int(stdout.Fd()),
		int(stderr.Fd()),
		int(completionWriter.Fd()),
	)
	written, sendErr := unix.SendmsgN(brokerFD, frame, rights, nil, unix.MSG_NOSIGNAL)
	closeErr := completionWriter.Close()
	if sendErr != nil {
		return 1, fmt.Errorf("sending session broker request: %w", sendErr)
	}
	if closeErr != nil {
		return 1, fmt.Errorf("closing session broker completion writer: %w", closeErr)
	}
	if written != len(frame) {
		return 1, io.ErrShortWrite
	}

	type completionResult struct {
		frame [sessionBrokerCompletionFrameSize]byte
		err   error
	}
	result := make(chan completionResult, 1)
	go func() {
		var completion completionResult
		_, completion.err = io.ReadFull(completionReader, completion.frame[:])
		result <- completion
	}()
	timer := time.NewTimer(deadline + sessionBrokerWorkerStopTimeout)
	defer timer.Stop()
	select {
	case completion := <-result:
		if completion.err != nil {
			return 1, fmt.Errorf("reading session broker completion: %w", completion.err)
		}
		return int(binary.BigEndian.Uint32(completion.frame[:])), nil
	case <-timer.C:
		_ = completionReader.Close()
		return 1, errors.New("timed out waiting for session broker completion")
	}
}

// ServeSessionBroker serves the inherited client endpoint until cancellation.
// The executable and validator are supplied by the trusted outer supervisor.
func ServeSessionBroker(ctx context.Context, executable string, fd int, validate SessionBrokerValidator) error {
	return serveSessionBroker(ctx, executable, fd, validate, sessionBrokerMaxWorkers)
}

// ServeSessionBrokerWithExecutor additionally handles selected validated
// commands inside the trusted broker process.
func ServeSessionBrokerWithExecutor(ctx context.Context, executable string, fd int, validate SessionBrokerValidator, execute SessionBrokerExecutor) error {
	return serveSessionBrokerWithExecutor(ctx, executable, fd, validate, execute, sessionBrokerMaxWorkers)
}

func serveSessionBrokerWithPinnedTmux(
	ctx context.Context,
	executable string,
	tmuxExecutable *os.File,
	controlCgroup *os.File,
	fd int,
	validate SessionBrokerValidator,
	detach SessionBrokerDetachPolicy,
	execute SessionBrokerExecutor,
) error {
	return serveSessionBrokerWithTools(ctx, executable, tmuxExecutable, controlCgroup, fd, validate, detach, execute, sessionBrokerMaxWorkers)
}

func serveSessionBroker(
	ctx context.Context,
	executable string,
	fd int,
	validate SessionBrokerValidator,
	maxWorkers int,
) error {
	return serveSessionBrokerWithExecutor(ctx, executable, fd, validate, nil, maxWorkers)
}

func serveSessionBrokerWithExecutor(
	ctx context.Context,
	executable string,
	fd int,
	validate SessionBrokerValidator,
	execute SessionBrokerExecutor,
	maxWorkers int,
) error {
	return serveSessionBrokerWithTools(ctx, executable, nil, nil, fd, validate, nil, execute, maxWorkers)
}

func serveSessionBrokerWithTools(
	ctx context.Context,
	executable string,
	tmuxExecutable *os.File,
	controlCgroup *os.File,
	fd int,
	validate SessionBrokerValidator,
	detach SessionBrokerDetachPolicy,
	execute SessionBrokerExecutor,
	maxWorkers int,
) error {
	defer unix.Close(fd)
	if executable == "" {
		return errors.New("session broker executable is empty")
	}
	if validate == nil {
		return errors.New("session broker validator is unavailable")
	}
	if maxWorkers < 1 {
		return errors.New("session broker worker limit must be positive")
	}
	executableFD, err := unix.Open(executable, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("pinning session broker executable: %w", err)
	}
	pinnedExecutable := os.NewFile(uintptr(executableFD), "session-broker-executable")
	if pinnedExecutable == nil {
		_ = unix.Close(executableFD)
		return errors.New("pinning session broker executable returned no file")
	}
	defer pinnedExecutable.Close()
	unix.CloseOnExec(fd)

	stopReceiver := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = unix.Shutdown(fd, unix.SHUT_RDWR)
		case <-stopReceiver:
		}
	}()
	defer close(stopReceiver)

	workers := make(chan struct{}, maxWorkers)
	var workerGroup sync.WaitGroup
	defer workerGroup.Wait()
	for {
		request, descriptors, err := receiveSessionBrokerRequest(fd)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, unix.ECONNRESET) || errors.Is(err, unix.ENOTCONN) {
				return err
			}
			continue
		}
		select {
		case workers <- struct{}{}:
			workerGroup.Add(1)
			go func() {
				defer workerGroup.Done()
				defer func() { <-workers }()
				handleSessionBrokerRequest(ctx, pinnedExecutable, tmuxExecutable, controlCgroup, validate, detach, execute, request, descriptors)
			}()
		default:
			rejectSessionBrokerRequest(descriptors, sessionBrokerBusyExitCode, "session broker is busy")
		}
	}
}

func receiveSessionBrokerRequest(fd int) (sessionBrokerRequest, []int, error) {
	frame := make([]byte, sessionBrokerMaxFrameBytes)
	rightsBuffer := make([]byte, unix.CmsgSpace(4*4))
	frameBytes, rightsBytes, flags, _, err := unix.Recvmsg(fd, frame, rightsBuffer, unix.MSG_CMSG_CLOEXEC)
	if err != nil {
		return sessionBrokerRequest{}, nil, err
	}
	if frameBytes == 0 && rightsBytes == 0 {
		return sessionBrokerRequest{}, nil, io.EOF
	}
	descriptors, err := parseSessionBrokerDescriptors(rightsBuffer[:rightsBytes])
	if err != nil {
		return sessionBrokerRequest{}, nil, err
	}
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		closeSessionBrokerDescriptors(descriptors)
		return sessionBrokerRequest{}, nil, errors.New("session broker request was truncated")
	}
	if len(descriptors) != 4 {
		closeSessionBrokerDescriptors(descriptors)
		return sessionBrokerRequest{}, nil, fmt.Errorf("session broker received %d descriptors; want 4", len(descriptors))
	}
	if err := validateSessionBrokerDescriptors(descriptors); err != nil {
		closeSessionBrokerDescriptors(descriptors)
		return sessionBrokerRequest{}, nil, err
	}
	request, err := decodeSessionBrokerRequest(frame[:frameBytes])
	if err != nil {
		rejectSessionBrokerRequest(descriptors, sessionBrokerDeniedExitCode, err.Error())
		return sessionBrokerRequest{}, nil, err
	}
	return request, descriptors, nil
}

func parseSessionBrokerDescriptors(control []byte) ([]int, error) {
	messages, err := unix.ParseSocketControlMessage(control)
	if err != nil {
		return nil, fmt.Errorf("parsing session broker descriptors: %w", err)
	}
	var descriptors []int
	for _, message := range messages {
		rights, rightsErr := unix.ParseUnixRights(&message)
		if rightsErr != nil {
			closeSessionBrokerDescriptors(descriptors)
			return nil, fmt.Errorf("parsing session broker rights: %w", rightsErr)
		}
		descriptors = append(descriptors, rights...)
	}
	return descriptors, nil
}

func validateSessionBrokerDescriptors(descriptors []int) error {
	for index, fd := range descriptors {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			return fmt.Errorf("checking session broker descriptor %d: %w", index, err)
		}
		access := flags & unix.O_ACCMODE
		if index == 0 && access == unix.O_WRONLY {
			return errors.New("session broker stdin descriptor is write-only")
		}
		if (index == 1 || index == 2) && access == unix.O_RDONLY {
			return fmt.Errorf("session broker output descriptor %d is read-only", index)
		}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(descriptors[3], &stat); err != nil {
		return fmt.Errorf("checking session broker completion descriptor: %w", err)
	}
	flags, err := unix.FcntlInt(uintptr(descriptors[3]), unix.F_GETFL, 0)
	if err != nil {
		return fmt.Errorf("checking session broker completion access: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFIFO || flags&unix.O_ACCMODE != unix.O_WRONLY {
		return errors.New("session broker completion descriptor is not a writable pipe")
	}
	return nil
}

func handleSessionBrokerRequest(
	serverContext context.Context,
	executable *os.File,
	tmuxExecutable *os.File,
	controlCgroup *os.File,
	validate SessionBrokerValidator,
	detach SessionBrokerDetachPolicy,
	execute SessionBrokerExecutor,
	request sessionBrokerRequest,
	descriptors []int,
) {
	stdin := os.NewFile(uintptr(descriptors[0]), "session-broker-stdin")
	stdout := os.NewFile(uintptr(descriptors[1]), "session-broker-stdout")
	stderr := os.NewFile(uintptr(descriptors[2]), "session-broker-stderr")
	defer stdin.Close()
	defer stdout.Close()
	defer stderr.Close()
	defer unix.Close(descriptors[3])
	if err := validate(request.Args); err != nil {
		writeSessionBrokerError(descriptors[2], fmt.Errorf("command denied: %w", err))
		writeSessionBrokerCompletion(descriptors[3], sessionBrokerDeniedExitCode)
		return
	}

	detached := detach != nil && detach(request.Args)
	requestParent := serverContext
	if detached {
		requestParent = context.Background()
	}
	requestContext, cancel := context.WithTimeout(requestParent, time.Duration(request.DeadlineMS)*time.Millisecond)
	defer cancel()
	if !detached {
		stopDisconnectMonitor := monitorSessionBrokerClientDisconnect(requestContext, descriptors[3], cancel)
		defer stopDisconnectMonitor()
	}
	if execute != nil {
		handled, executeErr := execute(requestContext, request.Args, stdin, stdout, stderr)
		if handled {
			exitCode := 0
			if executeErr != nil {
				exitCode = 1
				_, _ = fmt.Fprintln(stderr, executeErr)
			}
			writeSessionBrokerCompletion(descriptors[3], exitCode)
			return
		}
	}
	command := exec.CommandContext(requestContext, "/proc/self/fd/3", request.Args...)
	command.ExtraFiles = []*os.File{executable}
	command.Stdin = &sessionBrokerQuotaReader{reader: stdin, remaining: sessionBrokerMaxStdinBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = append(sanitizedSessionBrokerEnvironment(os.Environ()), EnvSessionBrokerWorker+"=1")
	if tmuxExecutable != nil {
		command.ExtraFiles = append(command.ExtraFiles, tmuxExecutable)
		command.Env = append(command.Env, envPinnedTmuxBinary+"=/proc/self/fd/4")
	}
	command.SysProcAttr = sessionBrokerWorkerProcessAttributes(controlCgroup)
	command.WaitDelay = sessionBrokerWorkerStopTimeout
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	err := command.Run()
	exitCode := sessionBrokerProcessExitCode(requestContext, err)
	if err != nil && exitCode == 1 {
		writeSessionBrokerError(descriptors[2], fmt.Errorf("worker failed: %w", err))
	}
	writeSessionBrokerCompletion(descriptors[3], exitCode)
}

func monitorSessionBrokerClientDisconnect(ctx context.Context, completionFD int, cancel context.CancelFunc) func() {
	monitorContext, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollFDs := []unix.PollFd{{Fd: int32(completionFD), Events: unix.POLLERR | unix.POLLHUP}}
		for {
			if monitorContext.Err() != nil {
				return
			}
			count, err := unix.Poll(pollFDs, int(sessionBrokerClientPollInterval/time.Millisecond))
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil || count > 0 && pollFDs[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				cancel()
				return
			}
		}
	}()
	return func() {
		stop()
		<-done
	}
}

func sessionBrokerWorkerProcessAttributes(controlCgroup *os.File) *syscall.SysProcAttr {
	attributes := &syscall.SysProcAttr{Setpgid: true}
	if controlCgroup != nil {
		attributes.UseCgroupFD = true
		attributes.CgroupFD = int(controlCgroup.Fd())
	}
	return attributes
}

func sessionBrokerProcessExitCode(ctx context.Context, err error) int {
	if err == nil {
		return 0
	}
	if ctx.Err() != nil {
		return sessionBrokerCanceledExitCode
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		if code := exitError.ExitCode(); code >= 0 {
			return code
		}
	}
	return 1
}

func sanitizedSessionBrokerEnvironment(env []string) []string {
	return withoutEnvironmentKeys(
		env,
		EnvSessionCustody,
		EnvSessionCustodyPaths,
		EnvSessionBrokerWorker,
		envLinuxSessionCustodyInit,
		envLinuxSessionCustodyCommand,
		envLinuxSessionCustodyNamespaced,
		envLinuxSessionScratch,
		envPinnedTmuxBinary,
	)
}

func rejectSessionBrokerRequest(descriptors []int, exitCode int, message string) {
	defer closeSessionBrokerDescriptors(descriptors)
	if len(descriptors) == 4 {
		writeSessionBrokerError(descriptors[2], errors.New(message))
		writeSessionBrokerCompletion(descriptors[3], exitCode)
	}
}

func writeSessionBrokerError(fd int, err error) {
	if err == nil {
		return
	}
	writeSessionBrokerNonblocking(fd, []byte("gt session broker: "+err.Error()+"\n"))
}

func writeSessionBrokerCompletion(fd int, exitCode int) {
	var frame [sessionBrokerCompletionFrameSize]byte
	binary.BigEndian.PutUint32(frame[:], uint32(exitCode))
	writeSessionBrokerNonblocking(fd, frame[:])
}

func writeSessionBrokerNonblocking(fd int, payload []byte) {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return
	}
	defer unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
	_, _ = unix.Write(fd, payload)
}

func closeSessionBrokerDescriptors(descriptors []int) {
	for _, fd := range descriptors {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
}
