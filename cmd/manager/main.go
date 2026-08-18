/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	_ "time/tzdata" // Embed IANA timezone database for distroless containers.

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/controller"
	"github.com/attune-io/attune/internal/fleetreport"
	"github.com/attune-io/attune/internal/metrics"
	_ "github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/resize"
	"github.com/attune-io/attune/internal/transform"
	"github.com/attune-io/attune/internal/webhook"
)

var (
	// Set by -ldflags at build time.
	version  = "dev"
	commit   = "none"
	date     = "unknown"
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(attunev1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var enableWebhooks bool
	var collectorTTL time.Duration
	var prometheusQPS float64
	var prometheusBurst int
	var maxConcurrentReconciles int
	var maxWorkloadWorkers int
	var requeueJitter time.Duration
	var maxProfileSamples int
	var maxPrometheusSeries int
	var maxStatusRecommendations int
	var statusIncludeExplanations bool
	var watchNamespaces string
	var prometheusTimeout time.Duration
	var fleetReportEnabled bool
	var fleetReportNamespace string
	var fleetReportName string
	var fleetReportClusterID string
	var fleetReportInterval time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true,
		"Enable admission webhooks for defaulting and validation.")
	flag.DurationVar(&collectorTTL, "collector-ttl", 10*time.Minute,
		"How long unused Prometheus collectors stay cached before eviction.")
	flag.Float64Var(&prometheusQPS, "prometheus-qps", 10,
		"Maximum Prometheus queries per second. Increase for large clusters with many policies.")
	flag.IntVar(&prometheusBurst, "prometheus-burst", 20,
		"Maximum burst for Prometheus query throttle.")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 2,
		"Maximum number of AttunePolicy reconciles running in parallel. Increase for large clusters with many policies. Helm clusterSize presets raise this further.")
	flag.IntVar(&maxWorkloadWorkers, "max-workload-workers", 10,
		"Maximum parallel workers processing workloads within a single AttunePolicy reconcile.")
	flag.DurationVar(&requeueJitter, "requeue-jitter", 2*time.Minute,
		"Maximum extra delay added only to full cooldown RequeueAfter values. Skipped while Ready is InsufficientData or PrometheusUnavailable. Set to 0 to disable.")
	flag.IntVar(&maxProfileSamples, "max-profile-samples", 10000,
		"Maximum samples passed to recommendation BuildProfile after downsampling. Negative disables the cap.")
	flag.IntVar(&maxPrometheusSeries, "max-prometheus-series", 5000,
		"Maximum series kept from a Prometheus range query matrix. Zero uses the collector default; negative disables the cap.")
	flag.IntVar(&maxStatusRecommendations, "max-status-recommendations", 100,
		"Default cap for status.recommendations entries (full set still used for resizes). Overridable per policy.")
	flag.BoolVar(&statusIncludeExplanations, "status-include-explanations", true,
		"When true, write recommendation explanation chains to status. Overridable per policy.")
	var maxPodsInMetricsQuery int
	var maxHistoryWindow time.Duration
	var minQueryStep time.Duration
	var blockerRefreshInterval time.Duration
	flag.IntVar(&maxPodsInMetricsQuery, "max-pods-in-metrics-query", 100,
		"When a workload has more pods than this, sample this many for metrics pod=~ regexes. Negative disables sampling.")
	flag.DurationVar(&maxHistoryWindow, "max-history-window", 0,
		"Optional operator-level ceiling for metrics historyWindow (e.g. 72h for large fleets). Zero disables extra clamp.")
	flag.DurationVar(&minQueryStep, "min-query-step", 0,
		"Optional operator-level floor for metrics queryStep (e.g. 10m for large fleets). Zero disables extra clamp.")
	flag.DurationVar(&blockerRefreshInterval, "blocker-refresh-interval", 0,
		"Minimum interval between Deferred/Infeasible blocker recomputes when not resizing. Zero (default) recomputes every reconcile; set e.g. 5m for large Recommend fleets.")
	var podLabelSelector string
	flag.StringVar(&podLabelSelector, "pod-label-selector", "",
		"Optional Kubernetes label selector for operator pod keep diagnostics and future ListWatch filtering "+
			"(e.g. 'attune.io/managed=true'). Combined with dynamic selectors from active policies. "+
			"All watched pods are field-stripped; empty Spec stubs are not used.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated list of namespaces to watch. Empty means all namespaces (cluster-scoped). "+
			"Reduces informer cache memory on large clusters where policies exist in a few namespaces. "+
			"Run multiple controller instances with disjoint lists to shard by namespace.")
	flag.DurationVar(&prometheusTimeout, "prometheus-timeout", 5*time.Minute,
		"Maximum time allowed for workload processing (including Prometheus queries) during a single reconciliation cycle. "+
			"If exceeded, partial results are used and the status condition indicates the timeout.")
	flag.BoolVar(&fleetReportEnabled, "fleet-report-enabled", false,
		"When true, periodically write a versioned fleet summary ConfigMap for multi-cluster collectors.")
	flag.StringVar(&fleetReportNamespace, "fleet-report-namespace", "",
		"Namespace for the fleet report ConfigMap. Defaults to the pod namespace (POD_NAMESPACE) or attune-system.")
	flag.StringVar(&fleetReportName, "fleet-report-configmap", "attune-fleet-report",
		"Name of the fleet report ConfigMap.")
	flag.StringVar(&fleetReportClusterID, "fleet-report-cluster-id", "",
		"Optional cluster identifier written into the fleet report (e.g. prod-us-east-1).")
	flag.DurationVar(&fleetReportInterval, "fleet-report-interval", 5*time.Minute,
		"How often to refresh the fleet report ConfigMap when enabled.")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if collectorTTL < 0 {
		setupLog.Error(fmt.Errorf("got %s", collectorTTL), "collector-ttl must be non-negative")
		os.Exit(1)
	}
	if prometheusQPS <= 0 {
		setupLog.Error(fmt.Errorf("got %f", prometheusQPS), "prometheus-qps must be positive")
		os.Exit(1)
	}
	if prometheusBurst <= 0 {
		setupLog.Error(fmt.Errorf("got %d", prometheusBurst), "prometheus-burst must be positive")
		os.Exit(1)
	}
	if maxConcurrentReconciles <= 0 {
		setupLog.Error(fmt.Errorf("got %d", maxConcurrentReconciles), "max-concurrent-reconciles must be positive")
		os.Exit(1)
	}
	if maxWorkloadWorkers <= 0 {
		setupLog.Error(fmt.Errorf("got %d", maxWorkloadWorkers), "max-workload-workers must be positive")
		os.Exit(1)
	}
	if requeueJitter < 0 {
		setupLog.Error(fmt.Errorf("got %s", requeueJitter), "requeue-jitter must be non-negative")
		os.Exit(1)
	}
	if prometheusTimeout <= 0 {
		setupLog.Error(fmt.Errorf("got %s", prometheusTimeout), "prometheus-timeout must be positive")
		os.Exit(1)
	}

	mgrOpts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "attune.attune.io",
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	}

	// Namespace-scoped caching: when --watch-namespaces is set, only watch
	// the listed namespaces for namespace-scoped resources (Pods, Deployments,
	// HPAs, AttunePolicies, etc.). Cluster-scoped resources (Nodes,
	// AttuneDefaults) are always watched regardless.
	if watchNamespaces != "" {
		nsMap := make(map[string]cache.Config)
		for _, ns := range strings.Split(watchNamespaces, ",") {
			ns = strings.TrimSpace(ns)
			if ns != "" {
				nsMap[ns] = cache.Config{}
			}
		}
		if len(nsMap) > 0 {
			mgrOpts.Cache = cache.Options{
				DefaultNamespaces: nsMap,
			}
			setupLog.Info("Namespace-scoped caching enabled", "namespaces", watchNamespaces)
		}
	}

	// Strip unused fields from cached objects to reduce informer memory at scale.
	// Workload/HPA write paths use APIReader (live Get) before MergeFrom/Update
	// so strip-transformed cache entries cannot wipe template image/command.
	if mgrOpts.Cache.ByObject == nil {
		mgrOpts.Cache.ByObject = make(map[client.Object]cache.ByObject)
	}
	var staticPodSel labels.Selector
	if podLabelSelector != "" {
		var err error
		staticPodSel, err = labels.Parse(podLabelSelector)
		if err != nil {
			setupLog.Error(err, "invalid --pod-label-selector")
			os.Exit(1)
		}
		setupLog.Info("Pod label selector configured", "selector", podLabelSelector)
	}
	podFilter := transform.NewPodCacheFilter(staticPodSel)
	if staticPodSel != nil {
		// Static-only filtering is active immediately (dynamic selectors refresh later).
		podFilter.UpdateDynamic(nil)
		podFilter.SetEnabled(true)
	}
	mgrOpts.Cache.ByObject[&corev1.Pod{}] = cache.ByObject{
		// Transform applies static --pod-label-selector OR dynamic policy
		// selectors (OR semantics). ListWatch-level Label is not set here
		// because a single Label cannot express the union of policy selectors.
		Transform: podFilter.Transform,
	}
	mgrOpts.Cache.ByObject[&appsv1.Deployment{}] = cache.ByObject{Transform: transform.StripDeploymentFields}
	mgrOpts.Cache.ByObject[&appsv1.StatefulSet{}] = cache.ByObject{Transform: transform.StripStatefulSetFields}
	mgrOpts.Cache.ByObject[&appsv1.DaemonSet{}] = cache.ByObject{Transform: transform.StripDaemonSetFields}
	mgrOpts.Cache.ByObject[&appsv1.ReplicaSet{}] = cache.ByObject{Transform: transform.StripReplicaSetFields}
	mgrOpts.Cache.ByObject[&autoscalingv2.HorizontalPodAutoscaler{}] = cache.ByObject{Transform: transform.StripHPAFields}
	mgrOpts.Cache.ByObject[&batchv1.Job{}] = cache.ByObject{Transform: transform.StripJobFields}
	mgrOpts.Cache.ByObject[&batchv1.CronJob{}] = cache.ByObject{Transform: transform.StripCronJobFields}

	// When webhooks are disabled, point the webhook server at a non-existent port
	// to prevent it from listening. The webhook handler is simply never registered.
	if !enableWebhooks {
		mgrOpts.WebhookServer = webhookserver.NewServer(webhookserver.Options{Port: 0})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Create a typed clientset for the /resize subresource (not available via controller-runtime client).
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes clientset")
		os.Exit(1)
	}

	// Detect OpenShift TLS profile for outbound connections. On vanilla K8s
	// this returns 0 (use Go defaults, i.e. TLS 1.2 with modern ciphers).
	clusterTLSMinVersion := metrics.DetectOpenShiftTLSProfile(clientset, setupLog)

	// Setup the AttunePolicyReconciler with a real Prometheus metrics factory and clientset.
	reconciler := controller.NewAttunePolicyReconciler()
	reconciler.Client = mgr.GetClient()
	reconciler.APIReader = mgr.GetAPIReader()
	reconciler.PodCacheFilter = podFilter
	reconciler.Scheme = mgr.GetScheme()
	reconciler.Clientset = clientset
	reconciler.Recorder = mgr.GetEventRecorder("attune")
	if sv, err := clientset.Discovery().ServerVersion(); err != nil {
		setupLog.Error(err, "unable to detect Kubernetes version; memory limit decreases will remain clamped")
	} else {
		allow := resize.AllowsInPlaceMemoryLimitDecrease(sv.GitVersion)
		reconciler.AllowInPlaceMemoryLimitDecrease = allow
		setupLog.Info("Kubernetes version for memory limit decrease policy",
			"gitVersion", sv.GitVersion, "allowInPlaceMemoryLimitDecrease", allow)
	}
	reconciler.CollectorTTL = collectorTTL
	reconciler.MaxConcurrentReconciles = maxConcurrentReconciles
	reconciler.MaxWorkloadWorkers = maxWorkloadWorkers
	reconciler.RequeueJitter = requeueJitter
	reconciler.MaxProfileSamples = maxProfileSamples
	reconciler.MaxPrometheusSeries = maxPrometheusSeries
	reconciler.MaxStatusRecommendations = maxStatusRecommendations
	reconciler.IncludeExplanationsInStatus = &statusIncludeExplanations
	reconciler.MaxPodsInMetricsQuery = maxPodsInMetricsQuery
	reconciler.MaxHistoryWindow = maxHistoryWindow
	reconciler.MinQueryStep = minQueryStep
	reconciler.BlockerRefreshInterval = blockerRefreshInterval
	reconciler.PrometheusTimeout = prometheusTimeout
	reconciler.MetricsFactory = func(address string, opts *metrics.CollectorOptions) (metrics.MetricsCollector, error) {
		if opts == nil {
			opts = &metrics.CollectorOptions{}
		}
		if opts.TLSMinVersion == 0 && clusterTLSMinVersion != 0 {
			opts.TLSMinVersion = clusterTLSMinVersion
		}
		// Apply operator series cap when the caller did not set MaxSeries.
		if opts.MaxSeries == 0 && maxPrometheusSeries != 0 {
			opts.MaxSeries = maxPrometheusSeries
		}
		collector, err := metrics.NewPrometheusCollectorWithOptions(address, ctrl.Log.WithName("prometheus"), opts)
		if err != nil {
			return nil, fmt.Errorf("creating Prometheus collector for %s: %w", address, err)
		}
		return metrics.NewRateLimitedCollector(collector, prometheusQPS, prometheusBurst), nil
	}
	if err = reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AttunePolicy")
		os.Exit(1)
	}

	if fleetReportEnabled {
		ns := fleetReportNamespace
		if ns == "" {
			ns = os.Getenv("POD_NAMESPACE")
		}
		if ns == "" {
			ns = "attune-system"
		}
		exporter := &fleetreport.Exporter{
			Client:    mgr.GetClient(),
			Log:       setupLog.WithName("fleet-report"),
			Namespace: ns,
			Name:      fleetReportName,
			ClusterID: fleetReportClusterID,
			Interval:  fleetReportInterval,
		}
		if err := mgr.Add(exporter); err != nil {
			setupLog.Error(err, "unable to add fleet report exporter")
			os.Exit(1)
		}
		setupLog.Info("fleet report exporter enabled",
			"namespace", ns, "configMap", fleetReportName, "interval", fleetReportInterval.String())
	}

	// Register webhooks (requires cert-manager or manual TLS cert provisioning).
	if enableWebhooks {
		if err = ctrl.NewWebhookManagedBy(mgr, &attunev1alpha1.AttunePolicy{}).
			WithDefaulter(&webhook.AttunePolicyDefaulter{}).
			WithValidator(&webhook.AttunePolicyValidator{}).
			Complete(); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "AttunePolicy")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &attunev1alpha1.AttuneDefaults{}).
			WithValidator(&webhook.AttuneDefaultsValidator{}).
			Complete(); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "AttuneDefaults")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &attunev1alpha1.AttuneNamespaceDefaults{}).
			WithValidator(&webhook.AttuneNamespaceDefaultsValidator{}).
			Complete(); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "AttuneNamespaceDefaults")
			os.Exit(1)
		}

		// Pod initial sizing webhook: mutates pod resources at creation time
		// based on existing AttunePolicy recommendations.
		mgr.GetWebhookServer().Register("/mutate-v1-pod",
			&webhookserver.Admission{Handler: &webhook.PodMutatingHandler{
				Client: mgr.GetClient(),
				Logger: setupLog.WithName("pod-initial-sizing"),
			}})
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	if enableWebhooks {
		if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			setupLog.Error(err, "unable to set up webhook ready check")
			os.Exit(1)
		}
	}

	setupLog.Info("starting manager",
		"version", version, "commit", commit, "date", date,
		"webhooks", enableWebhooks,
		"leaderElection", enableLeaderElection,
		"maxConcurrentReconciles", maxConcurrentReconciles,
		"collectorTTL", collectorTTL.String(),
		"prometheusQPS", prometheusQPS,
		"prometheusBurst", prometheusBurst,
		"prometheusTimeout", prometheusTimeout.String(),
		"watchNamespaces", watchNamespaces,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
