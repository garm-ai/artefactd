// Command artefactd serves garm.artefacts.v1 over NATS.
//
// It is flag parsing and a call to artefactd.Serve. Everything the service does
// is in that function, so a development process that runs several garm
// components together starts this one the same way this binary does.
//
// Nothing here authenticates, authorises, checks a caller's clearance or redacts
// an answer. The daemon did all of that before the call arrived — and the one
// thing that looks like an exception is not one: the classification an artefact
// is stamped at is a label this service ATTACHES, and the daemon is what
// compares a viewer against it.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/nats-io/nats.go"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/tool-go/garmtool"

	"github.com/garm-ai/artefactd"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("artefactd stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	url := flag.String("nats", nats.DefaultURL, "NATS server URL")
	postgres := flag.String("postgres", os.Getenv("POSTGRES_DSN"),
		"Postgres DSN for this service's own database (required)")
	bucket := flag.String("bucket", os.Getenv("ARTEFACT_BUCKET"),
		"The one S3 bucket every tenant's prefix lives in (required)")
	endpoint := flag.String("s3-endpoint", os.Getenv("AWS_ENDPOINT_URL"),
		"S3 endpoint for anything that is not AWS — SeaweedFS, MinIO (default: AWS)")
	region := flag.String("s3-region", os.Getenv("AWS_REGION"),
		"Region the signer signs for (default us-east-1)")
	clearance := flag.String("clearance", "",
		"Clearance every artefact this deployment holds is stamped at (default INTERNAL)")
	compartments := flag.String("compartments", "",
		"Comma-separated compartments every artefact is stamped with")
	// Minutes, and configurable, because the number is a trade and not a
	// constant. Short is what makes a leaked link a dead link; the other side is
	// that a short window makes this service a hard dependency of every NEW read
	// while revoking nothing already issued. A deployment that reads its
	// artefacts through a slow client raises it deliberately and knows what it
	// bought.
	readTTL := flag.Duration("read-url-ttl", 0,
		"How long a presigned read URL is good for (default 5m)")
	uploadTTL := flag.Duration("upload-url-ttl", 0,
		"How long a presigned write URL is good for (default 15m)")
	sweep := flag.Duration("sweep-every", 0,
		"How often orphaned writes are removed and expired artefacts shredded (default 1m)")
	// Four, from the runtime rather than written down here, because the number
	// is arithmetic about the whole plane and not a taste of this service's:
	// every unit of concurrency is a micro service instance answering $SRV.INFO
	// separately, and garmd collects a discovery round into a channel buffered
	// at 64. A store whose calls are two round trips to Postgres and one
	// signature does not need more; a deployment that does runs more processes
	// behind the queue group, which costs the plane one identity each rather
	// than n.
	concurrency := flag.Int("concurrency", garmtool.DefaultConcurrency,
		"Calls in flight at once, per tool — one micro service instance each")
	flag.Parse()

	if *concurrency <= 0 {
		return errors.New("--concurrency must be at least 1")
	}
	level, err := clearanceFrom(*clearance)
	if err != nil {
		return err
	}

	// The same context the service drains on, cancelled by an interrupt or a
	// SIGTERM: one shutdown path, whether it is a person pressing control-C or
	// an orchestrator replacing the pod.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return artefactd.Serve(ctx, artefactd.Config{
		NATS: *url, Postgres: *postgres,
		Bucket: *bucket, Endpoint: *endpoint, Region: *region,
		Clearance: level, Compartments: splitList(*compartments),
		ReadTTL: *readTTL, UploadTTL: *uploadTTL, SweepEvery: *sweep,
		Concurrency: *concurrency, Version: version(), Log: log,
	})
}

// clearanceFrom reads --clearance, in either spelling, and refuses a name this
// build does not know rather than falling back.
//
// Falling back would be the wrong direction in this one place: clearance is
// ordinal and the unspecified zero is the LOWEST value, so a typo that read as
// unspecified would stamp every artefact at a label every caller reaches.
func clearanceFrom(s string) (toolv1.Clearance, error) {
	if strings.TrimSpace(s) == "" {
		return toolv1.Clearance_CLEARANCE_UNSPECIFIED, nil
	}
	name := strings.ToUpper(strings.TrimSpace(s))
	if !strings.HasPrefix(name, "CLEARANCE_") {
		name = "CLEARANCE_" + name
	}
	v, ok := toolv1.Clearance_value[name]
	if !ok || toolv1.Clearance(v) == toolv1.Clearance_CLEARANCE_UNSPECIFIED {
		return 0, errors.New("--clearance must be PUBLIC, INTERNAL, CONFIDENTIAL or RESTRICTED; " +
			"an unknown name would stamp every artefact at a label every caller reaches")
	}
	return toolv1.Clearance(v), nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// version is the module version the toolchain stamped, which the daemon reads
// back from the broker's service info: v0.1.0 when installed with
// `go install github.com/garm-ai/artefactd/cmd/artefactd@v0.1.0`. An in-tree
// build is stamped "(devel)", which is not a version the broker accepts, so it
// is reported as a development build.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "0.0.0-dev"
}
