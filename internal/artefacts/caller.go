// Package artefacts is the service behind garm.artefacts.v1: who may name an
// artefact, what its key is, what it is classified at, and who may be given a
// URL to it.
//
// Everything it knows about a caller comes from Garm-Invocation, which the
// daemon sets on the hop: the subject, the principal kind, the tenant, the
// delegation chain and the run the call belongs to. There is no token here and
// NO CLEARANCE — the invocation deliberately carries none, because a tool that
// could see a clearance is a tool that would start filtering on one, which is a
// second unreviewed copy of the policy.
//
// That is why the per-artefact answer is not computed here. This service
// LABELS what it releases at the artefact's own classification and the daemon
// drops what the viewer does not reach, walking the card's value at step 8.
// What this service decides is the part a label cannot express: that the caller
// is not an agent and is not acting through one, that the artefact is committed
// and not shredded, that it belongs to this tenant, and that a disclosure row
// exists before a credential does.
package artefacts

import (
	"context"

	"github.com/garm-ai/contracts/callctx"
	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
)

// Caller is the identity behind one call, read off the hop and nowhere else.
type Caller struct {
	Subject string
	Tenant  string
	Kind    toolv1.PrincipalKind
	// Act is the delegation chain, innermost last: whoever is acting for
	// Subject. An empty chain is a principal calling for itself.
	Act []Act
	// RunID is the run this call belongs to. It is the middle component of
	// every key this service builds, so a write without one has nowhere to go.
	RunID string
	// CallID is the daemon's own ledger event id for this call, which is what
	// makes a row here joinable to the ledger without this service writing to
	// it.
	CallID string
}

// Act is one link of the delegation chain.
type Act struct {
	Subject string
	Kind    toolv1.PrincipalKind
}

// Delegated reports whether somebody is acting for the subject.
//
// It is the check that keeps a credential away from a model, and it is a CHECK
// and not an audience: an audience decides who is OFFERED a tool in a listing
// and the daemon's visibility gate never reads it, so a tool relying on its
// audience to keep a caller out is a tool with no gate at all. That mistake
// cost a day on garm.tasks.v1, where decide_task and approval_card declared an
// audience and no tool set and became unreachable by every scoped role while
// remaining callable by an unscoped one.
func (c Caller) Delegated() bool { return len(c.Act) > 0 }

// Agent is the innermost delegate, or "" when the subject called for itself.
func (c Caller) Agent() string {
	if len(c.Act) == 0 {
		return ""
	}
	return c.Act[len(c.Act)-1].Subject
}

// Automated reports whether the caller is, or is acting through, something
// other than a person.
//
// Both halves are needed and neither is sufficient. A model's tool call arrives
// with a delegation chain naming its agent; a service calling on its own behalf
// arrives with no chain at all and a SERVICE principal. `read_url` refuses both.
func (c Caller) Automated() bool {
	if c.Delegated() {
		return true
	}
	return c.Kind != toolv1.PrincipalKind_PRINCIPAL_KIND_USER
}

// CallerFrom reads the invocation context off ctx.
//
// It asserts the shape and never the authority: the daemon already decided that
// this caller may reach this tool. What this service needs is a tenant to
// confine an object to, a subject to attribute a disclosure to, and a call id
// to join a row to the ledger by.
func CallerFrom(ctx context.Context) (Caller, error) {
	ic := callctx.FromContext(ctx)
	if ic == nil {
		// The tool runtime refuses a call with no invocation context before a
		// handler sees it, so reaching here means a runtime that decoded one
		// and did not attach it.
		return Caller{}, refuse(CodeRefused, "the call carries no invocation context")
	}
	c := Caller{
		Subject: ic.GetPrincipal().GetSubject(),
		Tenant:  ic.GetAttribution().GetTenant(),
		Kind:    ic.GetPrincipal().GetKind(),
		RunID:   ic.GetAttribution().GetRunId(),
		CallID:  ic.GetCallId(),
	}
	if c.Subject == "" {
		return Caller{}, refuse(CodeRefused, "the invocation context names no subject")
	}
	if c.Tenant == "" {
		// Without a tenant there is no prefix, and an object with no prefix is
		// the one failure this design cannot tolerate.
		return Caller{}, refuse(CodeRefused, "the invocation context names no tenant")
	}
	for _, a := range ic.GetAct() {
		c.Act = append(c.Act, Act{Subject: a.GetSubject(), Kind: a.GetKind()})
	}
	return c, nil
}
