package artefacts

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"

	artefactsv1 "github.com/garm-ai/artefactd/gen/garm/artefacts/v1"
	"github.com/garm-ai/artefactd/internal/store"
)

// Handlers is the contract's four methods over the service.
//
// It translates and nothing else: every rule about who may do what is in
// Service, so a reader looking for the checks finds them in one file and a
// second transport would not be a second set of them.
//
// It satisfies artefactsv1.ArtefactsServiceHandler, which the generated binding
// requires and which has no Unimplemented embed — a tool added to the .proto
// and not implemented here fails to COMPILE rather than mounting and answering
// Unimplemented to a real caller.
type Handlers struct {
	Svc *Service
	// readPolicy is read_url's own tool annotation, which is the floor every
	// label on the card it serves is joined with. Loaded once at registration
	// rather than per call, and off the contract rather than written down.
	readPolicy *toolv1.ToolPolicy
}

// NewHandlers loads what the handlers need off the contract, and refuses to
// build if the contract does not carry it. A service that came up without
// read_url's policy would serve cards labelled at a floor it guessed.
func NewHandlers(svc *Service) (*Handlers, error) {
	p, err := PolicyOf("ReadUrl")
	if err != nil {
		return nil, err
	}
	return &Handlers{Svc: svc, readPolicy: p}, nil
}

var _ artefactsv1.ArtefactsServiceHandler = (*Handlers)(nil)

// BeginWrite names an artefact and hands back somewhere to put it.
func (h *Handlers) BeginWrite(ctx context.Context, req *artefactsv1.BeginWriteRequest) (*artefactsv1.BeginWriteResponse, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	out, err := h.Svc.Begin(ctx, c, BeginInput{
		MediaType: req.GetMediaType(),
		Label:     req.GetLabel(),
		ToolFQN:   req.GetToolFqn(),
		Retention: time.Duration(req.GetRetentionSeconds()) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return &artefactsv1.BeginWriteResponse{
		ArtefactId:      proto.String(out.Artefact.ID),
		UploadUrl:       proto.String(out.UploadURL),
		UploadExpiresAt: timestamppb.New(out.ExpiresAt),
		Codec:           proto.String(out.Artefact.Codec),
		DataKey:         out.DataKey,
	}, nil
}

// Commit records that the bytes are there.
func (h *Handlers) Commit(ctx context.Context, req *artefactsv1.CommitRequest) (*artefactsv1.CommitResponse, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	a, err := h.Svc.Commit(ctx, c, CommitInput{
		ID:                 req.GetArtefactId(),
		PlaintextSHA256:    req.GetPlaintextSha256(),
		PlaintextSizeBytes: req.GetPlaintextSizeBytes(),
		StoredSHA256:       req.GetStoredSha256(),
		StoredSizeBytes:    req.GetStoredSizeBytes(),
	})
	if err != nil {
		return nil, err
	}
	out := &artefactsv1.CommitResponse{
		ArtefactId: proto.String(a.ID),
		State:      stateProto(a.State).Enum(),
	}
	if a.CommittedAt != nil {
		out.CommittedAt = timestamppb.New(*a.CommittedAt)
	}
	if a.RetainUntil != nil {
		out.RetainUntil = timestamppb.New(*a.RetainUntil)
	}
	return out, nil
}

// Describe is what an artefact is, without the way in.
func (h *Handlers) Describe(ctx context.Context, req *artefactsv1.DescribeRequest) (*artefactsv1.DescribeResponse, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	a, err := h.Svc.Get(ctx, c, req.GetArtefactId())
	if err != nil {
		return nil, err
	}
	clearance, _ := clearanceOf(a.Clearance)
	out := &artefactsv1.DescribeResponse{
		ArtefactId: proto.String(a.ID),
		State:      stateProto(a.State).Enum(),
		Classification: &artefactsv1.Classification{
			Clearance:    clearance.Enum(),
			Compartments: a.Compartments,
		},
		MediaType:          proto.String(a.MediaType),
		Label:              proto.String(a.Label),
		ToolFqn:            proto.String(a.ToolFQN),
		RunId:              proto.String(a.RunID),
		PlaintextSha256:    proto.String(a.PlaintextSHA256),
		PlaintextSizeBytes: proto.Uint64(uint64(a.PlaintextSizeBytes)),
		StoredSizeBytes:    proto.Uint64(uint64(a.StoredSizeBytes)),
		Codec:              proto.String(a.Codec),
		CreatedAt:          timestamppb.New(a.CreatedAt),
	}
	if a.CommittedAt != nil {
		out.CommittedAt = timestamppb.New(*a.CommittedAt)
	}
	if a.RetainUntil != nil {
		out.RetainUntil = timestamppb.New(*a.RetainUntil)
	}
	if a.ShreddedAt != nil {
		out.ShreddedAt = timestamppb.New(*a.ShreddedAt)
	}
	return out, nil
}

// ReadUrl mints a URL and releases a key, inside a card the daemon projects.
//
// The response carries the id and the card, and nothing else. There is no plain
// URL field to fall back to, which is the point: a caller the daemon withholds
// the card from receives the id it already had.
func (h *Handlers) ReadUrl(ctx context.Context, req *artefactsv1.ReadUrlRequest) (*artefactsv1.ReadUrlResponse, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	released, err := h.Svc.ReadURL(ctx, c, req.GetArtefactId())
	if err != nil {
		return nil, err
	}
	return &artefactsv1.ReadUrlResponse{
		ArtefactId: proto.String(released.Artefact.ID),
		Release:    ReleaseCard(h.readPolicy, released),
	}, nil
}

// stateProto maps a row's state to the contract's enum.
//
// A switch and not a lookup by name: the database spells a state PENDING and
// the enum spells it STATE_PENDING, so a name lookup would answer
// STATE_UNSPECIFIED for every state and nothing would fail. A state added to
// one and not the other is a compile error here instead.
func stateProto(s store.State) artefactsv1.State {
	switch s {
	case store.StatePending:
		return artefactsv1.State_STATE_PENDING
	case store.StateCommitted:
		return artefactsv1.State_STATE_COMMITTED
	case store.StateShredded:
		return artefactsv1.State_STATE_SHREDDED
	default:
		return artefactsv1.State_STATE_UNSPECIFIED
	}
}
