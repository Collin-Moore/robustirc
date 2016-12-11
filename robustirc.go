package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"time"

	"golang.org/x/net/http2"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/robustirc/bridge/tlsutil"
	"github.com/robustirc/rafthttp"
	"github.com/robustirc/robustirc/api"
	"github.com/robustirc/robustirc/ircserver"
	"github.com/robustirc/robustirc/outputstream"
	"github.com/robustirc/robustirc/raft_store"
	"github.com/robustirc/robustirc/robusthttp"
	"github.com/robustirc/robustirc/timesafeguard"

	"github.com/armon/go-metrics"
	metrics_prometheus "github.com/armon/go-metrics/prometheus"
	"github.com/hashicorp/raft"
	"github.com/stapelberg/glog"

	_ "net/http/pprof"
)

const (
	expireSessionsInterval = 10 * time.Second
)

// XXX: when introducing a new flag, you must add it to the flag.Usage function in main().
var (
	raftDir = flag.String("raftdir",
		"/var/lib/robustirc",
		"Directory in which raft state is stored. If this directory is empty, you need to specify -join.")
	listen = flag.String("listen",
		":443",
		"[host]:port to listen on. Set to a port in the dynamic port range (49152 to 65535) and use DNS SRV records.")
	version = flag.Bool("version",
		false,
		"Print version and exit")

	singleNode = flag.Bool("singlenode",
		false,
		"Become a raft leader without any followers. Set to true if and only if starting the first node for the first time.")
	join = flag.String("join",
		"",
		"host:port of an existing raft node in the network that should be joined. Will also be loaded from -raftdir.")
	dumpCanaryState = flag.String("dump_canary_state",
		"",
		"If specified, initializes the raft node (from a snapshot), then dumps all message state to the specified file. To be used via robustirc-canary.")
	dumpHeapProfile = flag.String("dump_heap_profile",
		"",
		"If specified, a heap profile will be dumped to the specified file. Only relevant when -dump_canary_state is set.")
	canaryCompactionStart = flag.Int64("canary_compaction_start",
		0,
		"If > 0, a nanosecond precision UNIX timestamp of when the compaction was started (for deterministic results across runs).")

	network = flag.String("network_name",
		"",
		`Name of the network (e.g. "robustirc.net") to use in IRC messages. Ideally also a DNS name pointing to one or more servers.`)
	peerAddr = flag.String("peer_addr",
		"",
		`host:port of this raft node (e.g. "fastbox.robustirc.net:60667"). Must be publically reachable.`)
	tlsCertPath = flag.String("tls_cert_path",
		"",
		"Path to a .pem file containing the TLS certificate.")
	tlsKeyPath = flag.String("tls_key_path",
		"",
		"Path to a .pem file containing the TLS private key.")
	networkPassword = flag.String("network_password",
		"",
		"A secure password to protect the communication between raft nodes. Use pwgen(1) or similar. If empty, the ROBUSTIRC_NETWORK_PASSWORD environment variable is used.")

	node      *raft.Raft
	peerStore *raft.JSONPeers
	ircStore  *raft_store.LevelDBStore
	ircServer *ircserver.IRCServer

	// Version is overwritten by Makefile.
	Version = "unknown"

	isLeaderGauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Subsystem: "raft",
			Name:      "isleader",
			Help:      "1 if this node is the raft leader, 0 otherwise",
		},
		func() float64 {
			if node.State() == raft.Leader {
				return 1
			}
			return 0
		},
	)

	sessionsGauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Subsystem: "irc",
			Name:      "sessions",
			Help:      "Number of IRC sessions",
		},
		func() float64 {
			return float64(ircServer.NumSessions())
		},
	)

	sessionLimitGauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Subsystem: "irc",
			Name:      "session_limit",
			Help:      "Maximum Number of IRC sessions",
		},
		func() float64 {
			return float64(ircServer.SessionLimit())
		},
	)

	channelsGauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Subsystem: "irc",
			Name:      "channels",
			Help:      "Number of IRC channels",
		},
		func() float64 {
			return float64(ircServer.NumChannels())
		},
	)

	channelLimitGauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Subsystem: "irc",
			Name:      "channel_limit",
			Help:      "Maximum Number of IRC channels",
		},
		func() float64 {
			return float64(ircServer.ChannelLimit())
		},
	)

	appliedMessages = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "applied_messages",
			Help: "How many raft messages were applied, partitioned by message type",
		},
		[]string{"type"},
	)

	secondsInState = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seconds_in_state",
			Help: "How many seconds the node was in each raft state",
		},
		[]string{"state"},
	)
)

func init() {
	prometheus.MustRegister(isLeaderGauge)
	prometheus.MustRegister(sessionsGauge)
	prometheus.MustRegister(sessionLimitGauge)
	prometheus.MustRegister(channelsGauge)
	prometheus.MustRegister(channelLimitGauge)
	prometheus.MustRegister(appliedMessages)
	prometheus.MustRegister(secondsInState)
}

func joinMaster(addr string, peerStore *raft.JSONPeers) []string {
	type joinRequest struct {
		Addr string
	}
	var buf *bytes.Buffer
	if data, err := json.Marshal(joinRequest{*peerAddr}); err != nil {
		log.Fatal("Could not marshal join request:", err)
	} else {
		buf = bytes.NewBuffer(data)
	}

	client := robusthttp.Client(*networkPassword, true)
	req, err := http.NewRequest("POST", fmt.Sprintf("https://%s/join", addr), buf)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if res, err := client.Do(req); err != nil {
		log.Fatal("Could not send join request:", err)
	} else if res.StatusCode > 399 {
		data, _ := ioutil.ReadAll(res.Body)
		log.Fatal("Join request failed:", string(data))
	} else if res.StatusCode > 299 {
		loc := res.Header.Get("Location")
		if loc == "" {
			log.Fatal("Redirect has no Location header")
		}
		u, err := url.Parse(loc)
		if err != nil {
			log.Fatalf("Could not parse redirection %q: %v", loc, err)
		}

		return joinMaster(u.Host, peerStore)
	}

	log.Printf("Adding master %q as peer\n", addr)
	p, err := peerStore.Peers()
	if err != nil {
		log.Fatal("Could not read peers:", err)
	}
	p = raft.AddUniquePeer(p, addr)
	peerStore.SetPeers(p)
	return p
}

// XXX(1.0): delete this function as users are expected to have upgraded.
func deleteOldCompactionDatabases(tmpdir string) error {
	dir, err := os.Open(tmpdir)
	if err != nil {
		return err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(name, "permanent-compaction.sqlite3") {
			if err := os.Remove(filepath.Join(tmpdir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Copied from src/net/http/server.go
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}

func printDefault(f *flag.Flag) {
	format := "  -%s=%s: %s\n"
	if getter, ok := f.Value.(flag.Getter); ok {
		if _, ok := getter.Get().(string); ok {
			// put quotes on the value
			format = "  -%s=%q: %s\n"
		}
	}
	fmt.Fprintf(os.Stderr, format, f.Name, f.DefValue, f.Usage)
}

func main() {
	flag.Usage = func() {
		// It is unfortunate that we need to re-implement flag.PrintDefaults(),
		// but I cannot see any other way to achieve the grouping of flags.
		fmt.Fprintf(os.Stderr, "RobustIRC server (= node)\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "The following flags are REQUIRED:\n")
		printDefault(flag.Lookup("network_name"))
		printDefault(flag.Lookup("network_password"))
		printDefault(flag.Lookup("peer_addr"))
		printDefault(flag.Lookup("tls_cert_path"))
		printDefault(flag.Lookup("tls_key_path"))
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "The following flags are only relevant when bootstrapping the network (once):\n")
		printDefault(flag.Lookup("join"))
		printDefault(flag.Lookup("singlenode"))
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "The following flags are optional:\n")
		printDefault(flag.Lookup("dump_canary_state"))
		printDefault(flag.Lookup("dump_heap_profile"))
		printDefault(flag.Lookup("canary_compaction_start"))
		printDefault(flag.Lookup("listen"))
		printDefault(flag.Lookup("raftdir"))
		printDefault(flag.Lookup("tls_ca_file"))
		printDefault(flag.Lookup("version"))
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "The following flags are optional and provided by glog:\n")
		printDefault(flag.Lookup("alsologtostderr"))
		printDefault(flag.Lookup("log_backtrace_at"))
		printDefault(flag.Lookup("log_dir"))
		printDefault(flag.Lookup("log_total_bytes"))
		printDefault(flag.Lookup("logtostderr"))
		printDefault(flag.Lookup("stderrthreshold"))
		printDefault(flag.Lookup("v"))
		printDefault(flag.Lookup("vmodule"))
	}
	flag.Parse()

	// Store logs in -raftdir, unless otherwise specified.
	if flag.Lookup("log_dir").Value.String() == "" {
		flag.Set("log_dir", *raftDir)
	}

	defer glog.Flush()
	glog.MaxSize = 64 * 1024 * 1024
	glog.CopyStandardLogTo("INFO")

	log.Printf("RobustIRC %s\n", Version)
	if *version {
		return
	}

	if _, err := os.Stat(filepath.Join(*raftDir, "deletestate")); err == nil {
		if err := os.RemoveAll(*raftDir); err != nil {
			log.Fatal(err)
		}
		if err := os.Mkdir(*raftDir, 0700); err != nil {
			log.Fatal(err)
		}
		log.Printf("Deleted %q because %q existed\n", *raftDir, filepath.Join(*raftDir, "deletestate"))
	}

	if err := outputstream.DeleteOldDatabases(*raftDir); err != nil {
		log.Fatalf("Could not delete old outputstream databases: %v\n", err)
	}

	if err := deleteOldCompactionDatabases(*raftDir); err != nil {
		glog.Errorf("Could not delete old compaction databases: %v (ignoring)\n", err)
	}

	log.Printf("Initializing RobustIRC…\n")

	if *networkPassword == "" {
		*networkPassword = os.Getenv("ROBUSTIRC_NETWORK_PASSWORD")
	}
	if *networkPassword == "" {
		log.Fatalf("-network_password not set. You MUST protect your network.\n")
	}

	if *network == "" {
		log.Fatalf("-network_name not set, but required.\n")
	}

	if *peerAddr == "" {
		log.Printf("-peer_addr not set, initializing to %q. Make sure %q is a host:port string that other raft nodes can connect to!\n", *listen, *listen)
		*peerAddr = *listen
	}

	ircServer = ircserver.NewIRCServer(*raftDir, *network, time.Now())

	transport := rafthttp.NewHTTPTransport(
		*peerAddr,
		// Not deadlined, otherwise snapshot installments fail.
		robusthttp.Client(*networkPassword, false),
		nil,
		"")

	peerStore = raft.NewJSONPeers(*raftDir, transport)

	if *join == "" && !*singleNode {
		peers, err := peerStore.Peers()
		if err != nil {
			log.Fatal(err.Error())
		}
		if len(peers) == 0 {
			if !*timesafeguard.DisableTimesafeguard {
				log.Fatalf("No peers known and -join not specified. Joining the network is not safe because timesafeguard cannot be called.\n")
			}
		} else {
			if len(peers) == 1 && peers[0] == *peerAddr {
				// To prevent crashlooping too frequently in case the init system directly restarts our process.
				time.Sleep(10 * time.Second)
				log.Fatalf("Only known peer is myself (%q), implying this node was removed from the network. Please kill the process and remove the data.\n", *peerAddr)
			}
			if err := timesafeguard.SynchronizedWithNetwork(*peerAddr, peers, *networkPassword); err != nil {
				log.Fatal(err.Error())
			}
		}
	}

	var p []string

	config := raft.DefaultConfig()
	config.Logger = log.New(glog.LogBridgeFor("INFO"), "", log.Lshortfile)
	if *singleNode {
		config.EnableSingleNode = true
		config.StartAsLeader = true
	}

	// Keep 5 snapshots in *raftDir/snapshots, log to stderr.
	fss, err := raft.NewFileSnapshotStoreWithLogger(*raftDir, 5, config.Logger)
	if err != nil {
		log.Fatal(err)
	}

	// How often to check whether a snapshot should be taken. The check is
	// cheap, and the default value far too high for networks with a high
	// number of messages/s.
	// At the same time, it is important that we don’t check too early,
	// otherwise recovering from the most recent snapshot doesn’t work because
	// after recovering, a new snapshot (over the 0 committed messages) will be
	// taken immediately, effectively overwriting the result of the snapshot
	// recovery.
	config.SnapshotInterval = 300 * time.Second

	// Batch as many messages as possible into a single appendEntries RPC.
	// There is no downside to setting this too high.
	config.MaxAppendEntries = 1024

	// It could be that the heartbeat goroutine is not scheduled for a while,
	// so relax the default of 500ms.
	config.LeaderLeaseTimeout = timesafeguard.ElectionTimeout
	config.HeartbeatTimeout = timesafeguard.ElectionTimeout
	config.ElectionTimeout = timesafeguard.ElectionTimeout

	// We use prometheus, so hook up the metrics package (used by raft) to
	// prometheus as well.
	sink, err := metrics_prometheus.NewPrometheusSink()
	if err != nil {
		log.Fatal(err)
	}
	metrics.NewGlobal(metrics.DefaultConfig("raftmetrics"), sink)

	bootstrapping := *singleNode || *join != ""
	logStore, err := raft_store.NewLevelDBStore(filepath.Join(*raftDir, "raftlog"), bootstrapping)
	if err != nil {
		log.Fatal(err)
	}
	ircStore, err = raft_store.NewLevelDBStore(filepath.Join(*raftDir, "irclog"), bootstrapping)
	if err != nil {
		log.Fatal(err)
	}
	fsm := &FSM{
		store:             logStore,
		ircstore:          ircStore,
		lastSnapshotState: make(map[uint64][]byte),
	}
	logcache, err := raft.NewLogCache(config.MaxAppendEntries, logStore)
	if err != nil {
		log.Fatal(err)
	}

	node, err = raft.NewRaft(config, fsm, logcache, logStore, fss, peerStore, transport)
	if err != nil {
		log.Fatal(err)
	}

	if *dumpCanaryState != "" {
		canary(fsm, *dumpCanaryState)
		if *dumpHeapProfile != "" {
			debug.FreeOSMemory()
			f, err := os.Create(*dumpHeapProfile)
			if err != nil {
				log.Fatal(err)
			}
			defer f.Close()
			pprof.WriteHeapProfile(f)
		}
		return
	}

	go func() {
		for {
			secondsInState.WithLabelValues(node.State().String()).Inc()
			time.Sleep(1 * time.Second)
		}
	}()

	api := api.NewHTTP(
		ircServer,
		node,
		peerStore,
		ircStore,
		transport,
		*network,
		*networkPassword,
		*raftDir,
		*peerAddr,
		http.DefaultServeMux)

	srv := http.Server{Addr: *listen}
	if err := http2.ConfigureServer(&srv, nil); err != nil {
		log.Fatal(err)
	}

	// Manually create the net.TCPListener so that joinMaster() does not run
	// into connection refused errors (the master will try to contact the
	// node before acknowledging the join).
	kpr, err := tlsutil.NewKeypairReloader(*tlsCertPath, *tlsKeyPath)
	if err != nil {
		log.Fatal(err)
	}
	srv.TLSConfig.GetCertificate = kpr.GetCertificateFunc()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}

	tlsListener := tls.NewListener(tcpKeepAliveListener{ln.(*net.TCPListener)}, srv.TLSConfig)
	go srv.Serve(tlsListener)

	log.Printf("RobustIRC listening on %q. For status, see %s\n",
		*peerAddr,
		fmt.Sprintf("https://robustirc:%s@%s/", *networkPassword, *peerAddr))

	if *join != "" {
		if err := timesafeguard.SynchronizedWithMasterAndNetwork(*peerAddr, *join, *networkPassword); err != nil {
			log.Fatal(err.Error())
		}

		p = joinMaster(*join, peerStore)
		// TODO(secure): properly handle joins on the server-side where the joining node is already in the network.
	}

	if len(p) > 0 {
		node.SetPeers(p)
	}

	expireSessionsTimer := time.After(expireSessionsInterval)
	secondTicker := time.Tick(1 * time.Second)
	for {
		select {
		case <-secondTicker:
			if node.State() == raft.Shutdown {
				log.Fatal("Node removed from the network (in raft state shutdown), terminating.")
			}
		case <-expireSessionsTimer:
			expireSessionsTimer = time.After(expireSessionsInterval)

			// Race conditions (a node becoming a leader or ceasing to be the
			// leader shortly before/after this runs) are okay, since the timer
			// is triggered often enough on every node so that it will
			// eventually run on the leader.
			if node.State() != raft.Leader {
				continue
			}

			for _, msg := range ircServer.ExpireSessions() {
				if err := api.ApplyMessageWait(msg, 10*time.Second); err != nil {
					log.Printf("Apply(): %v", err)
				}
			}
		}
	}
}
