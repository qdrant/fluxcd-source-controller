package adapter

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/fluxcd/source-controller/internal/helm/registry"

	"context"

	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/fluxcd/source-controller/internal/cache"
	"github.com/fluxcd/source-controller/internal/controller"
	intdigest "github.com/fluxcd/source-controller/internal/digest"
	"helm.sh/helm/v3/pkg/getter"
	v1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kuberecorder "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"
)

const (
	controllerName = "helm-controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	getters  = getter.Providers{
		getter.Provider{
			Schemes: []string{"http", "https"},
			New:     getter.NewHTTPGetter,
		},
		getter.Provider{
			Schemes: []string{"oci"},
			New:     getter.NewOCIGetter,
		},
	}
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1beta2.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))
}

type StorageOptions struct {
	Path                    string
	ServerAdvertisedAddress string
}

type SourceAdapter struct {
	client.Client
	kuberecorder.EventRecorder
	helper.Metrics
	Context        context.Context
	StoragePath    string
	FileServerPort int
	ControllerName string
}
type ReconcilerOptions struct {
	RateLimiter ratelimiter.RateLimiter
}

func SetupSourceReconcilers(mgr ctrl.Manager, adapter SourceAdapter, opts ReconcilerOptions) error {
	storage := mustInitStorage(
		adapter.StoragePath,
		adapter.getFileServerAddress(),
		60*time.Second,
		2,
		intdigest.Canonical.String(),
	)
	cacheRecorder := cache.MustMakeMetrics()

	helmIndexCache, helmIndexCacheItemTTL := mustInitHelmCache(0, "15m", "1m")

	if err := (&controller.HelmRepositoryReconciler{
		Client:         mgr.GetClient(),
		EventRecorder:  adapter.EventRecorder,
		Metrics:        adapter.Metrics,
		Storage:        storage,
		Getters:        getters,
		ControllerName: controllerName,
		Cache:          helmIndexCache,
		TTL:            helmIndexCacheItemTTL,
		CacheRecorder:  cacheRecorder,
	}).SetupWithManagerAndOptions(mgr, controller.HelmRepositoryReconcilerOptions{
		RateLimiter: opts.RateLimiter,
	}); err != nil {
		return err
	}

	if err := (&controller.HelmChartReconciler{
		Client:                  mgr.GetClient(),
		RegistryClientGenerator: registry.ClientGenerator,
		Storage:                 storage,
		Getters:                 getters,
		EventRecorder:           adapter.EventRecorder,
		Metrics:                 adapter.Metrics,
		ControllerName:          controllerName,
		Cache:                   helmIndexCache,
		TTL:                     helmIndexCacheItemTTL,
		CacheRecorder:           cacheRecorder,
	}).SetupWithManagerAndOptions(adapter.Context, mgr, controller.HelmChartReconcilerOptions{
		RateLimiter: opts.RateLimiter,
	}); err != nil {
		return err
	}

	// Start file server for serving chart archives
	go func() {
		// Block until our controller manager is elected leader. We presume our
		// entire process will terminate if we lose leadership, so we don't need
		// to handle that.
		<-mgr.Elected()

		startFileServer(storage.BasePath, adapter.getFileServerAddress())
	}()
	return nil
}

func (a *SourceAdapter) getFileServerAddress() string {
	port := 9090
	if a.FileServerPort != 0 {
		port = a.FileServerPort
	}
	return fmt.Sprintf(":%d", port)
}

func mustInitStorage(path string, storageAdvAddr string, artifactRetentionTTL time.Duration, artifactRetentionRecords int, artifactDigestAlgo string) *controller.Storage {
	if storageAdvAddr == "" {
		storageAdvAddr = determineAdvStorageAddr(storageAdvAddr)
	}

	if artifactDigestAlgo != intdigest.Canonical.String() {
		algo, err := intdigest.AlgorithmForName(artifactDigestAlgo)
		if err != nil {
			setupLog.Error(err, "unable to configure canonical digest algorithm")
			os.Exit(1)
		}
		intdigest.Canonical = algo
	}

	storage, err := controller.NewStorage(path, storageAdvAddr, artifactRetentionTTL, artifactRetentionRecords)
	if err != nil {
		setupLog.Error(err, "unable to initialise storage")
		os.Exit(1)
	}
	return storage
}

func mustInitHelmCache(maxSize int, itemTTL, purgeInterval string) (*cache.Cache, time.Duration) {
	if maxSize <= 0 {
		setupLog.Info("caching of Helm index files is disabled")
		return nil, -1
	}

	interval, err := time.ParseDuration(purgeInterval)
	if err != nil {
		setupLog.Error(err, "unable to parse Helm index cache purge interval")
		os.Exit(1)
	}

	ttl, err := time.ParseDuration(itemTTL)
	if err != nil {
		setupLog.Error(err, "unable to parse Helm index cache item TTL")
		os.Exit(1)
	}

	return cache.New(maxSize, interval), ttl
}

func determineAdvStorageAddr(storageAddr string) string {
	host, port, err := net.SplitHostPort(storageAddr)
	if err != nil {
		setupLog.Error(err, "unable to parse storage address")
		os.Exit(1)
	}
	switch host {
	case "":
		host = "localhost"
	case "0.0.0.0":
		host = os.Getenv("HOSTNAME")
		if host == "" {
			hn, err := os.Hostname()
			if err != nil {
				setupLog.Error(err, "0.0.0.0 specified in storage addr but hostname is invalid")
				os.Exit(1)
			}
			host = hn
		}
	}
	return net.JoinHostPort(host, port)
}

func startFileServer(path string, address string) {
	setupLog.Info("starting file server")
	fs := http.FileServer(http.Dir(path))
	mux := http.NewServeMux()
	mux.Handle("/", fs)
	err := http.ListenAndServe(address, mux)
	if err != nil {
		setupLog.Error(err, "file server error")
	}
}
