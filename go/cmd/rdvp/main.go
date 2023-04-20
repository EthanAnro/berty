package main

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	// nolint:staticcheck
	libp2p_rp "github.com/berty/go-libp2p-rendezvous"
	libp2p_rpdb "github.com/berty/go-libp2p-rendezvous/db/sqlcipher"
	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/config"
	libp2p_ci "github.com/libp2p/go-libp2p/core/crypto"
	libp2p_host "github.com/libp2p/go-libp2p/core/host"
	metrics "github.com/libp2p/go-libp2p/core/metrics"
	libp2p_peer "github.com/libp2p/go-libp2p/core/peer"
	libp2p_relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/oklog/run"
	ff "github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"moul.io/srand"

	"berty.tech/berty/v2/go/pkg/errcode"
	"berty.tech/weshnet/pkg/ipfsutil"
	"berty.tech/weshnet/pkg/logutil"
	"berty.tech/weshnet/pkg/rendezvous"
)

func main() {
	log.SetFlags(0)

	// opts
	var (
		logFormat             = "color"   // json, console, color, light-console, light-color
		logToFile             = "stderr"  // can be stdout, stderr or a file path
		logFilters            = "info+:*" // info and more for everything
		serveURN              = ":memory:"
		serveListeners        = "/ip4/0.0.0.0/tcp/4040,/ip4/0.0.0.0/udp/4141/quic"
		servePK               = ""
		sharekeyPK            = ""
		serveAnnounce         = ""
		serveMetricsListeners = ""
		genkeyType            = "Ed25519"
		genkeyLength          = 2048
		emitterServer         = ""
		emitterPublicAddr     = ""
		emitterAdminKey       = ""
	)

	// parse opts
	var (
		serveFlags    = flag.NewFlagSet("serve", flag.ExitOnError)
		sharekeyFlags = flag.NewFlagSet("sharekey", flag.ExitOnError)
		genkeyFlags   = flag.NewFlagSet("genkey", flag.ExitOnError)
	)
	setupGlobalFlags := func(fs *flag.FlagSet) {
		fs.StringVar(&logFilters, "log.filters", logFilters, "logged namespaces")
		fs.StringVar(&logFormat, "log.format", logFormat, "if specified, will override default log format")
		fs.StringVar(&logToFile, "log.file", logToFile, "if specified, will log everything in JSON into a file and nothing on stderr")
	}
	setupGlobalFlags(serveFlags)
	setupGlobalFlags(sharekeyFlags)
	setupGlobalFlags(genkeyFlags)
	genkeyFlags.IntVar(&genkeyLength, "length", genkeyLength, "The length (in bits) of the key generated.")
	genkeyFlags.StringVar(&genkeyType, "type", genkeyType, "Type of the private key generated, one of : Ed25519, ECDSA, Secp256k1, RSA")
	serveFlags.String("config", "", "config file (optional)")
	serveFlags.StringVar(&serveAnnounce, "announce", serveAnnounce, "addrs that will be announce by this server")
	serveFlags.StringVar(&serveListeners, "l", serveListeners, "lists of listeners of (m)addrs separate by a comma")
	serveFlags.StringVar(&serveMetricsListeners, "metrics", serveMetricsListeners, "metrics listener, if empty will disable metrics")
	serveFlags.StringVar(&servePK, "pk", servePK, "private key (generated by `rdvp genkey`)")
	serveFlags.StringVar(&serveURN, "db", serveURN, "rdvp sqlite URN")
	serveFlags.StringVar(&emitterAdminKey, "emitter-admin-key", emitterAdminKey, "admin key of the emitter-io server")
	serveFlags.StringVar(&emitterServer, "emitter-server", emitterServer, "address of the emitter-io server, ie. tcp://127.0.0.1:8080")
	serveFlags.StringVar(&emitterPublicAddr, "emitter-public-addr", emitterPublicAddr, "if set, will be used to tell the client where to find emitter server")
	sharekeyFlags.StringVar(&sharekeyPK, "pk", sharekeyPK, "private key (generated by `rdvp genkey`)")

	serve := &ffcli.Command{
		Name:       "serve",
		ShortUsage: "rdvp [global flags] serve [flags]",
		LongHelp:   "EXAMPLE\n  rdvp genkey > rdvp.key\n  rdvp serve -pk `cat rdvp.key` -db ./rdvp-store",
		FlagSet:    serveFlags,
		Options: []ff.Option{
			ff.WithEnvVarPrefix("RDVP"),
			ff.WithConfigFileFlag("config"),
			ff.WithConfigFileParser(ff.PlainParser),
		},
		Exec: func(ctx context.Context, args []string) error {
			if len(args) > 0 {
				return flag.ErrHelp
			}

			mrand.Seed(srand.MustSecure())
			logger, cleanup, err := logutil.NewLogger(logutil.NewStdStream(logFilters, logFormat, logToFile))
			if err != nil {
				return errcode.TODO.Wrap(err)
			}
			defer cleanup()

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			var gServe run.Group
			gServe.Add(func() error {
				<-ctx.Done()
				return ctx.Err()
			}, func(error) {
				cancel()
			})

			laddrs := strings.Split(serveListeners, ",")
			listeners, err := ipfsutil.ParseAddrs(laddrs...)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			// load existing or generate new identity
			var priv libp2p_ci.PrivKey
			if servePK != "" {
				kbytes, err := base64.StdEncoding.DecodeString(servePK)
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
				priv, err = libp2p_ci.UnmarshalPrivateKey(kbytes)
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
			} else {
				// Don't use key params here, this is a dev tool, a real installation should use a static key.
				priv, _, err = libp2p_ci.GenerateKeyPairWithReader(libp2p_ci.Ed25519, -1, crand.Reader) // nolint:staticcheck
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
			}

			var addrsFactory config.AddrsFactory = func(ms []ma.Multiaddr) []ma.Multiaddr { return ms }
			if serveAnnounce != "" {
				aaddrs := strings.Split(serveAnnounce, ",")
				announces, err := ipfsutil.ParseAddrs(aaddrs...)
				if err != nil {
					return errcode.TODO.Wrap(err)
				}

				addrsFactory = func([]ma.Multiaddr) []ma.Multiaddr { return announces }
			}

			reporter := metrics.NewBandwidthCounter()

			// init p2p host
			host, err := libp2p.New(
				// default tpt + quic
				libp2p.DefaultTransports,

				// Nat & Relay service

				// @NOTE(gfanton): init relay manually
				libp2p.DisableRelay(),

				// swarm listeners
				libp2p.ListenAddrs(listeners...),

				// identity
				libp2p.Identity(priv),

				// announce
				libp2p.AddrsFactory(addrsFactory),

				// metrics
				libp2p.BandwidthReporter(reporter),
			)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			defer host.Close()
			logHostInfo(logger, host)

			_, err = libp2p_relayv2.New(host,
				// disable limits for now to have an equivalent of a relay v1
				libp2p_relayv2.WithInfiniteLimits(),
				libp2p_relayv2.WithResources(libp2p_relayv2.DefaultResources()),
			)
			if err != nil {
				return fmt.Errorf("unable to start relay v2; %w", err)
			}

			db, err := libp2p_rpdb.OpenDB(ctx, serveURN)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			defer db.Close()

			var syncDrivers []libp2p_rp.RendezvousSync

			if emitterServer != "" && emitterAdminKey != "" {
				emitter, err := rendezvous.NewEmitterServer(emitterServer, emitterAdminKey, &rendezvous.EmitterOptions{
					Logger:           logger.Named("emitter"),
					ServerPublicAddr: emitterPublicAddr,
				})
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
				defer emitter.Close()

				logger.Info("connected to mqtt broker", zap.String("broker", emitterServer))
				syncDrivers = append(syncDrivers, emitter)
			}

			// start service
			_ = libp2p_rp.NewRendezvousService(host, db, syncDrivers...)

			if serveMetricsListeners != "" {
				ml, err := net.Listen("tcp", serveMetricsListeners)
				if err != nil {
					return errcode.TODO.Wrap(err)
				}

				registry := prometheus.NewRegistry()
				registry.MustRegister(collectors.NewBuildInfoCollector())
				registry.MustRegister(collectors.NewGoCollector())
				registry.MustRegister(ipfsutil.NewHostCollector(host))
				registry.MustRegister(ipfsutil.NewBandwidthCollector(reporter))
				// @TODO(gfanton): add rdvp specific collector...

				handerfor := promhttp.HandlerFor(
					registry,
					promhttp.HandlerOpts{Registry: registry},
				)

				mux := http.NewServeMux()
				gServe.Add(func() error {
					mux.Handle("/metrics", handerfor)
					logger.Info("metrics listener",
						zap.String("handler", "/metrics"),
						zap.String("listener", ml.Addr().String()))

					server := &http.Server{
						Handler:           mux,
						ReadHeaderTimeout: 3 * time.Second,
					}

					return server.Serve(ml)
				}, func(error) {
					ml.Close()
				})
			}

			if err = gServe.Run(); err != nil {
				return errcode.TODO.Wrap(err)
			}
			return nil
		},
	}

	sharekey := &ffcli.Command{
		Name:       "sharekey",
		ShortUsage: "rdvp [global flags] sharekey -pk PK",
		FlagSet:    sharekeyFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) > 0 {
				return flag.ErrHelp
			}

			if sharekeyPK == "" {
				return flag.ErrHelp
			}

			kbytes, err := base64.StdEncoding.DecodeString(sharekeyPK)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}
			priv, err := libp2p_ci.UnmarshalPrivateKey(kbytes)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			// init p2p host
			host, err := libp2p.New(libp2p.Identity(priv))
			if err != nil {
				return errcode.TODO.Wrap(err)
			}
			defer host.Close()
			fmt.Println(host.ID().String())
			return nil
		},
	}

	genkey := &ffcli.Command{
		Name:    "genkey",
		FlagSet: genkeyFlags,
		Exec: func(context.Context, []string) error {
			keyType, ok := keyNameToKeyType[strings.ToLower(genkeyType)]
			if !ok {
				return fmt.Errorf("unknown key type : '%s'. Only Ed25519, ECDSA, Secp256k1, RSA supported", genkeyType)
			}
			priv, _, err := libp2p_ci.GenerateKeyPairWithReader(keyType, genkeyLength, crand.Reader) // nolint:staticcheck
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			kbytes, err := libp2p_ci.MarshalPrivateKey(priv)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			fmt.Println(base64.StdEncoding.EncodeToString(kbytes))
			return nil
		},
	}

	root := &ffcli.Command{
		ShortUsage:  "rdvp [global flags] <subcommand>",
		Options:     []ff.Option{ff.WithEnvVarPrefix("RDVP")},
		Subcommands: []*ffcli.Command{serve, genkey, sharekey},
		Exec: func(context.Context, []string) error {
			return flag.ErrHelp
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var process run.Group
	// handle close signal
	execute, interrupt := run.SignalHandler(ctx, os.Interrupt)
	process.Add(execute, interrupt)

	// add root command to process
	process.Add(func() error {
		return root.ParseAndRun(ctx, os.Args[1:])
	}, func(error) {
		cancel()
	})

	// run process
	if err := process.Run(); err != nil && err != context.Canceled {
		log.Println(err)
		return
	}
}

// Names are in lower case.
var keyNameToKeyType = map[string]int{
	"ed25519":   libp2p_ci.Ed25519,
	"ecdsa":     libp2p_ci.ECDSA,
	"secp256k1": libp2p_ci.Secp256k1,
	"rsa":       libp2p_ci.RSA,
}

// helpers

func logHostInfo(l *zap.Logger, host libp2p_host.Host) {
	// print peer addrs
	fields := []zapcore.Field{
		zap.String("host ID (local)", host.ID().String()),
	}

	addrs := host.Addrs()
	pi := libp2p_peer.AddrInfo{
		ID:    host.ID(),
		Addrs: addrs,
	}
	if maddrs, err := libp2p_peer.AddrInfoToP2pAddrs(&pi); err == nil {
		for _, maddr := range maddrs {
			fields = append(fields, zap.Stringer("maddr", maddr))
		}
	}

	l.Info("host started", fields...)
}
