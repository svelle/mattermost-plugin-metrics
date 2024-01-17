package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/alecthomas/units"
	"github.com/go-kit/log"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/scrape"
	"github.com/prometheus/prometheus/tsdb"

	root "github.com/mattermost/mattermost-plugin-metrics"

	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/mattermost/mattermost/server/public/pluginapi/cluster"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
)

// Plugin implements the interface expected by the Mattermost server to communicate between the server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	client *pluginapi.Client

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// tsdbLock using mutual access to perform actions on tsdb.
	tsdbLock sync.RWMutex

	// singletonLock using mutually exclusive lock to run a single instance of the plugin
	singletonLock         *cluster.Mutex
	singletonLockAcquired bool

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration

	// the local tsdb to be used for head block
	db *tsdb.DB

	// filestore is being used long storage of the immutable blocks
	fileBackend filestore.FileBackend

	closeChan chan bool
	waitGroup sync.WaitGroup

	logger log.Logger

	handler *handler
}

func (p *Plugin) OnActivate() error {
	p.client = pluginapi.NewClient(p.API, p.Driver)
	p.logger = &metricsLogger{api: p.API}

	p.handler = newHandler(p)

	appCfg := p.API.GetConfig()

	p.closeChan = make(chan bool)
	p.waitGroup = sync.WaitGroup{}

	// we are using a mutually exclusive lock to run a single instance of this plugin
	// we don't really need to collect metrics twice: although TSDB will take care
	// of overlapped blocks, it will increase the disk writes to the remote or local
	// disk.
	if p.isHA() {
		var err error
		p.singletonLock, err = cluster.NewMutex(p.API, root.Manifest.Id)
		if err != nil {
			return err
		}

		// the constant '20' is determined by healthcheck of the plugin which is 30 seconds.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err = p.singletonLock.LockWithContext(ctx)
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			p.API.LogDebug("Another instance of the plugin is running in another node with scraping mode. Skipping this one.")
			return nil
		} else if err != nil {
			return err
		}
		p.singletonLockAcquired = true
	}

	backend, err := filestore.NewFileBackend(filestore.NewFileBackendSettingsFromConfig(&appCfg.FileSettings, false, false))
	if err != nil {
		return fmt.Errorf("failed to initialize filebackend: %w", err)
	}
	p.fileBackend = backend

	p.closeChan = make(chan bool)
	p.waitGroup = sync.WaitGroup{}
	if p.configuration == nil {
		p.configuration = new(configuration)
		p.configuration.SetDefaults()
	}

	if err = p.configuration.IsValid(); err != nil {
		return fmt.Errorf("could not validate config: %w", err)
	}

	// initiate local tsdb
	p.tsdbLock.Lock()
	defer p.tsdbLock.Unlock()
	p.db, err = tsdb.Open(*p.configuration.DBPath, p.logger, nil, &tsdb.Options{
		RetentionDuration:              int64(30 * 24 * time.Hour / time.Millisecond),
		AllowOverlappingCompaction:     *p.configuration.AllowOverlappingCompaction,
		EnableMemorySnapshotOnShutdown: *p.configuration.EnableMemorySnapshotOnShutdown,
	}, nil)
	if err != nil {
		return fmt.Errorf("could not open target tsdb: %w", err)
	}

	manager := scrape.NewManager(nil, p.logger, p.db)
	syncCh := make(chan map[string][]*targetgroup.Group)

	// we start the manager first, then apply the scrape config
	p.waitGroup.Add(1)
	go func() {
		defer p.waitGroup.Done()

		p.API.LogInfo("Running scrape manager...")
		err2 := manager.Run(syncCh)
		if err2 != nil {
			p.API.LogError("scrape manager exited unexpectedly", "err", err2)
		}
	}()

	scpCfg := &config.Config{
		ScrapeConfigs: []*config.ScrapeConfig{
			{
				JobName:                    "prometheus",
				Scheme:                     "http",
				MetricsPath:                "metrics",
				ScrapeInterval:             model.Duration(time.Duration(*p.configuration.ScrapeIntervalSeconds) * time.Second),
				ScrapeTimeout:              model.Duration(time.Duration(*p.configuration.ScrapeTimeoutSeconds) * time.Second),
				BodySizeLimit:              units.Base2Bytes(*p.configuration.BodySizeLimitBytes),
				HonorLabels:                *p.configuration.HonorTimestamps,
				SampleLimit:                uint(*p.configuration.SampleLimit),
				NativeHistogramBucketLimit: uint(*p.configuration.BucketLimit),
			},
		},
	}
	manager.ApplyConfig(scpCfg)

	sync, err := generateTargetGroup(p.API.GetConfig(), nil)
	if err != nil {
		return fmt.Errorf("could not set scrape target :%w", err)
	}
	syncCh <- sync

	// check if cluster is enabled
	if p.isHA() {
		// TODO(isacikgoz): get cluster info
		// we will need to push new cluster layout to p.clusterCh by either polling the cluster table
		// or listening the cluster event messages
		p.API.LogWarn("cluster meterics is not enabled")
	}

	// this goroutine will need to be re-structurd to listen a more channels
	// once we start supporting HA, we will need to listen the cluster change channel and
	// convert the []mmodel.ClusterDiscovery entries into map[string][]*targetgroup.Group
	p.waitGroup.Add(1)
	go func() {
		defer p.waitGroup.Done()
		<-p.closeChan
		p.API.LogInfo("Stopping scrape manager...")
		manager.Stop()
	}()

	p.waitGroup.Add(1)
	go func() {
		defer p.waitGroup.Done()
		p.syncFileStore()
	}()

	return nil
}

func (p *Plugin) OnDeactivate() error {
	// the plugin mutex unlock panics if the lock was not acquired
	// so we need to check whether we actually acquired the lock
	if p.isHA() && p.singletonLockAcquired {
		defer p.singletonLock.Unlock()
	}

	p.tsdbLock.Lock()
	defer p.tsdbLock.Unlock()

	close(p.closeChan)
	p.waitGroup.Wait()

	p.API.LogInfo("Scrape manager stopped")

	if p.db != nil {
		return p.db.Close()
	}

	return nil
}

// ServeHTTP demonstrates a plugin that handles HTTP requests by greeting the world.
func (p *Plugin) ServeHTTP(_ *plugin.Context, w http.ResponseWriter, r *http.Request) {
	p.handler.ServeHTTP(w, r)
}
