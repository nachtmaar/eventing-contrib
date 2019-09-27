package reconciler

import (
	"context"
	"knative.dev/eventing/pkg/reconciler"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"

	"log"
	"os"

	githubsourceinformer "knative.dev/eventing-contrib/github/pkg/client/injection/informers/sources/v1alpha1/githubsource"
	secretinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/secret"
	servingclient "knative.dev/serving/pkg/client/injection/client"
	ksvcinformer "knative.dev/serving/pkg/client/injection/informers/serving/v1alpha1/service"
)

const (
	// Name of the reconciler
	ReconcilerName = "GithubSource"
)

func NewController(
	ctx context.Context,
	cmw configmap.Watcher,
) *controller.Impl {
	receiveAdapterImage, defined := os.LookupEnv(raImageEnvVar)
	if !defined {
		logging.FromContext(ctx).Errorf("required environment variable %q not defined", raImageEnvVar)
	}
	log.Println("Creating the GitHub Source controller.")

	githubsourceInformer := githubsourceinformer.Get(ctx)
	secretLister := secretinformer.Get(ctx).Lister()
	ksvcLister := ksvcinformer.Get(ctx).Lister()
	servingClient := servingclient.Get(ctx)

	// TODO(nachtmaar): check if reconciler needs to be public
	r := &Reconciler{
		Base:                 reconciler.NewBase(ctx, controllerAgentName, cmw),
		loggingContext:       ctx,
		githubsourceInformer: githubsourceInformer,
		secretLister:         secretLister,
		ksvcLister:           ksvcLister,
		servingClient:        servingClient,
		receiveAdapterImage:  receiveAdapterImage,
		webhookClient:        gitHubWebhookClient{},
		// TODO(nachtmaar)
		//eventTypeReconciler: eventtype.Reconciler{
		//},
	}

	impl := controller.NewImpl(r, r.Logger, ReconcilerName)

	r.Logger.Info("Setting up event handlers")
	// Required to register reconciliation of GithubSource
	githubsourceInformer.Informer().AddEventHandler(controller.HandleAll(impl.Enqueue))

	cmw.Watch(logging.ConfigMapName(), r.UpdateFromLoggingConfigMap)
	cmw.Watch(metrics.ConfigMapName(), r.UpdateFromMetricsConfigMap)
	return impl
}
