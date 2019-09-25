package reconciler

import (
	"context"
	"knative.dev/eventing-contrib/pkg/reconciler/eventtype"
	"knative.dev/eventing/pkg/reconciler"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"log"
	"os"

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

	secretLister := secretinformer.Get(ctx).Lister()
	ksvcLister := ksvcinformer.Get(ctx).Lister()
	servingClient := servingclient.Get(ctx)

	// TODO(nachtmaar): check if reconciler needs to be public
	r := &Reconciler{
		Base:          reconciler.NewBase(ctx, controllerAgentName, cmw),
		secretLister:  secretLister,
		ksvcLister:    ksvcLister,
		servingClient: servingClient,
		// TODO(nachtmaar)
		receiveAdapterImage: receiveAdapterImage,
		webhookClient:       gitHubWebhookClient{},
		// TODO(nachtmaar)
		eventTypeReconciler: eventtype.Reconciler{
			Scheme: mgr.GetScheme(),
		},
	}
	return controller.NewImpl(r, r.Logger, ReconcilerName)
}
