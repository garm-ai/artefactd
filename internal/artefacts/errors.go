package artefacts

import (
	"errors"
	"fmt"

	"github.com/garm-ai/tool-go/toolbind"

	"github.com/garm-ai/artefactd/internal/store"
)

// The codes this service answers with. They are the transport's strings, not a
// taxonomy of this service's invention, and which one means what is the
// daemon's contract with its callers.
const (
	// CodeRefused is an argument that will never be acceptable.
	CodeRefused = "400"
	// CodeDenied is a caller who may reach the tool but may not do this:
	// asking for a URL through an agent.
	CodeDenied = "403"
	// CodeNoSuchArtefact is every refusal that would otherwise tell a caller
	// which artefact ids exist. An artefact of another tenant and an id that
	// never existed get the same answer.
	CodeNoSuchArtefact = "404"
	// CodeConflict is a row that moved: an artefact something else committed
	// or swept first.
	CodeConflict = "409"
	// CodeGone is an artefact whose key has been destroyed.
	CodeGone = "410"
	// CodeBroke is this service's own fault, and never carries the reason.
	CodeBroke = "500"
)

// The three answers that must not be conflated, written down together because
// conflating any two of them either leaks or misleads.
//
//   - NOT FOUND: it never existed, or it is not yours. One answer for both, so
//     a caller cannot map another tenant's ids by the difference.
//   - GONE: it existed, its retention ran out, and its key was destroyed. The
//     record of it — and of who was given access to it — is still there. A
//     caller told "not found" here would go looking for a bug.
//   - DENIED: you may not have this. Note that the ORDINARY refusal of a read
//     is not this message at all: the daemon clears the card on the way out
//     and the caller receives an answer with an id and nothing in it. This
//     code is for the refusals this service makes itself, which are about how
//     the call was made rather than about who made it.
const (
	NotFoundMessage  = "no such artefact"
	GoneMessage      = "this artefact's key has been destroyed: the record of it remains and its contents do not"
	DelegatedMessage = "a URL for an artefact is not minted for an agent: the call arrives with a delegation chain. " +
		"A generated document is handed on as its id, and a person asks for the URL"
)

// refuse builds the error shape the tool runtime reads a code off.
//
// By value, never by pointer: the runtime matches with errors.As against a
// toolbind.CodedError target, which finds a value bare or wrapped and never a
// pointer, so a handler returning one of those would have its code silently
// dropped and answer 500 where the contract says 404.
func refuse(code, message string) error {
	return toolbind.CodedError{Code: code, Message: message}
}

func refusef(code, format string, args ...any) error {
	return toolbind.CodedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// fromStore turns a store failure into an answer.
//
// A missing row is "no such artefact" whatever the reason it is missing, and a
// row in the wrong state is a conflict. Anything else is this service's fault
// and the caller is told nothing about it — the reason goes to the log, which is
// where an operator looks.
func (s *Service) fromStore(err error, what string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return refuse(CodeNoSuchArtefact, NotFoundMessage)
	case errors.Is(err, store.ErrConflict):
		return refuse(CodeConflict, "this artefact moved: it was committed or swept by something else")
	default:
		s.log().Error(what, "err", err)
		return refuse(CodeBroke, "the artefact store could not be reached")
	}
}
