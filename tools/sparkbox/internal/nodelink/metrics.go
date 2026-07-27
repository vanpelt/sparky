package nodelink

import (
	"context"
	"errors"
	"io"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// metricOutcome deliberately returns a closed vocabulary. Error strings often
// contain sandbox names, addresses, or request IDs and must never become labels.
func metricOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrLinkBacklogged):
		return "backlogged"
	case errors.Is(err, ErrLinkClosed), errors.Is(err, io.EOF):
		return "transport"
	}
	var control *ctlops.Error
	if errors.As(err, &control) && control.Code == CodeNodeBusy {
		return "busy"
	}
	var refused *xssh.OpenChannelError
	if errors.As(err, &refused) {
		return "refused"
	}
	return "error"
}

func metricDisconnectReason(err error) string {
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, context.Canceled):
		return "shutdown"
	case errors.Is(err, ErrLinkClosed):
		return "closed"
	case errors.Is(err, context.DeadlineExceeded):
		return "liveness"
	}
	var control *ctlops.Error
	if errors.As(err, &control) {
		switch control.Code {
		case CodeRevoked:
			return "revoked"
		case CodeSuperseded:
			return "superseded"
		}
		return "protocol"
	}
	return "transport"
}

func metricStreamKind(kind string) string {
	switch kind {
	case StreamSSH:
		return StreamSSH
	case StreamTCP:
		return StreamTCP
	default:
		return "unknown"
	}
}
