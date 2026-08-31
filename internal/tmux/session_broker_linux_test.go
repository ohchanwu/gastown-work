//go:build linux

package tmux

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSessionBrokerStdinQuotaRejectsOverflow(t *testing.T) {
	reader := &sessionBrokerQuotaReader{
		reader:    bytes.NewReader(bytes.Repeat([]byte("x"), sessionBrokerMaxStdinBytes+1)),
		remaining: sessionBrokerMaxStdinBytes,
	}
	payload, err := io.ReadAll(reader)
	if !errors.Is(err, errSessionBrokerStdinLimit) {
		t.Fatalf("overflow error = %v, want %v", err, errSessionBrokerStdinLimit)
	}
	if len(payload) != sessionBrokerMaxStdinBytes {
		t.Fatalf("accepted stdin bytes = %d, want %d", len(payload), sessionBrokerMaxStdinBytes)
	}
}

func TestTmuxCommandContextUsesPinnedBinaryWithHostilePATH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(envPinnedTmuxBinary, "/proc/self/fd/4")
	command := (&Tmux{}).commandContext(context.Background(), "list-sessions")
	if command.Path != "/proc/self/fd/4" {
		t.Fatalf("tmux command path = %q, want pinned descriptor", command.Path)
	}
}

func TestSessionBrokerRequestRoundTrip(t *testing.T) {
	want := sessionBrokerRequest{
		Version:    sessionBrokerProtocolVersion,
		DeadlineMS: 30_000,
		Args:       []string{"mail", "inbox", "--json"},
	}
	payload, err := encodeSessionBrokerRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSessionBrokerRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version || got.DeadlineMS != want.DeadlineMS || strings.Join(got.Args, "\x00") != strings.Join(want.Args, "\x00") {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestSessionBrokerRequestRejectsMalformedBounds(t *testing.T) {
	tests := []struct {
		name    string
		request sessionBrokerRequest
	}{
		{name: "wrong version", request: sessionBrokerRequest{Version: 0, DeadlineMS: 1_000, Args: []string{"prime"}}},
		{name: "zero deadline", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, Args: []string{"prime"}}},
		{name: "excessive deadline", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: sessionBrokerMaxDeadlineMS + 1, Args: []string{"prime"}}},
		{name: "no arguments", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000}},
		{name: "too many arguments", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: make([]string, sessionBrokerMaxArgs+1)}},
		{name: "empty argument", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"prime", ""}}},
		{name: "nul argument", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"mail", "inbox\x00--json"}}},
		{name: "oversized argument", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{strings.Repeat("x", sessionBrokerMaxArgBytes+1)}}},
		{name: "oversized frame", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: repeatedBrokerArguments(17, sessionBrokerMaxArgBytes)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := encodeSessionBrokerRequest(test.request); err == nil {
				t.Fatal("malformed request encoded successfully")
			}
		})
	}
}

func TestSessionBrokerRejectsCommandBeforeWorkerExec(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/sh", serverFD, func([]string) error {
			return errors.New("not reviewed")
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	proofPath := filepath.Join(t.TempDir(), "escaped")
	code, err := runSessionBrokerClientAtFD(
		clientFD,
		[]string{"-c", "touch " + proofPath},
		os.Stdin,
		os.Stdout,
		os.Stderr,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if code != sessionBrokerDeniedExitCode {
		t.Fatalf("denied exit code = %d, want %d", code, sessionBrokerDeniedExitCode)
	}
	if _, err := os.Stat(proofPath); !os.IsNotExist(err) {
		t.Fatalf("denied broker request executed worker; stat error = %v", err)
	}
}

func TestSessionBrokerRejectsNonPipeCompletionBeforeValidation(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	var validations atomic.Int32
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/false", serverFD, func([]string) error {
			validations.Add(1)
			return nil
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	frame, err := encodeSessionBrokerRequest(sessionBrokerRequest{
		Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"unsafe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completion, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer completion.Close()
	rights := unix.UnixRights(0, 1, 2, int(completion.Fd()))
	if _, err := unix.SendmsgN(clientFD, frame, rights, nil, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := validations.Load(); got != 0 {
		t.Fatalf("validator calls = %d, want 0", got)
	}
}

func TestSessionBrokerClientStdinReplacesTerminalLikeDeviceButPreservesPipe(t *testing.T) {
	device, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	got, closeGot, err := sessionBrokerClientStdin(device)
	if err != nil {
		t.Fatal(err)
	}
	if got == device || closeGot != got {
		t.Fatal("character-device stdin was not replaced with an owned empty stream")
	}
	if err := closeGot.Close(); err != nil {
		t.Fatal(err)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	got, closeGot, err = sessionBrokerClientStdin(reader)
	if err != nil {
		t.Fatal(err)
	}
	if got != reader || closeGot != nil {
		t.Fatal("redirected pipe stdin was not preserved")
	}
}

func TestSessionBrokerCancellationKillsWorkerProcessGroup(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/sh", serverFD, func([]string) error { return nil }, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
	})

	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutReader.Close()
	clientDone := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		defer stdoutWriter.Close()
		code, err := runSessionBrokerClientAtFD(
			clientFD,
			[]string{"-c", "sleep 30 & echo $!; wait"},
			os.Stdin,
			stdoutWriter,
			os.Stderr,
			30*time.Second,
		)
		clientDone <- struct {
			code int
			err  error
		}{code: code, err: err}
	}()

	pidLine := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		line, err := bufio.NewReader(stdoutReader).ReadString('\n')
		pidLine <- struct {
			line string
			err  error
		}{line: line, err: err}
	}()
	var childPID int
	select {
	case result := <-pidLine:
		if result.err != nil {
			t.Fatalf("reading broker worker barrier: %v", result.err)
		}
		childPID, err = strconv.Atoi(strings.TrimSpace(result.line))
		if err != nil || childPID <= 0 {
			t.Fatalf("broker worker child PID = %q: %v", result.line, err)
		}
	case result := <-clientDone:
		t.Fatalf("broker client exited before child barrier: code %d, error %v", result.code, result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("broker worker did not publish child PID barrier")
	}
	cancel()
	select {
	case result := <-clientDone:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.code != sessionBrokerCanceledExitCode {
			t.Fatalf("canceled exit code = %d, want %d", result.code, sessionBrokerCanceledExitCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("broker client did not receive cancellation completion")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("serveSessionBroker() cancellation error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveSessionBroker() did not stop after cancellation")
	}
	for attempt := 0; attempt < 200; attempt++ {
		stat, err := readLinuxProcessStat(childPID)
		if errorsIsProcessGoneOrTerminal(stat, err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("broker worker child PID %d survived cancellation", childPID)
}

func TestSessionBrokerClientDisconnectCancelsInProcessRequest(t *testing.T) {
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	completionReader, completionWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []*os.File{stdinReader, stdinWriter, stdoutReader, stdoutWriter, stderrReader, stderrWriter, completionReader, completionWriter} {
		defer file.Close()
	}
	duplicate := func(file *os.File) int {
		fd, err := unix.Dup(int(file.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}

	started := make(chan struct{})
	canceled := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleSessionBrokerRequest(
			context.Background(), nil, nil, nil,
			func([]string) error { return nil }, nil,
			func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) (bool, error) {
				close(started)
				<-ctx.Done()
				canceled <- ctx.Err()
				return true, ctx.Err()
			},
			sessionBrokerRequest{DeadlineMS: 5_000, Args: []string{"mail", "inbox"}},
			[]int{duplicate(stdinReader), duplicate(stdoutWriter), duplicate(stderrWriter), duplicate(completionWriter)},
		)
	}()

	<-started
	if err := completionReader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("disconnect cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("client disconnect did not cancel broker request")
	}
	select {
	case <-done:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("broker request did not return after client disconnect")
	}
}

func TestSessionBrokerRequestRejectsMalformedFrames(t *testing.T) {
	valid, err := encodeSessionBrokerRequest(sessionBrokerRequest{
		Version:    sessionBrokerProtocolVersion,
		DeadlineMS: 1_000,
		Args:       []string{"prime"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty", payload: nil},
		{name: "truncated", payload: valid[:len(valid)-1]},
		{name: "trailing data", payload: append(append([]byte(nil), valid...), 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeSessionBrokerRequest(test.payload); err == nil {
				t.Fatal("malformed frame decoded successfully")
			}
		})
	}
}

func TestSessionBrokerRunsWorkerWithPassedStdio(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/sh", serverFD, func(args []string) error {
			return nil
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutReader.Close()
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()

	exitCode, err := runSessionBrokerClientAtFD(clientFD, []string{"-c", "printf broker-ok; exit 7"}, stdin, stdoutWriter, stderr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 7 {
		t.Fatalf("exit code = %d, want 7", exitCode)
	}
	if err := stdoutWriter.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "broker-ok" {
		t.Fatalf("stdout = %q, want broker-ok", output)
	}
}

func TestSessionBrokerPinsExecutableBeforeServingRequests(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "broker-worker")
	writeWorker := func(output string) {
		t.Helper()
		if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '"+output+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeWorker("trusted")

	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, executable, serverFD, func([]string) error { return nil }, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	run := func() string {
		t.Helper()
		stdoutReader, stdoutWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		code, runErr := runSessionBrokerClientAtFD(clientFD, []string{"ignored"}, os.Stdin, stdoutWriter, stderr, time.Second)
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		if runErr != nil {
			t.Fatal(runErr)
		}
		if code != 0 {
			t.Fatalf("worker exit code = %d, want 0", code)
		}
		output, readErr := io.ReadAll(stdoutReader)
		_ = stdoutReader.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(output)
	}

	if got := run(); got != "trusted" {
		t.Fatalf("first worker output = %q, want trusted", got)
	}
	if err := os.Rename(executable, executable+".trusted"); err != nil {
		t.Fatal(err)
	}
	writeWorker("replaced")
	if got := run(); got != "trusted" {
		t.Fatalf("worker followed replaced executable path: output = %q", got)
	}
}

func TestSessionBrokerRejectsWrongDescriptorCountBeforeValidation(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var validations atomic.Int32
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/false", serverFD, func(args []string) error {
			validations.Add(1)
			return nil
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	frame, err := encodeSessionBrokerRequest(sessionBrokerRequest{
		Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"unsafe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rights := unix.UnixRights(0, 1, 2)
	if _, err := unix.SendmsgN(clientFD, frame, rights, nil, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := validations.Load(); got != 0 {
		t.Fatalf("validator calls = %d, want 0", got)
	}
}

func TestSessionBrokerTruncatedRightsDoNotLeakDescriptors(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	defer unix.Close(serverFD)
	defer unix.Close(clientFD)
	frame, err := encodeSessionBrokerRequest(sessionBrokerRequest{
		Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"unsafe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.SendmsgN(clientFD, frame, unix.UnixRights(0, 0, 0, 0, 0), nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiveSessionBrokerRequest(serverFD); err == nil {
		t.Fatal("receiveSessionBrokerRequest() accepted truncated rights")
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("open descriptor count after truncated rights = %d, want %d", len(after), len(before))
	}
}

func TestSessionBrokerConcurrencyLimitRejectsBusyRequest(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	firstValidated := make(chan struct{}, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/sh", serverFD, func(args []string) error {
			select {
			case firstValidated <- struct{}{}:
			default:
			}
			return nil
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	firstResult := make(chan int, 1)
	go func() {
		code, _ := runSessionBrokerClientAtFD(clientFD, []string{"-c", "sleep 0.3"}, os.Stdin, os.Stdout, os.Stderr, time.Second)
		firstResult <- code
	}()
	select {
	case <-firstValidated:
	case <-time.After(time.Second):
		t.Fatal("first request was not validated")
	}
	secondCode, err := runSessionBrokerClientAtFD(clientFD, []string{"-c", "exit 0"}, os.Stdin, os.Stdout, os.Stderr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if secondCode != sessionBrokerBusyExitCode {
		t.Fatalf("busy exit code = %d, want %d", secondCode, sessionBrokerBusyExitCode)
	}
	if code := <-firstResult; code != 0 {
		t.Fatalf("first exit code = %d, want 0", code)
	}
}

func TestSessionBrokerWorkerUsesPinnedControlCgroup(t *testing.T) {
	control, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	attributes := sessionBrokerWorkerProcessAttributes(control)
	if !attributes.Setpgid || !attributes.UseCgroupFD || attributes.CgroupFD != int(control.Fd()) {
		t.Fatalf("worker attributes = %+v", attributes)
	}
	withoutControl := sessionBrokerWorkerProcessAttributes(nil)
	if !withoutControl.Setpgid || withoutControl.UseCgroupFD {
		t.Fatalf("uncontained worker attributes = %+v", withoutControl)
	}
}

func newSessionBrokerSocketPair(t *testing.T) (serverFD, clientFD int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	return pair[0], pair[1]
}

func repeatedBrokerArguments(count, size int) []string {
	args := make([]string, count)
	for index := range args {
		args[index] = strings.Repeat("x", size)
	}
	return args
}
