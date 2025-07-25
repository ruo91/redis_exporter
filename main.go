package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	log "github.com/sirupsen/logrus"

	"github.com/oliver006/redis_exporter/exporter"
)

var (
	/*
		BuildVersion, BuildDate, BuildCommitSha are filled in by the build script
	*/
	BuildVersion   = "<<< filled in by build >>>"
	BuildDate      = "<<< filled in by build >>>"
	BuildCommitSha = "<<< filled in by build >>>"
)

func getEnv(key string, defaultVal string) string {
	if envVal, ok := os.LookupEnv(key); ok {
		return envVal
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if envVal, ok := os.LookupEnv(key); ok {
		envBool, err := strconv.ParseBool(envVal)
		if err == nil {
			return envBool
		}
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	if envVal, ok := os.LookupEnv(key); ok {
		envInt64, err := strconv.ParseInt(envVal, 10, 64)
		if err == nil {
			return envInt64
		}
	}
	return defaultVal
}

func main() {
	var (
		redisAddr                      = flag.String("redis.addr", getEnv("REDIS_ADDR", "redis://localhost:6379"), "Address of the Redis instance to scrape")
		redisUser                      = flag.String("redis.user", getEnv("REDIS_USER", ""), "User name to use for authentication (Redis ACL for Redis 6.0 and newer)")
		redisPwd                       = flag.String("redis.password", getEnv("REDIS_PASSWORD", ""), "Password of the Redis instance to scrape")
		redisPwdFile                   = flag.String("redis.password-file", getEnv("REDIS_PASSWORD_FILE", ""), "Password file of the Redis instance to scrape")
		namespace                      = flag.String("namespace", getEnv("REDIS_EXPORTER_NAMESPACE", "redis"), "Namespace for metrics")
		checkKeys                      = flag.String("check-keys", getEnv("REDIS_EXPORTER_CHECK_KEYS", ""), "Comma separated list of key-patterns to export value and length/size, searched for with SCAN")
		checkSingleKeys                = flag.String("check-single-keys", getEnv("REDIS_EXPORTER_CHECK_SINGLE_KEYS", ""), "Comma separated list of single keys to export value and length/size")
		checkKeyGroups                 = flag.String("check-key-groups", getEnv("REDIS_EXPORTER_CHECK_KEY_GROUPS", ""), "Comma separated list of lua regex for grouping keys")
		checkStreams                   = flag.String("check-streams", getEnv("REDIS_EXPORTER_CHECK_STREAMS", ""), "Comma separated list of stream-patterns to export info about streams, groups and consumers, searched for with SCAN")
		checkSingleStreams             = flag.String("check-single-streams", getEnv("REDIS_EXPORTER_CHECK_SINGLE_STREAMS", ""), "Comma separated list of single streams to export info about streams, groups and consumers")
		streamsExcludeConsumerMetrics  = flag.Bool("streams-exclude-consumer-metrics", getEnvBool("REDIS_EXPORTER_STREAMS_EXCLUDE_CONSUMER_METRICS", false), "Don't collect per consumer metrics for streams (decreases cardinality)")
		countKeys                      = flag.String("count-keys", getEnv("REDIS_EXPORTER_COUNT_KEYS", ""), "Comma separated list of patterns to count (eg: 'db0=production_*,db3=sessions:*'), searched for with SCAN")
		checkKeysBatchSize             = flag.Int64("check-keys-batch-size", getEnvInt64("REDIS_EXPORTER_CHECK_KEYS_BATCH_SIZE", 1000), "Approximate number of keys to process in each execution, larger value speeds up scanning.\nWARNING: Still Redis is a single-threaded app, huge COUNT can affect production environment.")
		scriptPath                     = flag.String("script", getEnv("REDIS_EXPORTER_SCRIPT", ""), "Comma separated list of path(s) to Redis Lua script(s) for gathering extra metrics")
		listenAddress                  = flag.String("web.listen-address", getEnv("REDIS_EXPORTER_WEB_LISTEN_ADDRESS", ":9121"), "Address to listen on for web interface and telemetry.")
		metricPath                     = flag.String("web.telemetry-path", getEnv("REDIS_EXPORTER_WEB_TELEMETRY_PATH", "/metrics"), "Path under which to expose metrics.")
		logFormat                      = flag.String("log-format", getEnv("REDIS_EXPORTER_LOG_FORMAT", "txt"), "Log format, valid options are txt and json")
		configCommand                  = flag.String("config-command", getEnv("REDIS_EXPORTER_CONFIG_COMMAND", "CONFIG"), "What to use for the CONFIG command, set to \"-\" to skip config metrics extraction")
		connectionTimeout              = flag.String("connection-timeout", getEnv("REDIS_EXPORTER_CONNECTION_TIMEOUT", "15s"), "Timeout for connection to Redis instance")
		tlsClientKeyFile               = flag.String("tls-client-key-file", getEnv("REDIS_EXPORTER_TLS_CLIENT_KEY_FILE", ""), "Name of the client key file (including full path) if the server requires TLS client authentication")
		tlsClientCertFile              = flag.String("tls-client-cert-file", getEnv("REDIS_EXPORTER_TLS_CLIENT_CERT_FILE", ""), "Name of the client certificate file (including full path) if the server requires TLS client authentication")
		tlsCaCertFile                  = flag.String("tls-ca-cert-file", getEnv("REDIS_EXPORTER_TLS_CA_CERT_FILE", ""), "Name of the CA certificate file (including full path) if the server requires TLS client authentication")
		tlsServerKeyFile               = flag.String("tls-server-key-file", getEnv("REDIS_EXPORTER_TLS_SERVER_KEY_FILE", ""), "Name of the server key file (including full path) if the web interface and telemetry should use TLS")
		tlsServerCertFile              = flag.String("tls-server-cert-file", getEnv("REDIS_EXPORTER_TLS_SERVER_CERT_FILE", ""), "Name of the server certificate file (including full path) if the web interface and telemetry should use TLS")
		tlsServerCaCertFile            = flag.String("tls-server-ca-cert-file", getEnv("REDIS_EXPORTER_TLS_SERVER_CA_CERT_FILE", ""), "Name of the CA certificate file (including full path) if the web interface and telemetry should require TLS client authentication")
		tlsServerMinVersion            = flag.String("tls-server-min-version", getEnv("REDIS_EXPORTER_TLS_SERVER_MIN_VERSION", "TLS1.2"), "Minimum TLS version that is acceptable by the web interface and telemetry when using TLS")
		maxDistinctKeyGroups           = flag.Int64("max-distinct-key-groups", getEnvInt64("REDIS_EXPORTER_MAX_DISTINCT_KEY_GROUPS", 100), "The maximum number of distinct key groups with the most memory utilization to present as distinct metrics per database, the leftover key groups will be aggregated in the 'overflow' bucket")
		isDebug                        = flag.Bool("debug", getEnvBool("REDIS_EXPORTER_DEBUG", false), "Output verbose debug information")
		setClientName                  = flag.Bool("set-client-name", getEnvBool("REDIS_EXPORTER_SET_CLIENT_NAME", true), "Whether to set client name to redis_exporter")
		isTile38                       = flag.Bool("is-tile38", getEnvBool("REDIS_EXPORTER_IS_TILE38", false), "Whether to scrape Tile38 specific metrics")
		isCluster                      = flag.Bool("is-cluster", getEnvBool("REDIS_EXPORTER_IS_CLUSTER", false), "Whether this is a redis cluster (Enable this if you need to fetch key level data on a Redis Cluster).")
		exportClientList               = flag.Bool("export-client-list", getEnvBool("REDIS_EXPORTER_EXPORT_CLIENT_LIST", false), "Whether to scrape Client List specific metrics")
		exportClientPort               = flag.Bool("export-client-port", getEnvBool("REDIS_EXPORTER_EXPORT_CLIENT_PORT", false), "Whether to include the client's port when exporting the client list. Warning: including the port increases the number of metrics generated and will make your Prometheus server take up more memory")
		showVersion                    = flag.Bool("version", false, "Show version information and exit")
		redisMetricsOnly               = flag.Bool("redis-only-metrics", getEnvBool("REDIS_EXPORTER_REDIS_ONLY_METRICS", false), "Whether to also export go runtime metrics")
		pingOnConnect                  = flag.Bool("ping-on-connect", getEnvBool("REDIS_EXPORTER_PING_ON_CONNECT", false), "Whether to ping the redis instance after connecting")
		inclConfigMetrics              = flag.Bool("include-config-metrics", getEnvBool("REDIS_EXPORTER_INCL_CONFIG_METRICS", false), "Whether to include all config settings as metrics")
		inclModulesMetrics             = flag.Bool("include-modules-metrics", getEnvBool("REDIS_EXPORTER_INCL_MODULES_METRICS", false), "Whether to collect Redis Modules metrics")
		disableExportingKeyValues      = flag.Bool("disable-exporting-key-values", getEnvBool("REDIS_EXPORTER_DISABLE_EXPORTING_KEY_VALUES", false), "Whether to disable values of keys stored in redis as labels or not when using check-keys/check-single-key")
		excludeLatencyHistogramMetrics = flag.Bool("exclude-latency-histogram-metrics", getEnvBool("REDIS_EXPORTER_EXCLUDE_LATENCY_HISTOGRAM_METRICS", false), "Do not try to collect latency histogram metrics")
		redactConfigMetrics            = flag.Bool("redact-config-metrics", getEnvBool("REDIS_EXPORTER_REDACT_CONFIG_METRICS", true), "Whether to redact config settings that include potentially sensitive information like passwords")
		inclSystemMetrics              = flag.Bool("include-system-metrics", getEnvBool("REDIS_EXPORTER_INCL_SYSTEM_METRICS", false), "Whether to include system metrics like e.g. redis_total_system_memory_bytes")
		skipTLSVerification            = flag.Bool("skip-tls-verification", getEnvBool("REDIS_EXPORTER_SKIP_TLS_VERIFICATION", false), "Whether to to skip TLS verification")
		skipCheckKeysForRoleMaster     = flag.Bool("skip-checkkeys-for-role-master", getEnvBool("REDIS_EXPORTER_SKIP_CHECKKEYS_FOR_ROLE_MASTER", false), "Whether to skip gathering the check-keys metrics (size, val) when the instance is of type master (reduce load on master nodes)")
		basicAuthUsername              = flag.String("basic-auth-username", getEnv("REDIS_EXPORTER_BASIC_AUTH_USERNAME", ""), "Username for basic authentication")
		basicAuthPassword              = flag.String("basic-auth-password", getEnv("REDIS_EXPORTER_BASIC_AUTH_PASSWORD", ""), "Password for basic authentication")
		inclMetricsForEmptyDatabases   = flag.Bool("include-metrics-for-empty-databases", getEnvBool("REDIS_EXPORTER_INCL_METRICS_FOR_EMPTY_DATABASES", true), "Whether to emit db metrics (like db_keys) for empty databases")
	)
	flag.Parse()

	switch *logFormat {
	case "json":
		log.SetFormatter(&log.JSONFormatter{})
	default:
		log.SetFormatter(&log.TextFormatter{})
	}
	if *showVersion {
		log.SetOutput(os.Stdout)
	}
	log.Printf("Redis Metrics Exporter %s    build date: %s    sha1: %s    Go: %s    GOOS: %s    GOARCH: %s",
		BuildVersion, BuildDate, BuildCommitSha,
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
	)
	if *showVersion {
		return
	}
	if *isDebug {
		log.SetLevel(log.DebugLevel)
		log.Debugln("Enabling debug output")
	} else {
		log.SetLevel(log.InfoLevel)
	}

	to, err := time.ParseDuration(*connectionTimeout)
	if err != nil {
		log.Fatalf("Couldn't parse connection timeout duration, err: %s", err)
	}

	passwordMap := make(map[string]string)
	if *redisPwd == "" && *redisPwdFile != "" {
		passwordMap, err = exporter.LoadPwdFile(*redisPwdFile)
		if err != nil {
			log.Fatalf("Error loading redis passwords from file %s, err: %s", *redisPwdFile, err)
		}
	}

	var ls map[string][]byte
	if *scriptPath != "" {
		scripts := strings.Split(*scriptPath, ",")
		ls = make(map[string][]byte, len(scripts))
		for _, script := range scripts {
			if ls[script], err = os.ReadFile(script); err != nil {
				log.Fatalf("Error loading script file %s    err: %s", script, err)
			}
		}
	}

	registry := prometheus.NewRegistry()
	if !*redisMetricsOnly {
		registry.MustRegister(
			// expose process metrics like CPU, Memory, file descriptor usage etc.
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
			// expose all Go runtime metrics like GC stats, memory stats etc.
			collectors.NewGoCollector(collectors.WithGoCollectorRuntimeMetrics(collectors.MetricsAll)),
		)
	}

	exp, err := exporter.NewRedisExporter(
		*redisAddr,
		exporter.Options{
			User:                           *redisUser,
			Password:                       *redisPwd,
			PasswordMap:                    passwordMap,
			Namespace:                      *namespace,
			ConfigCommandName:              *configCommand,
			CheckKeys:                      *checkKeys,
			CheckSingleKeys:                *checkSingleKeys,
			CheckKeysBatchSize:             *checkKeysBatchSize,
			CheckKeyGroups:                 *checkKeyGroups,
			MaxDistinctKeyGroups:           *maxDistinctKeyGroups,
			CheckStreams:                   *checkStreams,
			CheckSingleStreams:             *checkSingleStreams,
			StreamsExcludeConsumerMetrics:  *streamsExcludeConsumerMetrics,
			CountKeys:                      *countKeys,
			LuaScript:                      ls,
			InclSystemMetrics:              *inclSystemMetrics,
			InclConfigMetrics:              *inclConfigMetrics,
			DisableExportingKeyValues:      *disableExportingKeyValues,
			ExcludeLatencyHistogramMetrics: *excludeLatencyHistogramMetrics,
			RedactConfigMetrics:            *redactConfigMetrics,
			SetClientName:                  *setClientName,
			IsTile38:                       *isTile38,
			IsCluster:                      *isCluster,
			InclModulesMetrics:             *inclModulesMetrics,
			ExportClientList:               *exportClientList,
			ExportClientsInclPort:          *exportClientPort,
			SkipCheckKeysForRoleMaster:     *skipCheckKeysForRoleMaster,
			SkipTLSVerification:            *skipTLSVerification,
			ClientCertFile:                 *tlsClientCertFile,
			ClientKeyFile:                  *tlsClientKeyFile,
			CaCertFile:                     *tlsCaCertFile,
			ConnectionTimeouts:             to,
			MetricsPath:                    *metricPath,
			RedisMetricsOnly:               *redisMetricsOnly,
			PingOnConnect:                  *pingOnConnect,
			RedisPwdFile:                   *redisPwdFile,
			Registry:                       registry,
			BuildInfo: exporter.BuildInfo{
				Version:   BuildVersion,
				CommitSha: BuildCommitSha,
				Date:      BuildDate,
			},
			BasicAuthUsername:            *basicAuthUsername,
			BasicAuthPassword:            *basicAuthPassword,
			InclMetricsForEmptyDatabases: *inclMetricsForEmptyDatabases,
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	// Verify that initial client keypair and CA are accepted
	if (*tlsClientCertFile != "") != (*tlsClientKeyFile != "") {
		log.Fatal("TLS client key file and cert file should both be present")
	}
	_, err = exp.CreateClientTLSConfig()
	if err != nil {
		log.Fatal(err)
	}

	log.Infof("Providing metrics at %s%s", *listenAddress, *metricPath)
	log.Debugf("Configured redis addr: %#v", *redisAddr)
	server := &http.Server{
		Addr:    *listenAddress,
		Handler: exp,
	}
	go func() {
		if *tlsServerCertFile != "" && *tlsServerKeyFile != "" {
			log.Debugf("Bind as TLS using cert %s and key %s", *tlsServerCertFile, *tlsServerKeyFile)

			tlsConfig, err := exp.CreateServerTLSConfig(*tlsServerCertFile, *tlsServerKeyFile, *tlsServerCaCertFile, *tlsServerMinVersion)
			if err != nil {
				log.Fatal(err)
			}
			server.TLSConfig = tlsConfig
			if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("TLS Server error: %v", err)
			}
		} else {
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Server error: %v", err)
			}
		}
	}()

	// graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	_quit := <-quit
	log.Infof("Received %s signal, exiting", _quit.String())
	// Create a context with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown the HTTP server gracefully
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}
	log.Infof("Server shut down gracefully")
}
