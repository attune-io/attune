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

	corev1 "k8s.io/api/core/v1"
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
	"github.com/attune-io/attune/internal/metrics"
	_ "github.com/attune-io/attune/internal/operatormetrics"
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
	var watchNamespaces string
	var prometheusTimeout time.Duration

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
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 1,
		"Maximum number of AttunePolicy reconciles running in parallel. Increase for large clusters with many policies.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated list of namespaces to watch. Empty means all namespaces (cluster-scoped). "+
			"Reduces informer cache memory on large clusters where policies exist in a few namespaces.")
	flag.DurationVar(&prometheusTimeout, "prometheus-timeout", 5*time.Minute,
		"Maximum time allowed for workload processing (including Prometheus queries) during a single reconciliation cycle. "+
			"If exceeded, partial results are used and the status condition indicates the timeout.")

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

	// Strip unused fields from cached Pods to reduce informer memory at scale.
	// See internal/transform/pod.go for the list of preserved vs stripped fields.
	if mgrOpts.Cache.ByObject == nil {
		mgrOpts.Cache.ByObject = make(map[client.Object]cache.ByObject)
	}
	mgrOpts.Cache.ByObject[&corev1.Pod{}] = cache.ByObject{
		Transform: transform.StripPodFields,
	}

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
	clusterTLSMinVersion := metrics.DetectOpenShiftTLSProfile(mgr.GetConfig(), setupLog)

	// Setup the AttunePolicyReconciler with a real Prometheus metrics factory and clientset.
	reconciler := controller.NewAttunePolicyReconciler()
	reconciler.Client = mgr.GetClient()
	reconciler.Scheme = mgr.GetScheme()
	reconciler.Clientset = clientset
	reconciler.Recorder = mgr.GetEventRecorder("attune")
	reconciler.CollectorTTL = collectorTTL
	reconciler.MaxConcurrentReconciles = maxConcurrentReconciles
	reconciler.PrometheusTimeout = prometheusTimeout
	reconciler.MetricsFactory = func(address string, opts *metrics.CollectorOptions) (metrics.MetricsCollector, error) {
		if opts == nil {
			opts = &metrics.CollectorOptions{}
		}
		if opts.TLSMinVersion == 0 && clusterTLSMinVersion != 0 {
			opts.TLSMinVersion = clusterTLSMinVersion
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
