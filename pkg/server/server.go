// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/spacemonkeygo/monkit/v3"
	mhttp "github.com/spacemonkeygo/monkit/v3/http"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/common/rpc/rpcpool"
	"storj.io/gateway-mt/pkg/authclient"
	"storj.io/gateway-mt/pkg/httpserver"
	"storj.io/gateway-mt/pkg/minio"
	"storj.io/gateway-mt/pkg/server/gw"
	"storj.io/gateway-mt/pkg/server/middleware"
	"storj.io/gateway-mt/pkg/trustedip"
	"storj.io/gateway/miniogw"
	"storj.io/minio/cmd"
	"storj.io/private/version"
	"storj.io/uplink"
	"storj.io/uplink/private/transport"
)

var (
	mon = monkit.Package()

	// Error is an error class S3 Gateway http server error.
	Error = errs.Class("gateway")

	minioOnce sync.Once
)

// Peer represents an S3 compatible http server.
//
// Note that Peer relies upon Minio global variables, which are protected by a Sync.Once() construct.
// Once Peer.Run() has been called, new instances of a Peer will not update any configuration used
// by Minio.
type Peer struct {
	server     *httpserver.Server
	log        *zap.Logger
	config     Config
	closeLayer func(context.Context) error
	inShutdown int32
}

// New returns new instance of an S3 compatible http server.
func New(config Config, log *zap.Logger, trustedIPs trustedip.List, corsAllowedOrigins []string,
	authClient *authclient.AuthClient, concurrentAllowed uint) (*Peer, error) {
	r := mux.NewRouter()
	r.SkipClean(true)
	r.UseEncodedPath()

	publicServices := r.PathPrefix("/-/").Subrouter()
	publicServices.HandleFunc("/version", versionInfo)

	if config.EncodeInMemory {
		r.Use(middleware.SetInMemory)
	}

	// Create object API handler

	satelliteConnectionPool := rpcpool.New(rpcpool.Options{
		Name:           "satellite",
		Capacity:       config.SatelliteConnectionpool.Capacity,
		KeyCapacity:    config.SatelliteConnectionpool.KeyCapacity,
		IdleExpiration: config.SatelliteConnectionpool.IdleExpiration,
	})

	connectionPool := rpcpool.New(rpcpool.Options{
		Name:           "default",
		Capacity:       config.ConnectionPool.Capacity,
		KeyCapacity:    config.ConnectionPool.KeyCapacity,
		IdleExpiration: config.ConnectionPool.IdleExpiration,
	})

	uplinkConfig := configureUplinkConfig(config.Client)

	layer, err := gw.NewMultiTenantLayer(miniogw.NewStorjGateway(config.S3Compatibility), satelliteConnectionPool, connectionPool, uplinkConfig, config.InsecureLogAll)
	if err != nil {
		return nil, err
	}

	if config.DomainName == "" {
		return nil, errs.New("DomainName required but not given")
	}

	dedupedDomains := deduplicateDomains(config.DomainName)

	set := func(value, envName string) {
		err = errs.Combine(err, os.Setenv(envName, value))
	}
	// TODO(sean): can we set globalDomainNames instead?
	set(strings.Join(dedupedDomains, ","), "MINIO_DOMAIN") // MINIO_DOMAIN supports comma-separated domains.
	set("off", "MINIO_BROWSER")
	set("dummy-key-to-satisfy-minio", "MINIO_ACCESS_KEY")
	set("dummy-key-to-satisfy-minio", "MINIO_SECRET_KEY")
	if err != nil {
		return nil, err
	}

	minio.RegisterAPIRouter(r, layer, dedupedDomains, concurrentAllowed, corsAllowedOrigins)

	r.Use(func(handler http.Handler) http.Handler {
		return mhttp.TraceHandler(handler, mon)
	})
	r.Use(middleware.NewMetrics("gmt"))
	r.Use(middleware.AccessKey(authClient, trustedIPs, log))
	r.Use(middleware.CollectEvent)
	for i, m := range cmd.GlobalHandlers {
		r.Use(middleware.MonitorMinioGlobalHandler(i, m))
	}

	// we deliberately don't log paths for this service because they have
	// sensitive information. Note that middleware.AccessKey is chained before
	// so we can use encrypted credentials while logging requests/responses.
	r.Use(middleware.NewLogRequests(log, config.InsecureLogAll))
	r.Use(middleware.NewLogResponses(log, config.InsecureLogAll))

	var handler http.Handler = minio.CriticalErrorHandler{Handler: minio.CorsHandler(corsAllowedOrigins)(r)}

	var tlsConfig *httpserver.TLSConfig
	if !config.InsecureDisableTLS {
		tlsConfig = &httpserver.TLSConfig{
			CertDir:                            config.CertDir,
			CertMagic:                          config.CertMagic.Enabled,
			CertMagicKeyFile:                   config.CertMagic.KeyFile,
			CertMagicDNSChallengeWithGCloudDNS: true,
			CertMagicDNSChallengeWithGCloudDNSProject: config.CertMagic.Project,
			CertMagicDNSChallengeOverrideDomain:       config.CertMagic.ChallengeOverrideDomain,
			CertMagicEmail:                            config.CertMagic.Email,
			CertMagicStaging:                          config.CertMagic.Staging,
			CertMagicBucket:                           config.CertMagic.Bucket,
			CertMagicPublicURLs:                       strings.Split(config.DomainName, ","),
		}
	}

	server, err := httpserver.New(log, handler, nil, httpserver.Config{
		Address:        config.Server.Address,
		AddressTLS:     config.Server.AddressTLS,
		TLSConfig:      tlsConfig,
		TrafficLogging: false, // gateway-mt has its own logging middleware for this
	})
	if err != nil {
		return nil, err
	}

	peer := Peer{
		log:        log,
		server:     server,
		config:     config,
		closeLayer: layer.Shutdown,
	}
	publicServices.HandleFunc("/health", peer.healthCheck)
	return &peer, nil
}

// deduplicateDomains removes duplicate domains, as well as naively strips any
// wildcard prefix. e.g. "gateway.local,*.gateway.local" would return just
// "gateway.local". These are used by minio.RegisterAPIRouter().
func deduplicateDomains(domains string) (result []string) {
	dedupedDomains := make(map[string]struct{})
	for _, domain := range strings.Split(domains, ",") {
		dedupedDomains[strings.TrimPrefix(domain, "*.")] = struct{}{}
	}
	for domain := range dedupedDomains {
		result = append(result, domain)
	}
	return result
}

// configureUplinkConfig configures new uplink.Config using clientConfig.
func configureUplinkConfig(clientConfig ClientConfig) gw.UplinkConfig {
	ret := gw.UplinkConfig{
		Base: uplink.Config{
			DialTimeout: clientConfig.DialTimeout,
		},
		Uploads: gw.UploadConfig{
			PieceHashAlgorithmBlake3: clientConfig.Upload.PieceHashAlgorithmBlake3,
			RefactoredCodePath:       clientConfig.Upload.RefactoredCodePath,
		},
	}

	transport.SetMaximumBufferSize(&ret.Base, clientConfig.MaximumBufferSize.Int())

	return ret
}

func (s *Peer) healthCheck(w http.ResponseWriter, r *http.Request) {
	// TODO: should this function do any tests to confirm the server is operational before returning a 200?
	// this function should be low-effort, in the sense that the load balancer is going to be hitting it regularly.
	if atomic.LoadInt32(&s.inShutdown) != 0 {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func versionInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, version.Build.Version.String())
}

// Run starts the S3 compatible http server.
func (s *Peer) Run(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	// Minio, Gateway, and the LogTarget are global, so additionally ensure only one
	// of each are added, such may be the case if starting multiple servers in parallel.
	minioOnce.Do(func() {
		minio.StartMinio(!s.config.InsecureDisableTLS)
	})

	return s.server.Run(ctx)
}

// Close shuts down the server and all underlying resources.
func (s *Peer) Close() error {
	atomic.StoreInt32(&s.inShutdown, 1)
	if s.config.ShutdownDelay > 0 {
		s.log.Info("Waiting before server shutdown:", zap.Duration("Delay", s.config.ShutdownDelay))
		time.Sleep(s.config.ShutdownDelay)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// note: httpserver.Shutdown has its own configured timeout
	return Error.Wrap(errs.Combine(s.closeLayer(ctx), s.server.Shutdown()))
}

// Address returns the web address the peer is listening on.
func (s *Peer) Address() string {
	return s.server.Addr()
}

// AddressTLS returns the TLS web address the peer is listening on.
func (s *Peer) AddressTLS() string {
	return s.server.AddrTLS()
}
