package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	runtimewebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/kaito-project/llm-gateway-auth/api/v1alpha1"
	"github.com/kaito-project/llm-gateway-auth/internal/controller"
	"github.com/kaito-project/llm-gateway-auth/internal/webhook"
)

const (
	caName         = "apikey-operator-ca"
	caOrganization = "kaito.sh"
)

func main() {
	var metricsAddr string
	var probeAddr string
	var enableWebhook bool
	var enableLeaderElection bool
	var webhookPort int
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableWebhook, "enable-webhook", true, "Enable the validating admission webhook server.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the validating admission webhook listens on.")
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

	// Webhook config from env (set via Helm + Downward API). We need these
	// up front because the webhook server is wired into the manager.
	var (
		podNamespace, webhookName, secretName, serviceName string
		secretLister                                       corev1listers.SecretLister
	)
	if enableWebhook {
		podNamespace = os.Getenv("POD_NAMESPACE")
		webhookName = os.Getenv("WEBHOOK_NAME")
		secretName = os.Getenv("WEBHOOK_SECRET_NAME")
		serviceName = os.Getenv("WEBHOOK_SERVICE_NAME")
		if podNamespace == "" || webhookName == "" || secretName == "" || serviceName == "" {
			ctrl.Log.Error(nil, "POD_NAMESPACE, WEBHOOK_NAME, WEBHOOK_SECRET_NAME, and WEBHOOK_SERVICE_NAME must be set when --enable-webhook=true")
			os.Exit(1)
		}

		// Build a namespace-scoped Secret informer/lister so the webhook
		// server can read the rotator-managed serving cert directly out of
		// the Secret on every TLS handshake. This avoids the kubelet
		// projected-Secret refresh lag and removes the need for an on-disk
		// cert mount.
		ctx := ctrl.SetupSignalHandler()
		var err error
		secretLister, err = newSecretLister(ctx, ctrl.GetConfigOrDie(), podNamespace)
		if err != nil {
			ctrl.Log.Error(err, "unable to start secret lister for webhook TLS")
			os.Exit(1)
		}
		// Stash the signal context for mgr.Start below.
		signalCtx = ctx
	}

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "apikey-operator.kaito.sh",
	}
	if enableWebhook {
		mgrOpts.WebhookServer = runtimewebhook.NewServer(runtimewebhook.Options{
			Port: webhookPort,
			TLSOpts: []func(*tls.Config){
				func(cfg *tls.Config) {
					cfg.MinVersion = tls.VersionTLS13
					cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
						return loadCertificateFromSecret(secretLister, podNamespace, secretName)
					}
				},
			},
		})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
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

	if enableWebhook {
		// certsReady is closed by cert-controller once the webhook serving
		// cert has been written to the Secret and the
		// ValidatingWebhookConfiguration's caBundle has been patched. We do
		// not gate webhook registration on it: with no on-disk CertDir the
		// rotator's ensureCertsMounted() poll never succeeds and IsReady
		// never closes. Instead the GetCertificate callback above returns
		// (nil, nil) until the Secret is populated, so kube-apiserver simply
		// retries the handshake while bootstrap is in flight.
		certsReady := make(chan struct{})

		if err := rotator.AddRotator(mgr, &rotator.CertRotator{
			SecretKey: types.NamespacedName{
				Namespace: podNamespace,
				Name:      secretName,
			},
			CAName:         caName,
			CAOrganization: caOrganization,
			DNSName:        fmt.Sprintf("%s.%s.svc", serviceName, podNamespace),
			IsReady:        certsReady,
			// Only the leader writes the Secret; otherwise the two replicas
			// race on Update and one crashes with a 409 conflict.
			RequireLeaderElection: enableLeaderElection,
			Webhooks: []rotator.WebhookInfo{{
				Name: webhookName,
				Type: rotator.Validating,
			}},
		}); err != nil {
			ctrl.Log.Error(err, "unable to set up cert rotator")
			os.Exit(1)
		}

		// Register the validator up front so controller-runtime starts the
		// webhook server as part of mgr.Start(); the TLS handshake is
		// satisfied dynamically by GetCertificate against the Secret lister.
		mgr.GetWebhookServer().Register(
			"/validate-kaito-sh-v1alpha1-apikey",
			admission.WithValidator[*v1alpha1.APIKey](scheme, &webhook.APIKeyValidator{}),
		)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctx := signalCtx
	if ctx == nil {
		ctx = ctrl.SetupSignalHandler()
	}
	ctrl.Log.Info("starting controller manager")
	if err := mgr.Start(ctx); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// signalCtx is set by main() when the webhook secret lister needs to start
// before the manager. It is then reused as the manager's stop channel.
var signalCtx context.Context

// newSecretLister starts a namespace-scoped Secret informer and returns a
// lister once the cache has synced. The webhook TLS GetCertificate callback
// reads from this lister on every handshake.
func newSecretLister(ctx context.Context, cfg *rest.Config, namespace string) (corev1listers.SecretLister, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 24*time.Hour, informers.WithNamespace(namespace))
	informer := factory.Core().V1().Secrets().Informer()
	lister := factory.Core().V1().Secrets().Lister()
	go factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return nil, errors.New("failed waiting for webhook secret cache to sync")
	}
	return lister, nil
}

// loadCertificateFromSecret reads tls.crt/tls.key out of the rotator-managed
// Secret and returns them as a parsed TLS keypair. Called on every TLS
// handshake so cert rotations are picked up without restarts or remounts.
//
// Returning (nil, nil) when the Secret doesn't exist yet makes the TLS stack
// surface "no certificates configured" to the client (kube-apiserver), which
// will retry — preferable to a hard internal error during initial bootstrap.
func loadCertificateFromSecret(lister corev1listers.SecretLister, namespace, name string) (*tls.Certificate, error) {
	secret, err := lister.Secrets(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil //nolint:nilerr
		}
		return nil, fmt.Errorf("get secret %s/%s: %w", namespace, name, err)
	}
	crt, ok := secret.Data["tls.crt"]
	if !ok {
		return nil, fmt.Errorf("tls.crt missing from secret %s/%s", namespace, name)
	}
	key, ok := secret.Data["tls.key"]
	if !ok {
		return nil, fmt.Errorf("tls.key missing from secret %s/%s", namespace, name)
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		return nil, fmt.Errorf("parse tls keypair from secret %s/%s: %w", namespace, name, err)
	}
	return &pair, nil
}
