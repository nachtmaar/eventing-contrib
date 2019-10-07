package reconciler

import (
	"context"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"knative.dev/eventing/pkg/duck"
	"knative.dev/eventing/pkg/reconciler"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"

	"log"
	"os"
	"knative.dev/eventing/pkg/apis/sources/v1alpha1"

	githublient "knative.dev/eventing-contrib/github/pkg/client/injection/client"
	eventtypeinformer "knative.dev/eventing/pkg/client/injection/informers/eventing/v1alpha1/eventtype"
	deploymentinformer "knative.dev/pkg/client/injection/kube/informers/apps/v1/deployment"
	githubsourceinformer "knative.dev/eventing-contrib/github/pkg/client/injection/informers/sources/v1alpha1/githubsource"
	secretinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/secret"
	servingclient "knative.dev/serving/pkg/client/injection/client"
	ksvcinformer "knative.dev/serving/pkg/client/injection/informers/serving/v1alpha1/service"
	sourcescheme "knative.dev/eventing-contrib/kafka/source/pkg/client/clientset/versioned/scheme"
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

	githubClientSet := githublient.Get(ctx)
	githubsourceInformer := githubsourceinformer.Get(ctx)
	secretLister := secretinformer.Get(ctx).Lister()
	ksvcLister := ksvcinformer.Get(ctx).Lister()
	ksvcInformer := ksvcinformer.Get(ctx).Informer()
	servingClient := servingclient.Get(ctx)
	deploymentInformer := deploymentinformer.Get(ctx)
	eventTypeInformer := eventtypeinformer.Get(ctx)

	// TODO(nachtmaar): check if reconciler needs to be public
	r := &Reconciler{
		Base:                   reconciler.NewBase(ctx, controllerAgentName, cmw),
		loggingContext:         ctx,
		githubSourcesClientSet: githubClientSet,
		githubsourceLister:     githubsourceInformer.Lister(),
		secretLister:           secretLister,
		ksvcLister:             ksvcLister,
		servingClient:          servingClient,
		receiveAdapterImage:    receiveAdapterImage,
		webhookClient:          gitHubWebhookClient{},
	}

	impl := controller.NewImpl(r, r.Logger, ReconcilerName)
	r.sinkReconciler = duck.NewSinkReconciler(ctx, impl.EnqueueKey)

	r.Logger.Info("Setting up event handlers")
	// Required to register reconciliation of GithubSource
	githubsourceInformer.Informer().AddEventHandler(controller.HandleAll(impl.Enqueue))

	deploymentInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.Filter(v1alpha1.SchemeGroupVersion.WithKind("GitHubSource")),
		Handler:    controller.HandleAll(impl.EnqueueControllerOf),
	})

	eventTypeInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.Filter(v1alpha1.SchemeGroupVersion.WithKind("GitHubSource")),
		Handler:    controller.HandleAll(impl.EnqueueControllerOf),
	})

	ksvcInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.Filter(v1alpha1.SchemeGroupVersion.WithKind("GitHubSource")),
		Handler:    controller.HandleAll(impl.EnqueueControllerOf),
	})

	// Handle config map update for logging or metrics changes
	cmw.Watch(logging.ConfigMapName(), r.UpdateFromLoggingConfigMap)
	cmw.Watch(metrics.ConfigMapName(), r.UpdateFromMetricsConfigMap)

	return impl
}

func init() {
	sourcescheme.AddToScheme(scheme.Scheme)
}
