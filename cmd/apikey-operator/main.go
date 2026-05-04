package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/controller"
	"github.com/kaito-project/llm-gateway-auth/internal/webhook"
)

const (
	webhookCertDir = "/tmp/k8s-webhook-server/serving-certs"
	caName         = "apikey-operator-ca"
	caOrganization = "kaito.sh"
)

func main() {
	var metricsAddr string
	var probeAddr string
	var enableWebhook bool
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableWebhook, "enable-webhook", true, "Enable the validating admission webhook server.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Ensures only one active controller at a time.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to add clientgo scheme")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to add corev1 scheme")
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to add v1alpha1 scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "apikey-operator.kaito.sh",
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.APIKeyReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "APIKey")
		os.Exit(1)
	}

	// certsReady is closed by cert-controller once the webhook serving cert
	// is written to disk and the ValidatingWebhookConfiguration's caBundle is
	// patched. We must defer webhook registration until then so the server
	// doesn't start with stale or missing certs.
	certsReady := make(chan struct{})

	if enableWebhook {
		podNamespace := os.Getenv("POD_NAMESPACE")
		webhookName := os.Getenv("WEBHOOK_NAME")
		secretName := os.Getenv("WEBHOOK_SECRET_NAME")
		serviceName := os.Getenv("WEBHOOK_SERVICE_NAME")
		if podNamespace == "" || webhookName == "" || secretName == "" || serviceName == "" {
			ctrl.Log.Error(nil, "POD_NAMESPACE, WEBHOOK_NAME, WEBHOOK_SECRET_NAME, and WEBHOOK_SERVICE_NAME must be set when --enable-webhook=true")
			os.Exit(1)
		}

		if err := rotator.AddRotator(mgr, &rotator.CertRotator{
			SecretKey: types.NamespacedName{
				Namespace: podNamespace,
				Name:      secretName,
			},
			CertDir:        webhookCertDir,
			CAName:         caName,
			CAOrganization: caOrganization,
			DNSName:        fmt.Sprintf("%s.%s.svc", serviceName, podNamespace),
			IsReady:        certsReady,
			Webhooks: []rotator.WebhookInfo{{
				Name: webhookName,
				Type: rotator.Validating,
			}},
		}); err != nil {
			ctrl.Log.Error(err, "unable to set up cert rotator")
			os.Exit(1)
		}

		go func() {
			<-certsReady
			ctrl.Log.Info("webhook certs ready, registering validator")
			mgr.GetWebhookServer().Register(
				"/validate-kaito-sh-v1alpha1-apikey",
				admission.WithValidator[*v1alpha1.APIKey](scheme, &webhook.APIKeyValidator{}),
			)
		}()
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting controller manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
