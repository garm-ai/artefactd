// Package artefactd runs the artefact service.
//
// It is the whole of what `artefactd` does, as a function, so a process that
// wants to run several garm components together for development can start this
// one the same way the binary does — with a context it cancels and a
// configuration it built, rather than flags and an environment. cmd/artefactd is
// flag parsing and a call to Serve.
package artefactd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/tool-go/garmtool"

	artefactsv1 "github.com/garm-ai/artefactd/gen/garm/artefacts/v1"
	"github.com/garm-ai/artefactd/internal/artefacts"
	"github.com/garm-ai/artefactd/internal/blob"
	"github.com/garm-ai/artefactd/internal/codec"
	"github.com/garm-ai/artefactd/internal/store"
)

// Config is everything the service needs to run. Nothing here is read from the
// environment except the object store's credentials, which come from the AWS
// SDK's default chain the way every other garm component that reaches the same
// bucket takes them: a caller that wants environment variables reads them and
// fills this in, which is what makes one process able to run two of these.
type Config struct {
	// NATS is the broker to serve on.
	NATS string
	// Postgres is the DSN of this service's own database, under its own role. It
	// is migrated at startup and the service refuses to start if it cannot be
	// reached.
	Postgres string

	// Bucket is the one bucket every tenant's prefix lives in. Required: there
	// is no default, because a service that invented a bucket name would create
	// one and look like it worked.
	Bucket string
	// Endpoint is AWS_ENDPOINT_URL for anything that is not AWS. Empty means
	// AWS.
	Endpoint string
	// Region is what the signer signs for. Empty takes us-east-1, which an
	// S3-compatible store ignores and a signature still has to name.
	Region string

	// Blob replaces the object store, for a test. Zero builds an S3 client from
	// the three fields above.
	Blob blob.Store
	// Codec seals an artefact's payload. Zero is codec.Null, which is what a
	// development plane runs — and is NOT what a bank deploys.
	Codec codec.Codec

	// Clearance and Compartments are what every artefact this deployment holds
	// is stamped at. Zero clearance takes artefacts.DefaultClearance, which is
	// INTERNAL: the absence of a decision is not a decision to publish.
	Clearance    toolv1.Clearance
	Compartments []string

	// UploadTTL is how long a presigned PUT is good for. Zero takes the
	// service's default.
	UploadTTL time.Duration
	// ReadTTL is how long a presigned read URL is good for. Zero takes the
	// service's default, which is minutes. See README.md: a short window is the
	// window in which a leaked link is live, and it also makes this service a
	// hard dependency of every NEW read while doing nothing to revoke one
	// already issued.
	ReadTTL time.Duration
	// SweepEvery is how often orphaned writes are removed and expired artefacts
	// are shredded. Zero takes a minute.
	SweepEvery time.Duration

	// Concurrency is how many calls this process handles at once, per tool. It
	// is not a goroutine count: the runtime registers one micro service instance
	// per unit, each with its own subscriptions and its own $SRV.INFO identity,
	// so a large number here is a large number of responders in the plane's
	// discovery round rather than a cheap bound. Zero takes
	// garmtool.DefaultConcurrency.
	Concurrency int

	// Version is what the service advertises to the broker. A caller that leaves
	// it empty gets a development version.
	Version string
	// Log is where this service writes. Zero is a text handler on stderr.
	Log *slog.Logger
}

// ContractVersion is the version of garm.artefacts.v1 this build serves.
//
// It comes from the generated binding, which is stamped by the generator from
// buf.gen.yaml, rather than being written here — so the version a daemon
// compares against its catalogue and the version the binding was generated at
// cannot drift.
const ContractVersion = artefactsv1.ContractVersion

// defaultSweep is how often the sweeper runs when the caller names no interval.
const defaultSweep = time.Minute

// sweepBatch bounds one pass, so a backlog is worked through over several
// minutes rather than in one transaction that holds the table.
const sweepBatch = 200

// Serve runs the service until ctx is cancelled, then drains.
//
// The order is fixed and every step before the first subscription is a refusal
// that changes nothing: the configuration, the object store, the database and
// its migrations, the broker, then the endpoints. A service that came up without
// one of them would advertise itself and refuse every call, which reads as an
// outage rather than as the configuration mistake it is.
func Serve(ctx context.Context, cfg Config) error {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.Postgres == "" {
		return errors.New("artefactd: --postgres is required: this service keeps the artefact record")
	}

	objects := cfg.Blob
	if objects == nil {
		if cfg.Bucket == "" {
			return errors.New("artefactd: --bucket is required: a service that invented one " +
				"would create it and look like it worked")
		}
		s3, err := blob.NewS3(ctx, blob.Config{
			Bucket: cfg.Bucket, Endpoint: cfg.Endpoint, Region: cfg.Region,
		})
		if err != nil {
			return fmt.Errorf("artefactd: %w", err)
		}
		objects = s3
	}

	seal := cfg.Codec
	if seal == nil {
		seal = codec.Null{}
	}

	db, err := store.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer db.Close()
	schema, err := db.Migrate(ctx)
	if err != nil {
		return err
	}

	svc := &artefacts.Service{
		DB: db, Blob: objects, Codec: seal, Log: log,
		Classification: artefacts.Classification{
			Clearance: cfg.Clearance, Compartments: cfg.Compartments,
		},
		UploadTTL: cfg.UploadTTL, ReadTTL: cfg.ReadTTL,
	}
	handlers, err := artefacts.NewHandlers(svc)
	if err != nil {
		return fmt.Errorf("artefactd: %w", err)
	}

	url := cfg.NATS
	if url == "" {
		url = nats.DefaultURL
	}
	// Reconnect forever rather than exit: a service that dies because the broker
	// blinked turns a transient outage into a deployment event.
	nc, err := nats.Connect(url,
		nats.Name("artefactd"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("disconnected from nats", "err", err)
			}
		}),
	)
	if err != nil {
		return fmt.Errorf("artefactd: connecting to nats at %s: %w", url, err)
	}
	defer nc.Close()

	// The logger goes in so the runtime reports the configuration actually in
	// force — the concurrency included, whether this caller named one or took
	// the default — on this service's own log stream rather than nowhere. A
	// non-positive Concurrency leaves garmtool's default, which is what
	// WithConcurrency does with it, so there is no guard here.
	runtime := garmtool.New("artefacts", serviceVersion(cfg.Version),
		garmtool.WithConcurrency(cfg.Concurrency),
		garmtool.WithLogger(log),
	)
	if err := artefactsv1.ServeArtefactsService(runtime, handlers); err != nil {
		return fmt.Errorf("artefactd: registering %s: %w", artefacts.ServiceName, err)
	}

	sweep := cfg.SweepEvery
	if sweep <= 0 {
		sweep = defaultSweep
	}
	go sweeper(ctx, svc, sweep, log)

	// The effective presigned read window is on this line deliberately. It is a
	// security parameter and an availability parameter at once, and an operator
	// reading a log should see the number in force rather than the flag they did
	// or did not pass.
	log.Info("serving", "service", artefacts.ServiceName, "version", serviceVersion(cfg.Version),
		"contract", ContractVersion, "nats", nc.ConnectedUrl(), "schema", schema,
		"bucket", cfg.Bucket, "endpoint", cfg.Endpoint, "codec", seal.Name(),
		"read_url_ttl", svc.ReadWindow(), "clearance", svc.Classification.Clearance.String(),
		"compartments", cfg.Compartments)
	if err := runtime.Run(ctx, nc); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("drained")
	return nil
}

// sweeper removes what was never written and shreds what outlived its
// retention. Both are the cost the design accepted rather than a background
// tidy-up: an uncommitted write is ruling 2's acknowledged orphan, and shredding
// at expiry is how ruling 5's retention is actually enforced.
func sweeper(ctx context.Context, svc *artefacts.Service, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			out, err := svc.Sweep(ctx, sweepBatch)
			if err != nil && ctx.Err() == nil {
				log.Warn("sweeping artefacts", "err", err)
				continue
			}
			if out.Orphans > 0 || out.Shredded > 0 {
				log.Info("swept", "orphans", out.Orphans, "shredded", out.Shredded)
			}
		}
	}
}

func serviceVersion(v string) string {
	if v == "" {
		return "0.0.0-dev"
	}
	return v
}
