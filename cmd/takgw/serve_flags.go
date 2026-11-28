package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/health"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// serveUsage heads the flag list for `takgw serve -h`.
const serveUsage = `usage: takgw serve -source NAME (-stdin | -listen HOST:PORT | -connect HOST:PORT) [flags]

Runs the gateway: reads CoT, maps each event to a Lattice entity, drops repeats, late arrivals, unchanged reports and
suspect ones, checks what's left with Entity Preflight, and publishes it. By default each entity is written as one JSON
line to -out, the format "takgw preflight" reads. With -lattice it is published to the Lattice environment named by
LATTICE_ENDPOINT, ENVIRONMENT_TOKEN and, for Sandboxes, SANDBOXES_TOKEN.

Where the CoT comes from:
  -stdin             one stream on standard input, until it ends.
  -listen HOST:PORT  streams from TAK clients or servers that connect in. With -tls-cert, -tls-key and -tls-ca, only
                     clients presenting a certificate signed by -tls-ca are accepted (mutual TLS). Without them it is
                     plain TCP with no authentication: anyone who can reach the port can inject tracks.
  -connect HOST:PORT a TAK server's streaming TLS port (often 8089), connected to as a client with -tls-cert and
                     -tls-key, trusting only -tls-ca for the server. Reconnects with backoff. It never writes to the
                     server. Certificate files are PEM; convert a TAK .p12 with openssl.

Exit status: 0 after a clean stop (end of stdin, Ctrl-C or SIGTERM), 1 for a runtime failure (a malformed stdin
stream, a listener or output error, or credentials the platform refuses), 2 for a usage error. With -lattice, an
entity the platform rejects is counted and logged, and the gateway carries on.

flags:
`

// serveConfig is everything runServe reads from its flags.
type serveConfig struct {
	pipeline    pipeline.Config
	stdin       bool
	listen      string
	connect     string
	tls         *tls.Config    // the listener's or the dialer's TLS; nil for plain TCP
	health      *health.Config // the gateway's own asset; nil when -health-position isn't given
	out         string
	lattice     bool
	maxConns    int
	idleTimeout time.Duration
	verbose     bool
}

// Defaults for the connection flags.
const (
	defaultMaxConns    = 16
	defaultIdleTimeout = 5 * time.Minute
)

// parseServeFlags reads the flags. It returns the config and -1, or an exit status when runServe should stop now.
func parseServeFlags(args []string, stderr io.Writer) (serveConfig, int) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "name of this feed (required); it is part of every entity ID, so keep it stable")
	useStdin := fs.Bool("stdin", false, "read one CoT stream from stdin, and stop at its end")
	listen := fs.String("listen", "", "accept CoT streams on this host:port: mutual TLS with the -tls flags, otherwise plain TCP")
	connect := fs.String("connect", "", "connect to a TAK server's streaming TLS port at this host:port (needs the -tls flags)")
	tlsCert := fs.String("tls-cert", "", "this side's certificate, PEM")
	tlsKey := fs.String("tls-key", "", "the private key for -tls-cert, PEM")
	tlsCA := fs.String("tls-ca", "", "the authority, PEM, that the other side's certificate must be signed by")
	tlsServerName := fs.String("tls-server-name", "", "with -connect, the name the server's certificate must carry, if not the host dialed")
	out := fs.String("out", "-", `append published entities as JSON lines to this file; "-" is stdout`)
	lattice := fs.Bool("lattice", false, "publish to the Lattice environment in LATTICE_ENDPOINT, ENVIRONMENT_TOKEN and SANDBOXES_TOKEN instead of -out")
	simulated := fs.Bool("simulated", false, "mark every entity as simulated (do this for test data such as cot-gen's)")
	trackQuality := fs.Int("track-quality", mapping.DefaultTrackQuality, "tracked.trackQualityWrapper for every track, 0 to 15")
	classification := fs.String("classification", string(entity.ClassificationUnclassified), "dataClassification level for every entity")
	integration := fs.String("integration-name", mapping.DefaultIntegrationName, "provenance.integrationName")
	refresh := fs.Duration("refresh", state.DefaultRefresh, "republish an unchanged entity this often; negative publishes every report")
	maxEntities := fs.Int("max-entities", state.DefaultMaxEntities, "how many entities to track at once")
	maxConns := fs.Int("max-conns", defaultMaxConns, "with -listen, how many connections at once, counting only those past the TLS handshake")
	idle := fs.Duration("idle-timeout", defaultIdleTimeout, "with -listen or -connect, close a connection silent for this long")
	healthPosition := fs.String("health-position", "", "publish the gateway itself as an asset at lat,lon[,hae]: where it runs")
	healthName := fs.String("health-name", "", `the gateway asset's display name (default "takgw (SOURCE)")`)
	healthInterval := fs.Duration("health-interval", health.MaxInterval, "how often to republish the gateway asset, at most 30s")
	verbose := fs.Bool("v", false, "log every message's outcome")
	fs.Usage = func() {
		fmt.Fprint(stderr, serveUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return serveConfig{}, 0
		}
		return serveConfig{}, 2
	}

	// Check what the pipeline can't: the input choice, the connection limits, and the classification against the SDK.
	usage := func(format string, a ...any) (serveConfig, int) {
		fmt.Fprintf(stderr, "takgw serve: "+format+"\n", a...)
		return serveConfig{}, 2
	}
	switch {
	case fs.NArg() > 0:
		return usage("unexpected arguments: %v", fs.Args())
	case *source == "":
		return usage("-source is required: it names the feed, and is part of every entity ID")
	case howMany(*useStdin, *listen != "", *connect != "") != 1:
		return usage("choose exactly one of -stdin, -listen or -connect")
	case *maxConns < 1:
		return usage("-max-conns must be at least 1")
	case flagSet(fs, "max-conns") && *listen == "":
		return usage("-max-conns needs -listen")
	case *idle <= 0:
		return usage("-idle-timeout must be positive")
	case flagSet(fs, "idle-timeout") && *useStdin:
		return usage("-idle-timeout needs -listen or -connect")
	case *lattice && flagSet(fs, "out"):
		return usage("choose -lattice or -out, not both")
	}

	// TLS: all three files or none, only for a network input, and always for -connect.
	tlsFlags := howMany(*tlsCert != "", *tlsKey != "", *tlsCA != "")
	var tlsConfig *tls.Config
	switch {
	case tlsFlags != 0 && tlsFlags != 3:
		return usage("-tls-cert, -tls-key and -tls-ca go together: give all three")
	case tlsFlags == 3 && *useStdin:
		return usage("the -tls flags need -listen or -connect")
	case tlsFlags == 0 && *connect != "":
		return usage("-connect needs -tls-cert, -tls-key and -tls-ca: TAK servers take mutual TLS")
	case *tlsServerName != "" && *connect == "":
		return usage("-tls-server-name needs -connect")
	case tlsFlags == 3 && *connect != "":
		var err error
		if tlsConfig, err = transport.ClientTLS(*tlsCert, *tlsKey, *tlsCA, *tlsServerName); err != nil {
			return usage("%v", err)
		}
	case tlsFlags == 3:
		var err error
		if tlsConfig, err = transport.ServerTLS(*tlsCert, *tlsKey, *tlsCA); err != nil {
			return usage("%v", err)
		}
	}
	level := entity.ClassificationLevel(*classification)
	if err := preflight.CheckClassification(level); err != nil {
		return usage("-classification: %v", err)
	}
	cfg := serveConfig{
		pipeline: pipeline.Config{
			Mapping: mapping.Config{
				Source: *source, IntegrationName: *integration, Simulated: *simulated,
				TrackQuality: *trackQuality, Classification: level,
			},
			Limits: cot.DefaultLimits,
			State:  state.Config{Refresh: *refresh, MaxEntities: *maxEntities},
		},
		stdin: *useStdin, listen: *listen, connect: *connect, tls: tlsConfig, out: *out, lattice: *lattice,
		maxConns: *maxConns, idleTimeout: *idle, verbose: *verbose,
	}

	// The gateway's own asset, when asked for.
	switch {
	case *healthPosition != "":
		lat, lon, hae, err := parsePosition(*healthPosition)
		if err != nil {
			return usage("-health-position: %v", err)
		}
		cfg.health = &health.Config{
			Source: *source, Name: *healthName, Latitude: lat, Longitude: lon, Altitude: hae, Interval: *healthInterval,
			IntegrationName: *integration, Simulated: *simulated, Classification: level, Input: inputName(cfg),
		}
		if _, err := health.New(*cfg.health); err != nil {
			return usage("%v", err)
		}
	case *healthName != "" || flagSet(fs, "health-interval"):
		return usage("-health-name and -health-interval need -health-position")
	}

	// The pipeline checks these again, but checking now keeps a usage error from creating the -out file first.
	if err := cfg.pipeline.Mapping.Validate(); err != nil {
		return usage("%v", err)
	}
	if _, err := state.New(cfg.pipeline.State); err != nil {
		return usage("%v", err)
	}
	return cfg, -1
}

// parsePosition reads "lat,lon" or "lat,lon,hae" in degrees and meters. health.New checks the ranges.
func parsePosition(s string) (lat, lon float64, hae *float64, err error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, nil, fmt.Errorf("want lat,lon or lat,lon,hae, got %q", s)
	}
	var v [3]float64
	for i, p := range parts {
		if v[i], err = strconv.ParseFloat(strings.TrimSpace(p), 64); err != nil {
			return 0, 0, nil, fmt.Errorf("%q is not a number", p)
		}
	}
	if len(parts) == 3 {
		if math.IsNaN(v[2]) || math.IsInf(v[2], 0) {
			return 0, 0, nil, fmt.Errorf("altitude %q is not a finite number", parts[2])
		}
		hae = new(v[2])
	}
	return v[0], v[1], hae, nil
}

// inputName describes the feed, for the gateway asset's sensor.
func inputName(cfg serveConfig) string {
	switch {
	case cfg.connect != "":
		return "TAK server " + cfg.connect
	case cfg.listen != "" && cfg.tls != nil:
		return "TAK clients on " + cfg.listen + " (mutual TLS)"
	case cfg.listen != "":
		return "TAK clients on " + cfg.listen
	}
	return "stdin"
}

// howMany counts the true values.
func howMany(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// flagSet reports whether the named flag was given on the command line, as opposed to left at its default.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
