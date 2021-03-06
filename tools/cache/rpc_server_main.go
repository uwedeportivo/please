package main

import (
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strings"
	"time"

	"github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/op/go-logging.v1"

	"cli"
	"tools/cache/cluster"
	"tools/cache/server"
)

var log = logging.MustGetLogger("rpc_cache_server")

var opts struct {
	Usage       string `usage:"rpc_cache_server is a server for Please's remote RPC cache.\n\nSee https://please.build/cache.html for more information."`
	Port        int    `short:"p" long:"port" description:"Port to serve on" default:"7677"`
	HTTPPort    int    `long:"http_port" description:"Port to serve HTTP on (for profiling, metrics etc)"`
	MetricsPort int    `long:"metrics_port" description:"Port to serve Prometheus metrics on"`
	Dir         string `short:"d" long:"dir" description:"Directory to write into" default:"plz-rpc-cache"`
	Verbosity   int    `short:"v" long:"verbosity" description:"Verbosity of output (higher number = more output, default 2 -> notice, warnings and errors only)" default:"2"`
	LogFile     string `long:"log_file" description:"File to log to (in addition to stdout)"`

	CleanFlags struct {
		LowWaterMark   cli.ByteSize `short:"l" long:"low_water_mark" description:"Size of cache to clean down to" default:"18G"`
		HighWaterMark  cli.ByteSize `short:"i" long:"high_water_mark" description:"Max size of cache to clean at" default:"20G"`
		CleanFrequency cli.Duration `short:"f" long:"clean_frequency" description:"Frequency to clean cache at" default:"10m"`
		MaxArtifactAge cli.Duration `short:"m" long:"max_artifact_age" description:"Clean any artifact that's not been read in this long" default:"720h"`
	} `group:"Options controlling when to clean the cache"`

	TLSFlags struct {
		KeyFile       string `long:"key_file" description:"File containing PEM-encoded private key."`
		CertFile      string `long:"cert_file" description:"File containing PEM-encoded certificate"`
		CACertFile    string `long:"ca_cert_file" description:"File containing PEM-encoded CA certificate"`
		WritableCerts string `long:"writable_certs" description:"File or directory containing certificates that are allowed to write to the cache"`
		ReadonlyCerts string `long:"readonly_certs" description:"File or directory containing certificates that are allowed to read from the cache"`
	} `group:"Options controlling TLS communication & authentication"`

	ClusterFlags struct {
		ClusterPort      int    `long:"cluster_port" default:"7946" description:"Port to gossip among cluster nodes on"`
		ClusterAddresses string `short:"c" long:"cluster_addresses" description:"Comma-separated addresses of one or more nodes to join a cluster"`
		SeedCluster      bool   `long:"seed_cluster" description:"Seeds a new cache cluster."`
		ClusterSize      int    `long:"cluster_size" description:"Number of nodes to expect in the cluster.\nMust be passed if --seed_cluster is, has no effect otherwise."`
		NodeName         string `long:"node_name" env:"NODE_NAME" description:"Name of this node in the cluster. Only usually needs to be passed if running multiple nodes on the same machine, when it should be unique."`
		SeedIf           string `long:"seed_if" description:"Makes us the seed (overriding seed_cluster) if node_name matches this value and we can't resolve any cluster addresses. This makes it a lot easier to set up in automated deployments like Kubernetes."`
		AdvertiseAddr    string `long:"advertise_addr" env:"NODE_IP" description:"IP address to advertise to other cluster nodes"`
	} `group:"Options controlling clustering behaviour"`
}

func main() {
	cli.ParseFlagsOrDie("Please RPC cache server", "5.5.0", &opts)
	cli.InitLogging(opts.Verbosity)
	if opts.LogFile != "" {
		cli.InitFileLogging(opts.LogFile, opts.Verbosity)
	}
	if (opts.TLSFlags.KeyFile == "") != (opts.TLSFlags.CertFile == "") {
		log.Fatalf("Must pass both --key_file and --cert_file if you pass one")
	} else if opts.TLSFlags.KeyFile == "" && (opts.TLSFlags.WritableCerts != "" || opts.TLSFlags.ReadonlyCerts != "") {
		log.Fatalf("You can only use --writable_certs / --readonly_certs with https (--key_file and --cert_file)")
	}

	log.Notice("Scanning existing cache directory %s...", opts.Dir)
	cache := server.NewCache(opts.Dir, time.Duration(opts.CleanFlags.CleanFrequency),
		time.Duration(opts.CleanFlags.MaxArtifactAge),
		uint64(opts.CleanFlags.LowWaterMark), uint64(opts.CleanFlags.HighWaterMark))

	var clusta *cluster.Cluster
	if opts.ClusterFlags.SeedIf != "" && opts.ClusterFlags.SeedIf == opts.ClusterFlags.NodeName {
		ips, err := net.LookupIP(opts.ClusterFlags.ClusterAddresses)
		opts.ClusterFlags.SeedCluster = err != nil || len(ips) == 0
	}
	if opts.ClusterFlags.SeedCluster {
		if opts.ClusterFlags.ClusterSize < 2 {
			log.Fatalf("You must pass a cluster size of > 1 when initialising the seed node.")
		}
		clusta = cluster.NewCluster(opts.ClusterFlags.ClusterPort, opts.Port, opts.ClusterFlags.NodeName, opts.ClusterFlags.AdvertiseAddr)
		clusta.Init(opts.ClusterFlags.ClusterSize)
	} else if opts.ClusterFlags.ClusterAddresses != "" {
		clusta = cluster.NewCluster(opts.ClusterFlags.ClusterPort, opts.Port, opts.ClusterFlags.NodeName, opts.ClusterFlags.AdvertiseAddr)
		clusta.Join(strings.Split(opts.ClusterFlags.ClusterAddresses, ","))
	}

	if opts.HTTPPort != 0 {
		http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(fmt.Sprintf("Total size: %d bytes\nNum files: %d\n", cache.TotalSize(), cache.NumFiles())))
		})
		go func() {
			port := fmt.Sprintf(":%d", opts.HTTPPort)
			if opts.TLSFlags.KeyFile != "" {
				log.Fatalf("%s\n", http.ListenAndServeTLS(port, opts.TLSFlags.CertFile, opts.TLSFlags.KeyFile, nil))
			} else {
				log.Fatalf("%s\n", http.ListenAndServe(port, nil))
			}
		}()
		log.Notice("Serving HTTP stats on port %d", opts.HTTPPort)
	}

	log.Notice("Starting up RPC cache server on port %d...", opts.Port)
	s, lis := server.BuildGrpcServer(opts.Port, cache, clusta, opts.TLSFlags.KeyFile, opts.TLSFlags.CertFile,
		opts.TLSFlags.CACertFile, opts.TLSFlags.ReadonlyCerts, opts.TLSFlags.WritableCerts)

	if opts.MetricsPort != 0 {
		grpc_prometheus.Register(s)
		grpc_prometheus.EnableHandlingTimeHistogram()
		mux := http.NewServeMux()
		mux.Handle("/metrics", prometheus.Handler())
		log.Notice("Serving Prometheus metrics on port %d /metrics", opts.MetricsPort)
		go http.ListenAndServe(fmt.Sprintf(":%d", opts.MetricsPort), mux)
	}

	server.ServeGrpcForever(s, lis)
}
