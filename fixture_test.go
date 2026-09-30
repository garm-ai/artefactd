package artefactd_test

// The fixture stands the service up the way a deployment does — through Serve,
// on a real broker, with a real Postgres — and talks to it the way the daemon
// does: the protobuf body on the contract's own subject, the caller's assertions
// in the Garm-Invocation header, and a reply awaited.
//
// Over a broker rather than by calling a handler, because three of the things
// this service can get wrong are invisible from inside the process: a service
// name the broker refuses, a subject the daemon never publishes to, and a
// refusal the runtime turns into a 500. Only a caller publishing where the
// contract says to publish can tell.
//
// Through Serve rather than by assembling a service by hand, because Serve is
// what the binary and a development stack both call, and a test that wired its
// own would be covering an arrangement nobody runs.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/garm-ai/contracts/callctx"
	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/contracts/wire"

	"github.com/garm-ai/artefactd"
	"github.com/garm-ai/artefactd/internal/blob"
	"github.com/garm-ai/artefactd/internal/store"
)

const (
	theTenant    = "example"
	theRun       = "run_01hq"
	theGenerator = "service:example.tools.v1.workbooks"
	thePerson    = "user:reader@example.com"
	theAgent     = "agent:example.agents.v1.Analyst"
	theTool      = "example.tools.v1.create_workbook"
	xlsx         = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// The routes, as the contract names them.
const (
	routeBegin    = "/garm.artefacts.v1.ArtefactsService/BeginWrite"
	routeCommit   = "/garm.artefacts.v1.ArtefactsService/Commit"
	routeDescribe = "/garm.artefacts.v1.ArtefactsService/Describe"
	routeReadURL  = "/garm.artefacts.v1.ArtefactsService/ReadUrl"
)

type fixture struct {
	NC     *nats.Conn
	Bucket string
	Blob   blob.Store
}

// newFixture starts the service on an embedded broker with its own Postgres
// schema, against whichever object store the environment provides: the plane's
// SeaweedFS when ARTEFACT_S3_ENDPOINT is set, and a local double otherwise.
func newFixture(t *testing.T, objects blob.Store) *fixture {
	t.Helper()
	dsn := store.TestDSN(t)
	url := embeddedNATS(t)

	f := &fixture{NC: connect(t, url), Blob: objects}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- artefactd.Serve(ctx, artefactd.Config{
			NATS: url, Postgres: dsn, Blob: objects,
			// Longer than any test, so the sweeper never runs under a case that
			// is not about it.
			SweepEvery: time.Hour,
			Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Serve did not return after its context was cancelled")
		}
	})
	waitForSubject(t, f.NC, wire.Subject(routeBegin))
	waitForSubject(t, f.NC, wire.Subject(routeReadURL))
	return f
}

// embeddedNATS runs a server on a port the kernel picks, so parallel packages
// cannot collide and the suite does not pass or fail on what else is on the
// machine.
func embeddedNATS(t *testing.T) string {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("building the embedded server: %v", err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("the embedded server never became ready")
	}
	return srv.ClientURL()
}

func connect(t *testing.T, url string) *nats.Conn {
	t.Helper()
	// Above the two-second default: under -race on a loaded machine the embedded
	// server's accept can take longer, and the failure then reads as a timeout
	// against a server that is fine.
	nc, err := nats.Connect(url, nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to the embedded server: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// waitForSubject polls the subject itself rather than sleeping: a fixed sleep
// either flakes on a loaded machine or is slow on every run. The probe carries no
// invocation context, so the runtime refuses it before any handler is reached —
// which is a reply, and a reply is all this waits for.
func waitForSubject(t *testing.T, nc *nats.Conn, subject string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := nc.Request(subject, nil, 250*time.Millisecond)
		if err == nil || !isNoResponder(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing ever answered on %s", subject)
		}
	}
}

func isNoResponder(err error) bool {
	return err == nats.ErrNoResponders || strings.Contains(err.Error(), "no responders")
}

// caller is who a request arrives as.
type caller struct {
	Subject string
	Kind    toolv1.PrincipalKind
	Act     []string
	RunID   string
}

// generator is a tool service writing an artefact while executing a run.
func generator() caller {
	return caller{
		Subject: theGenerator, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_SERVICE,
		RunID: theRun,
	}
}

// person is a person calling for themselves: no delegation chain, which is what
// read_url requires.
func person() caller {
	return caller{Subject: thePerson, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER}
}

// throughAnAgent is a person's run asking on their behalf, which is the shape
// every call made for a model arrives in.
func throughAnAgent() caller {
	return caller{
		Subject: thePerson, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
		Act: []string{theAgent}, RunID: theRun,
	}
}

// invocation is the header the daemon sets on every hop.
func (f *fixture) invocation(t *testing.T, c caller) string {
	t.Helper()
	ic := &toolv1.InvocationContext{
		Attribution: &toolv1.CallContext{
			Tenant: theTenant, App: "garmd", RunId: c.RunID, CorrelationId: "corr_1",
		},
		Principal: &toolv1.InvocationPrincipal{Subject: c.Subject, Kind: c.Kind},
		// The daemon sends its ledger event id as the call id, and the decoder
		// refuses a context without one.
		CallId: "ev_call_1",
	}
	for _, a := range c.Act {
		ic.Act = append(ic.Act, &toolv1.Act{
			Subject: a, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		})
	}
	enc, err := callctx.Encode(ic)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// call sends a request the way the daemon's transport does, and unmarshals the
// reply into out when there is one. It returns the error code the service
// answered with, empty on success.
func (f *fixture) call(t *testing.T, route string, c caller, req, out proto.Message) (string, string) {
	t.Helper()
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	msg := nats.NewMsg(wire.Subject(route))
	msg.Data = body
	msg.Header.Set(callctx.Header, f.invocation(t, c))

	reply, err := f.NC.RequestMsg(msg, 20*time.Second)
	if err != nil {
		t.Fatalf("requesting %s: %v", route, err)
	}
	if code := reply.Header.Get("Nats-Service-Error-Code"); code != "" {
		return code, reply.Header.Get("Nats-Service-Error")
	}
	if out != nil {
		if err := proto.Unmarshal(reply.Data, out); err != nil {
			t.Fatalf("decoding the answer of %s: %v", route, err)
		}
	}
	return "", ""
}

// ok is call for a request that must succeed.
func (f *fixture) ok(t *testing.T, route string, c caller, req, out proto.Message) {
	t.Helper()
	code, why := f.call(t, route, c, req, out)
	if code != "" {
		t.Fatalf("%s answered %s: %s", route, code, why)
	}
}
