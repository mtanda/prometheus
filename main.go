// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	_ "net/http/pprof" // Comment this line to disable pprof endpoint.
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"

	clientmodel "github.com/prometheus/client_golang/model"
	registry "github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/notification"
	"github.com/prometheus/prometheus/retrieval"
	"github.com/prometheus/prometheus/rules/manager"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/local"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/storage/remote/influxdb"
	"github.com/prometheus/prometheus/storage/remote/opentsdb"
	"github.com/prometheus/prometheus/web"
	"github.com/prometheus/prometheus/web/api"
)

const deletionBatchSize = 100

// Commandline flags.
var (
	configFile = flag.String("config.file", "prometheus.conf", "Prometheus configuration file name.")

	alertmanagerURL           = flag.String("alertmanager.url", "", "The URL of the alert manager to send notifications to.")
	notificationQueueCapacity = flag.Int("alertmanager.notification-queue-capacity", 100, "The capacity of the queue for pending alert manager notifications.")

	persistenceStoragePath = flag.String("storage.local.path", "/tmp/metrics", "Base path for metrics storage.")

	remoteStorageType    = flag.String("storage.remote.type", "opentsdb", "The type of remote storage to use. Valid values: 'opentsdb', 'influxdb'.")
	remoteStorageURL     = flag.String("storage.remote.url", "", "The URL of the remote storage instance to send samples to.")
	remoteStorageTimeout = flag.Duration("storage.remote.timeout", 30*time.Second, "The timeout to use when sending samples to the remote storage.")

	numMemoryChunks = flag.Int("storage.local.memory-chunks", 1024*1024, "How many chunks to keep in memory. While the size of a chunk is 1kiB, the total memory usage will be significantly higher than this value * 1kiB. Furthermore, for various reasons, more chunks might have to be kept in memory temporarily.")

	persistenceRetentionPeriod = flag.Duration("storage.local.retention", 15*24*time.Hour, "How long to retain samples in the local storage.")
	maxChunksToPersist         = flag.Int("storage.local.max-chunks-to-persist", 1024*1024, "How many chunks can be waiting for persistence before sample ingestion will stop. Many chunks waiting to be persisted will increase the checkpoint size.")

	checkpointInterval         = flag.Duration("storage.local.checkpoint-interval", 5*time.Minute, "The period at which the in-memory metrics and the chunks not yet persisted to series files are checkpointed.")
	checkpointDirtySeriesLimit = flag.Int("storage.local.checkpoint-dirty-series-limit", 5000, "If approx. that many time series are in a state that would require a recovery operation after a crash, a checkpoint is triggered, even if the checkpoint interval hasn't passed yet. A recovery operation requires a disk seek. The default limit intends to keep the recovery time below 1min even on spinning disks. With SSD, recovery is much faster, so you might want to increase this value in that case to avoid overly frequent checkpoints.")
	seriesSyncStrategy         = flag.String("storage.local.series-sync-strategy", "adaptive", "When to sync series files after modification. Possible values: 'never', 'always', 'adaptive'. Sync'ing slows down storage performance but reduces the risk of data loss in case of an OS crash. With the 'adaptive' strategy, series files are sync'd for as long as the storage is not too much behind on chunk persistence.")

	storageDirty          = flag.Bool("storage.local.dirty", false, "If set, the local storage layer will perform crash recovery even if the last shutdown appears to be clean.")
	storagePedanticChecks = flag.Bool("storage.local.pedantic-checks", false, "If set, a crash recovery will perform checks on each series file. This might take a very long time.")

	printVersion = flag.Bool("version", false, "Print version information.")
)

type prometheus struct {
	ruleManager         manager.RuleManager
	targetManager       retrieval.TargetManager
	notificationHandler *notification.NotificationHandler
	storage             local.Storage
	remoteStorageQueue  *remote.StorageQueueManager

	webService *web.WebService

	closeOnce sync.Once
}

// NewPrometheus creates a new prometheus object based on flag values.
// Call Serve() to start serving and Close() for clean shutdown.
func NewPrometheus() *prometheus {
	conf, err := config.LoadFromFile(*configFile)
	if err != nil {
		glog.Fatalf("Error loading configuration from %s: %v", *configFile, err)
	}

	notificationHandler := notification.NewNotificationHandler(*alertmanagerURL, *notificationQueueCapacity)

	var syncStrategy local.SyncStrategy
	switch *seriesSyncStrategy {
	case "never":
		syncStrategy = local.Never
	case "always":
		syncStrategy = local.Always
	case "adaptive":
		syncStrategy = local.Adaptive
	default:
		glog.Fatalf("Invalid flag value for 'storage.local.series-sync-strategy': %s", *seriesSyncStrategy)
	}

	o := &local.MemorySeriesStorageOptions{
		MemoryChunks:               *numMemoryChunks,
		MaxChunksToPersist:         *maxChunksToPersist,
		PersistenceStoragePath:     *persistenceStoragePath,
		PersistenceRetentionPeriod: *persistenceRetentionPeriod,
		CheckpointInterval:         *checkpointInterval,
		CheckpointDirtySeriesLimit: *checkpointDirtySeriesLimit,
		Dirty:          *storageDirty,
		PedanticChecks: *storagePedanticChecks,
		SyncStrategy:   syncStrategy,
	}
	memStorage, err := local.NewMemorySeriesStorage(o)
	if err != nil {
		glog.Fatal("Error opening memory series storage: ", err)
	}

	var sampleAppender storage.SampleAppender
	var remoteStorageQueue *remote.StorageQueueManager
	if *remoteStorageURL == "" {
		glog.Warningf("No remote storage URL provided; not sending any samples to long-term storage")
		sampleAppender = memStorage
	} else {
		var c remote.StorageClient
		switch *remoteStorageType {
		case "opentsdb":
			c = opentsdb.NewClient(*remoteStorageURL, *remoteStorageTimeout)
		case "influxdb":
			c = influxdb.NewClient(*remoteStorageURL, *remoteStorageTimeout)
		default:
			glog.Fatalf("Invalid flag value for 'storage.remote.type': %s", *remoteStorageType)
		}
		remoteStorageQueue = remote.NewStorageQueueManager(c, 100*1024)
		sampleAppender = storage.Tee{
			Appender1: remoteStorageQueue,
			Appender2: memStorage,
		}
	}

	targetManager := retrieval.NewTargetManager(sampleAppender, conf.GlobalLabels())
	targetManager.AddTargetsFromConfig(conf)

	ruleManager := manager.NewRuleManager(&manager.RuleManagerOptions{
		SampleAppender:      sampleAppender,
		NotificationHandler: notificationHandler,
		EvaluationInterval:  conf.EvaluationInterval(),
		Storage:             memStorage,
		PrometheusURL:       web.MustBuildServerURL(),
	})
	if err := ruleManager.AddRulesFromConfig(conf); err != nil {
		glog.Fatal("Error loading rule files: ", err)
	}

	flags := map[string]string{}
	flag.VisitAll(func(f *flag.Flag) {
		flags[f.Name] = f.Value.String()
	})
	prometheusStatus := &web.PrometheusStatusHandler{
		BuildInfo:   BuildInfo,
		Config:      conf.String(),
		RuleManager: ruleManager,
		TargetPools: targetManager.Pools(),
		Flags:       flags,
		Birth:       time.Now(),
	}

	alertsHandler := &web.AlertsHandler{
		RuleManager: ruleManager,
	}

	consolesHandler := &web.ConsolesHandler{
		Storage: memStorage,
	}

	metricsService := &api.MetricsService{
		Now:     clientmodel.Now,
		Storage: memStorage,
	}

	webService := &web.WebService{
		StatusHandler:   prometheusStatus,
		MetricsHandler:  metricsService,
		ConsolesHandler: consolesHandler,
		AlertsHandler:   alertsHandler,
	}

	p := &prometheus{
		ruleManager:         ruleManager,
		targetManager:       targetManager,
		notificationHandler: notificationHandler,
		storage:             memStorage,
		remoteStorageQueue:  remoteStorageQueue,

		webService: webService,
	}
	webService.QuitChan = make(chan struct{})
	return p
}

// Serve starts the Prometheus server. It returns after the server has been shut
// down. The method installs an interrupt handler, allowing to trigger a
// shutdown by sending SIGTERM to the process.
func (p *prometheus) Serve() {
	if p.remoteStorageQueue != nil {
		go p.remoteStorageQueue.Run()
	}
	go p.ruleManager.Run()
	go p.notificationHandler.Run()

	p.storage.Start()

	go func() {
		err := p.webService.ServeForever()
		if err != nil {
			glog.Fatal(err)
		}
	}()

	notifier := make(chan os.Signal)
	signal.Notify(notifier, os.Interrupt, syscall.SIGTERM)
	select {
	case <-notifier:
		glog.Warning("Received SIGTERM, exiting gracefully...")
	case <-p.webService.QuitChan:
		glog.Warning("Received termination request via web service, exiting gracefully...")
	}

	p.targetManager.Stop()
	p.ruleManager.Stop()

	if err := p.storage.Stop(); err != nil {
		glog.Error("Error stopping local storage: ", err)
	}

	if p.remoteStorageQueue != nil {
		p.remoteStorageQueue.Stop()
	}

	p.notificationHandler.Stop()
	glog.Info("See you next time!")
}

// Describe implements registry.Collector.
func (p *prometheus) Describe(ch chan<- *registry.Desc) {
	p.notificationHandler.Describe(ch)
	p.storage.Describe(ch)
	if p.remoteStorageQueue != nil {
		p.remoteStorageQueue.Describe(ch)
	}
}

// Collect implements registry.Collector.
func (p *prometheus) Collect(ch chan<- registry.Metric) {
	p.notificationHandler.Collect(ch)
	p.storage.Collect(ch)
	if p.remoteStorageQueue != nil {
		p.remoteStorageQueue.Collect(ch)
	}
}

func main() {
	flag.Parse()
	versionInfoTmpl.Execute(os.Stdout, BuildInfo)

	if *printVersion {
		os.Exit(0)
	}

	p := NewPrometheus()
	registry.MustRegister(p)
	p.Serve()
}
