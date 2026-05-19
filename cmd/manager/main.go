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

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/controller"
	"github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	_ "github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/webhook"
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
	utilruntime.Must(rightsizev1alpha1.AddToScheme(scheme))
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
		"Maximum number of RightSizePolicy reconciles running in parallel. Increase for large clusters with many policies.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated list of namespaces to watch. Empty means all namespaces (cluster-scoped). "+
			"Reduces informer cache memory on large clusters where policies exist in a few namespaces.")

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

	mgrOpts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "kube-rightsize.rightsize.io",
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	}

	// Namespace-scoped caching: when --watch-namespaces is set, only watch
	// the listed namespaces for namespace-scoped resources (Pods, Deployments,
	// HPAs, RightSizePolicies, etc.). Cluster-scoped resources (Nodes,
	// RightSizeDefaults) are always watched regardless.
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

	// Setup the RightSizePolicyReconciler with a real Prometheus metrics factory and clientset.
	if err = (&controller.RightSizePolicyReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Clientset:               clientset,
		Recorder:                mgr.GetEventRecorder("kube-rightsize"),
		CollectorTTL:            collectorTTL,
		MaxConcurrentReconciles: maxConcurrentReconciles,
		MetricsFactory: func(address string, opts *metrics.CollectorOptions) (metrics.MetricsCollector, error) {
			collector, err := metrics.NewPrometheusCollectorWithOptions(address, ctrl.Log.WithName("prometheus"), opts)
			if err != nil {
				return nil, fmt.Errorf("creating Prometheus collector for %s: %w", address, err)
			}
			return metrics.NewRateLimitedCollector(collector, prometheusQPS, prometheusBurst), nil
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RightSizePolicy")
		os.Exit(1)
	}

	// Register webhooks (requires cert-manager or manual TLS cert provisioning).
	if enableWebhooks {
		if err = ctrl.NewWebhookManagedBy(mgr, &rightsizev1alpha1.RightSizePolicy{}).
			WithDefaulter(&webhook.RightSizePolicyDefaulter{}).
			WithValidator(&webhook.RightSizePolicyValidator{}).
			Complete(); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "RightSizePolicy")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &rightsizev1alpha1.RightSizeDefaults{}).
			WithValidator(&webhook.RightSizeDefaultsValidator{}).
			Complete(); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "RightSizeDefaults")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &rightsizev1alpha1.RightSizeNamespaceDefaults{}).
			WithValidator(&webhook.RightSizeNamespaceDefaultsValidator{}).
			Complete(); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "RightSizeNamespaceDefaults")
			os.Exit(1)
		}
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

	setupLog.Info("starting manager", "version", version, "commit", commit, "date", date)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
