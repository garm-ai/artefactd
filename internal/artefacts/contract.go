package artefacts

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"

	artefactsv1 "github.com/garm-ai/artefactd/gen/garm/artefacts/v1"
)

// ServiceName is the proto service this process implements.
const ServiceName = "garm.artefacts.v1.ArtefactsService"

// The tool names, as a caller and a manifest spell them. Read off the contract
// by the tests; written here so the service's own code refers to a tool by a
// constant rather than a literal.
const (
	ToolBeginWrite = "garm.artefacts.v1.begin_write"
	ToolCommit     = "garm.artefacts.v1.commit"
	ToolDescribe   = "garm.artefacts.v1.describe"
	ToolReadURL    = "garm.artefacts.v1.read_url"
)

// ServiceDescriptor is this contract's service, as the linked generated code
// carries it.
//
// Read rather than written down, because the descriptor is what this process
// actually serves: the tests assert their rules over it, and the card builder
// reads a tool's own policy off it so the endpoint floor a label is joined with
// is the floor the daemon will use.
func ServiceDescriptor() (protoreflect.ServiceDescriptor, error) {
	fd := (&artefactsv1.BeginWriteRequest{}).ProtoReflect().Descriptor().ParentFile()
	sd := fd.Services().ByName("ArtefactsService")
	if sd == nil {
		return nil, fmt.Errorf("artefacts: %s declares no ArtefactsService", fd.Path())
	}
	return sd, nil
}

// PolicyOf is one method's tool annotation.
//
// The card builder needs read_url's, because the endpoint's own clearance and
// compartments are the floor every label on a card it serves must be at or
// above — and reading it off the contract is what stops the floor in the card
// from drifting from the floor in the annotation when somebody edits one.
func PolicyOf(method protoreflect.Name) (*toolv1.ToolPolicy, error) {
	sd, err := ServiceDescriptor()
	if err != nil {
		return nil, err
	}
	md := sd.Methods().ByName(method)
	if md == nil {
		return nil, fmt.Errorf("artefacts: %s has no method %s", ServiceName, method)
	}
	p, ok := proto.GetExtension(md.Options(), toolv1.E_Tool).(*toolv1.ToolPolicy)
	if !ok || p == nil || p.GetName() == "" {
		return nil, fmt.Errorf("artefacts: %s declares no tool", md.FullName())
	}
	return p, nil
}
