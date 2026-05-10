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

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	"github.com/SebTardif/kube-rightsize/internal/controller"
	"github.com/SebTardif/kube-rightsize/internal/metrics"
	_ "github.com/SebTardif/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardif/kube-rightsize/internal/webhook"
)

var (
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

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true,
		"Enable admission webhooks for defaulting and validation.")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgrOpts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "kube-rightsize.rightsize.io",
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
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Clientset: clientset,
		MetricsFactory: func(address string) (metrics.MetricsCollector, error) {
			collector, err := metrics.NewPrometheusCollector(address, nil)
			if err != nil {
				return nil, fmt.Errorf("creating Prometheus collector for %s: %w", address, err)
			}
			return metrics.NewRateLimitedCollector(collector, 10, 20), nil
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
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "version", version, "commit", commit, "date", date)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
