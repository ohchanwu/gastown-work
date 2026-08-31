//go:build !linux

package tmux

import "context"

func RunSessionBrokerClient([]string) (handled bool, exitCode int, err error) {
	return false, 0, nil
}

func ServeSessionBroker(context.Context, string, int, SessionBrokerValidator) error {
	return ErrSessionCustodyUnsupported
}

func ServeSessionBrokerWithExecutor(context.Context, string, int, SessionBrokerValidator, SessionBrokerExecutor) error {
	return ErrSessionCustodyUnsupported
}
