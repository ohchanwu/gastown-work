package tmux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	sessionBrokerProtocolVersion  uint16 = 1
	sessionBrokerMaxDeadlineMS    uint32 = 10 * 60 * 1_000
	sessionBrokerMaxArgs                 = 64
	sessionBrokerMaxArgBytes             = 4 * 1_024
	sessionBrokerMaxFrameBytes           = 64 * 1_024
	sessionBrokerFrameHeaderBytes        = 8
)

type sessionBrokerRequest struct {
	Version    uint16
	DeadlineMS uint32
	Args       []string
}

// SessionBrokerValidator applies the command package's default-deny policy
// before the broker starts a process outside the containment namespace.
type SessionBrokerValidator func(args []string) error

// SessionBrokerDetachPolicy marks an already-validated request whose worker
// must outlive shutdown of the contained session that requested it.
type SessionBrokerDetachPolicy func(args []string) bool

// SessionBrokerExecutor handles an already-validated command inside the
// trusted broker process. Returning handled=false preserves the normal pinned
// worker execution path.
type SessionBrokerExecutor func(context.Context, []string, io.Reader, io.Writer, io.Writer) (handled bool, err error)

func encodeSessionBrokerRequest(request sessionBrokerRequest) ([]byte, error) {
	if err := validateSessionBrokerRequest(request); err != nil {
		return nil, err
	}

	frameSize := sessionBrokerFrameHeaderBytes
	for _, arg := range request.Args {
		frameSize += 2 + len(arg)
	}
	if frameSize > sessionBrokerMaxFrameBytes {
		return nil, fmt.Errorf("session broker request is %d bytes; limit is %d", frameSize, sessionBrokerMaxFrameBytes)
	}

	frame := make([]byte, frameSize)
	binary.BigEndian.PutUint16(frame[0:2], request.Version)
	binary.BigEndian.PutUint32(frame[2:6], request.DeadlineMS)
	binary.BigEndian.PutUint16(frame[6:8], uint16(len(request.Args)))
	offset := sessionBrokerFrameHeaderBytes
	for _, arg := range request.Args {
		binary.BigEndian.PutUint16(frame[offset:offset+2], uint16(len(arg)))
		offset += 2
		copy(frame[offset:offset+len(arg)], arg)
		offset += len(arg)
	}
	return frame, nil
}

func decodeSessionBrokerRequest(frame []byte) (sessionBrokerRequest, error) {
	if len(frame) < sessionBrokerFrameHeaderBytes {
		return sessionBrokerRequest{}, errors.New("session broker request has a truncated header")
	}
	if len(frame) > sessionBrokerMaxFrameBytes {
		return sessionBrokerRequest{}, fmt.Errorf("session broker request is %d bytes; limit is %d", len(frame), sessionBrokerMaxFrameBytes)
	}

	request := sessionBrokerRequest{
		Version:    binary.BigEndian.Uint16(frame[0:2]),
		DeadlineMS: binary.BigEndian.Uint32(frame[2:6]),
	}
	argCount := int(binary.BigEndian.Uint16(frame[6:8]))
	if argCount > sessionBrokerMaxArgs {
		return sessionBrokerRequest{}, fmt.Errorf("session broker request has %d arguments; limit is %d", argCount, sessionBrokerMaxArgs)
	}

	offset := sessionBrokerFrameHeaderBytes
	request.Args = make([]string, 0, argCount)
	for range argCount {
		if len(frame)-offset < 2 {
			return sessionBrokerRequest{}, errors.New("session broker request has a truncated argument length")
		}
		argSize := int(binary.BigEndian.Uint16(frame[offset : offset+2]))
		offset += 2
		if argSize > sessionBrokerMaxArgBytes {
			return sessionBrokerRequest{}, fmt.Errorf("session broker argument is %d bytes; limit is %d", argSize, sessionBrokerMaxArgBytes)
		}
		if len(frame)-offset < argSize {
			return sessionBrokerRequest{}, errors.New("session broker request has a truncated argument")
		}
		request.Args = append(request.Args, string(frame[offset:offset+argSize]))
		offset += argSize
	}
	if offset != len(frame) {
		return sessionBrokerRequest{}, errors.New("session broker request has trailing data")
	}
	if err := validateSessionBrokerRequest(request); err != nil {
		return sessionBrokerRequest{}, err
	}
	return request, nil
}

func validateSessionBrokerRequest(request sessionBrokerRequest) error {
	if request.Version != sessionBrokerProtocolVersion {
		return fmt.Errorf("unsupported session broker protocol version %d", request.Version)
	}
	if request.DeadlineMS == 0 || request.DeadlineMS > sessionBrokerMaxDeadlineMS {
		return fmt.Errorf("session broker deadline %dms is outside 1..%dms", request.DeadlineMS, sessionBrokerMaxDeadlineMS)
	}
	if len(request.Args) == 0 || len(request.Args) > sessionBrokerMaxArgs {
		return fmt.Errorf("session broker request has %d arguments; limit is 1..%d", len(request.Args), sessionBrokerMaxArgs)
	}
	for index, arg := range request.Args {
		if len(arg) == 0 || len(arg) > sessionBrokerMaxArgBytes {
			return fmt.Errorf("session broker argument %d is %d bytes; limit is 1..%d", index, len(arg), sessionBrokerMaxArgBytes)
		}
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("session broker argument %d contains NUL", index)
		}
	}
	return nil
}
