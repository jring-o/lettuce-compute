package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/docker/docker/client"
)

// Engine-outage classification (TB-80).
//
// A container engine that stops answering under a running daemon — a Podman
// machine whose API socket died behind a "running" VM, Docker Desktop quit,
// a rootless socket file nothing listens on — used to be reported exactly
// like a unit that failed: Prepare's ping failure was a "prepare failure"
// (three of them paused the runtime for ten minutes, each one a billed
// abandon), and a create-time connection refusal in Execute was an
// "execution failure" counted against the LEAF. Neither breaker classified
// by WHAT went wrong, only by WHERE, so the volunteer was told the leaf keeps
// failing, the head billed every copy, and the engine was re-probed only by
// fetching a real unit and abandoning it every ten minutes. This file gives
// the condition its own error type so both breakers can tell an engine that
// is not there from a unit that is broken.

// EngineUnreachableError reports that the container engine's API did not
// answer: the socket refused, vanished, or dropped the connection. It is
// raised by the container runtime at every call that talks to the engine
// (Prepare's ping and pull, Execute's create/start/wait) and by the
// detector's registration ping. It is a RUNTIME outage, never a unit's own
// fault: the daemon takes the runtime out of service, gives every unrun unit
// back budget-neutral, and re-probes the engine until it answers.
type EngineUnreachableError struct {
	// Backend is the engine kind the runtime was connected to, and Socket
	// where; both may be empty for a runtime built without a detector.
	Backend ContainerBackend
	Socket  string
	// Err is the underlying transport error.
	Err error
}

// Error names the engine and the socket so the abandon reason the head
// records, the log line and the notice all say what the volunteer can act on.
func (e *EngineUnreachableError) Error() string {
	where := ""
	switch {
	case e.Backend != "" && e.Socket != "":
		where = fmt.Sprintf(" (%s at %s)", e.Backend, e.Socket)
	case e.Backend != "":
		where = fmt.Sprintf(" (%s)", e.Backend)
	case e.Socket != "":
		where = fmt.Sprintf(" (%s)", e.Socket)
	}
	if e.Err == nil {
		return "container engine unreachable" + where
	}
	return "container engine unreachable" + where + ": " + e.Err.Error()
}

// Unwrap exposes the transport error for errors.Is / errors.As.
func (e *EngineUnreachableError) Unwrap() error { return e.Err }

// IsEngineUnreachable reports whether err (anywhere in its chain) is an
// EngineUnreachableError.
func IsEngineUnreachable(err error) bool {
	var target *EngineUnreachableError
	return errors.As(err, &target)
}

// engineUnreachable wraps a transport failure from this runtime's engine as
// an EngineUnreachableError naming the backend and socket. A nil err yields
// nil.
func (c *ContainerRuntime) engineUnreachable(err error) error {
	if err == nil {
		return nil
	}
	return &EngineUnreachableError{Backend: c.backend, Socket: c.engineSocket, Err: err}
}

// classifyEngineError returns err wrapped as an EngineUnreachableError when it
// is a connection failure to the engine's API, and err unchanged otherwise. A
// context cancellation is never an outage: the daemon cancelled the call.
func (c *ContainerRuntime) classifyEngineError(err error) error {
	if err == nil || !isEngineConnectionError(err) {
		return err
	}
	return c.engineUnreachable(err)
}

// isEngineConnectionError reports whether err is a failure to reach the
// engine's API at the transport — as opposed to an answer FROM the engine
// that happens to describe a network problem of its own (a registry it could
// not resolve, a pull that timed out). The Docker client marks the former:
// every dial failure, timeout and "connection refused" on the socket becomes
// its connection-failed error, a permission-denied socket is wrapped with
// its own wording, and any other transport error carries the url.Error the
// HTTP client produced. An engine that dies mid-request shows as EOF on the
// response. Daemon-side errors are parsed HTTP responses and carry none of
// these, so an image-pull error whose TEXT says "dial tcp … no such host" is
// not an outage — the engine answered.
func isEngineConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if client.IsErrConnectionFailed(err) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	// A chain broken by %v formatting keeps the client's wording.
	msg := err.Error()
	for _, tell := range []string{
		"Cannot connect to the Docker daemon",
		"permission denied while trying to connect to the Docker daemon socket",
		"error during connect:",
	} {
		if strings.Contains(msg, tell) {
			return true
		}
	}
	return false
}
