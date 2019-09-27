/*
Copyright 2018 The Knative Authors

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

package reconciler

import (
	"context"
	"fmt"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	githubsourcev1alph1ainformers "knative.dev/eventing-contrib/github/pkg/client/informers/externalversions/sources/v1alpha1"
	"knative.dev/eventing/pkg/reconciler"
	pkgLogging "knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"

	// TODO(nachtmaar) check import names
	servingv1alphalisters "knative.dev/serving/pkg/client/listers/serving/v1alpha1"

	"strings"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	eventingv1alpha1 "knative.dev/eventing/pkg/apis/eventing/v1alpha1"
	"knative.dev/pkg/logging"
	servingv1alpha1 "knative.dev/serving/pkg/apis/serving/v1alpha1"
	"knative.dev/serving/pkg/client/clientset/versioned"

	sourcesv1alpha1 "knative.dev/eventing-contrib/github/pkg/apis/sources/v1alpha1"
	"knative.dev/eventing-contrib/github/pkg/reconciler/resources"
	"knative.dev/eventing-contrib/pkg/controller/sinks"
	"knative.dev/eventing-contrib/pkg/reconciler/eventtype"
)

const (
	// controllerAgentName is the string used by this controller to identify
	// itself when creating events.
	controllerAgentName = "github-source-controller"
	raImageEnvVar       = "GH_RA_IMAGE"
	finalizerName       = controllerAgentName
	component           = "githubsource"
)

// Reconciler reconciles a GitHubSource object
type Reconciler struct {
	*reconciler.Base

	loggingContext context.Context
	loggingConfig  *pkgLogging.Config

	githubsourceInformer githubsourcev1alph1ainformers.GitHubSourceInformer
	receiveAdapterImage  string
	webhookClient        webhookClient
	// TODO(nachtmaar): convert to pkg/controller too
	//eventTypeReconciler eventtype.Reconciler
	secretLister  v1.SecretLister
	ksvcLister    servingv1alphalisters.ServiceLister
	servingClient versioned.Interface

	metricsConfig *metrics.ExporterOptions
}

type webhookArgs struct {
	source                *sourcesv1alpha1.GitHubSource
	domain                string
	accessToken           string
	secretToken           string
	alternateGitHubAPIURL string
	hookID                string
}

// Reconcile reads that state of the cluster for a GitHubSource
// object and makes changes based on the state read and what is in the
// GitHubSource.Spec
func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	// TODO(nachtmaar) remove
	fmt.Println("reconcile")

	// TODO(nachtmaar) why desugared logger
	logger := logging.FromContext(ctx).Desugar()
	// TODO(nachtmaar) remove
	logger.Info("Reconcile", zap.Any("key", key))

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.Error("invalid resource key: %s", zap.Any("key", key))
		return nil
	}

	src, err := r.githubsourceInformer.Lister().GitHubSources(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		logger.Error("could not find github source", zap.Any("key", key))
	} else if err != nil {
		return err
	}

	githubsource := src.DeepCopy()

	// See if the source has been deleted
	accessor, err := meta.Accessor(githubsource)
	if err != nil {
		logger.Error("Failed to get metadata accessor", zap.Error(err))
		return err
	}

	var reconcileErr error
	if accessor.GetDeletionTimestamp() == nil {
		reconcileErr = r.reconcile(ctx, githubsource)
	} else {
		reconcileErr = r.finalize(ctx, githubsource)
	}

	return reconcileErr
}

func (r *Reconciler) reconcile(ctx context.Context, source *sourcesv1alpha1.GitHubSource) error {
	source.Status.ObservedGeneration = source.Generation
	source.Status.InitializeConditions()

	accessToken, err := r.secretFrom(ctx, source.Namespace, source.Spec.AccessToken.SecretKeyRef)
	if err != nil {
		source.Status.MarkNoSecrets("AccessTokenNotFound", "%s", err)
		return err
	}
	secretToken, err := r.secretFrom(ctx, source.Namespace, source.Spec.SecretToken.SecretKeyRef)
	if err != nil {
		source.Status.MarkNoSecrets("SecretTokenNotFound", "%s", err)
		return err
	}
	source.Status.MarkSecrets()

	uri, err := sinks.GetSinkURI(ctx, r.DynamicClientSet, source.Spec.Sink, source.Namespace)
	if err != nil {
		source.Status.MarkNoSink("NotFound", "%s", err)
		return err
	}
	source.Status.MarkSink(uri)

	ksvc, err := r.getOwnedService(ctx, source)
	if err != nil {
		if apierrors.IsNotFound(err) {
			ksvc = resources.MakeService(source, r.receiveAdapterImage)
			// TODO(nachtmaar) - is this still required ?
			//if err = controllerutil.SetControllerReference(source, ksvc, r.scheme); err != nil {
			//	return err
			//}
			// TODO(nachtmaar) is v1alpha1 correct ?
			if _, err = r.servingClient.ServingV1alpha1().Services(ksvc.Namespace).Create(ksvc); err != nil {
				return err
			}
			r.Recorder.Eventf(source, corev1.EventTypeNormal, "ServiceCreated", "Created Service %q", ksvc.Name)
			// TODO: Mark Deploying for the ksvc
			// Wait for the Service to get a status
			return nil
		}
		// Error was something other than NotFound
		return err
	}

	routeCondition := ksvc.Status.GetCondition(servingv1alpha1.ServiceConditionReady)
	if routeCondition != nil && routeCondition.Status == corev1.ConditionTrue && ksvc.Status.URL != nil {
		receiveAdapterDomain := ksvc.Status.URL.Host
		// TODO: Mark Deployed for the ksvc
		// TODO: Mark some condition for the webhook status?
		r.addFinalizer(source)
		if source.Status.WebhookIDKey == "" {
			args := &webhookArgs{
				source:                source,
				domain:                receiveAdapterDomain,
				accessToken:           accessToken,
				secretToken:           secretToken,
				alternateGitHubAPIURL: source.Spec.GitHubAPIURL,
			}

			hookID, err := r.createWebhook(ctx, args)
			if err != nil {
				return err
			}
			source.Status.WebhookIDKey = hookID
		}
	} else {
		return nil
	}

	//TODO(nachtmaar)
	//err = r.reconcileEventTypes(ctx, source)
	//if err != nil {
	//	return err
	//}
	source.Status.MarkEventTypes()

	return nil
}

func (r *Reconciler) finalize(ctx context.Context, source *sourcesv1alpha1.GitHubSource) error {
	// Always remove the finalizer. If there's a failure cleaning up, an event
	// will be recorded allowing the webhook to be removed manually by the
	// operator.
	r.removeFinalizer(source)

	// If a webhook was created, try to delete it
	if source.Status.WebhookIDKey != "" {
		// Get access token
		accessToken, err := r.secretFrom(ctx, source.Namespace, source.Spec.AccessToken.SecretKeyRef)
		if err != nil {
			source.Status.MarkNoSecrets("AccessTokenNotFound", "%s", err)
			r.Recorder.Eventf(source, corev1.EventTypeWarning, "FailedFinalize", "Could not delete webhook %q: %v", source.Status.WebhookIDKey, err)
			return err
		}

		args := &webhookArgs{
			source:                source,
			accessToken:           accessToken,
			alternateGitHubAPIURL: source.Spec.GitHubAPIURL,
			hookID:                source.Status.WebhookIDKey,
		}
		// Delete the webhook using the access token and stored webhook ID
		err = r.deleteWebhook(ctx, args)
		if err != nil {
			r.Recorder.Eventf(source, corev1.EventTypeWarning, "FailedFinalize", "Could not delete webhook %q: %v", source.Status.WebhookIDKey, err)
			return err
		}
		// Webhook deleted, clear ID
		source.Status.WebhookIDKey = ""
	}
	return nil
}
func (r *Reconciler) createWebhook(ctx context.Context, args *webhookArgs) (string, error) {
	logger := logging.FromContext(ctx)

	logger.Info("creating GitHub webhook")

	owner, repo, err := parseOwnerRepoFrom(args.source.Spec.OwnerAndRepository)
	if err != nil {
		return "", err
	}

	hookOptions := &webhookOptions{
		accessToken: args.accessToken,
		secretToken: args.secretToken,
		domain:      args.domain,
		owner:       owner,
		repo:        repo,
		events:      args.source.Spec.EventTypes,
		secure:      args.source.Spec.Secure,
	}
	hookID, err := r.webhookClient.Create(ctx, hookOptions, args.alternateGitHubAPIURL)
	if err != nil {
		return "", fmt.Errorf("failed to create webhook: %v", err)
	}
	return hookID, nil
}

func (r *Reconciler) deleteWebhook(ctx context.Context, args *webhookArgs) error {
	logger := logging.FromContext(ctx)

	logger.Info("deleting GitHub webhook")

	owner, repo, err := parseOwnerRepoFrom(args.source.Spec.OwnerAndRepository)
	if err != nil {
		return err
	}

	hookOptions := &webhookOptions{
		accessToken: args.accessToken,
		owner:       owner,
		repo:        repo,
		events:      args.source.Spec.EventTypes,
		secure:      args.source.Spec.Secure,
	}
	err = r.webhookClient.Delete(ctx, hookOptions, args.hookID, args.alternateGitHubAPIURL)
	if err != nil {
		return fmt.Errorf("failed to delete webhook: %v", err)
	}
	return nil
}

func (r *Reconciler) secretFrom(ctx context.Context, namespace string, secretKeySelector *corev1.SecretKeySelector) (string, error) {

	secret, err := r.secretLister.Secrets(namespace).Get(secretKeySelector.Name)
	if err != nil {
		return "", err
	}
	secretVal, ok := secret.Data[secretKeySelector.Key]
	if !ok {
		return "", fmt.Errorf(`key "%s" not found in secret "%s"`, secretKeySelector.Key, secretKeySelector.Name)
	}
	return string(secretVal), nil
}

func parseOwnerRepoFrom(ownerAndRepository string) (string, string, error) {
	components := strings.Split(ownerAndRepository, "/")
	if len(components) > 2 {
		return "", "", fmt.Errorf("ownerAndRepository is malformatted, expected 'owner/repository' but found %q", ownerAndRepository)
	}
	owner := components[0]
	if len(owner) == 0 && len(components) > 1 {
		return "", "", fmt.Errorf("owner is empty, expected 'owner/repository' but found %q", ownerAndRepository)
	}
	repo := ""
	if len(components) > 1 {
		repo = components[1]
	}

	return owner, repo, nil
}

func (r *Reconciler) getOwnedService(ctx context.Context, source *sourcesv1alpha1.GitHubSource) (*servingv1alpha1.Service, error) {
	ksvList, err := r.ksvcLister.Services(source.Namespace).List(labels.Everything())

	if err != nil {
		return nil, err
	}
	for _, ksvc := range ksvList {
		if metav1.IsControlledBy(ksvc, source) {
			//TODO if there are >1 controlled, delete all but first?
			return ksvc, nil
		}
	}

	return nil, apierrors.NewNotFound(servingv1alpha1.Resource("services"), "")
}

//func (r *Reconciler) reconcileEventTypes(ctx context.Context, source *sourcesv1alpha1.GitHubSource) error {
//	args := r.newEventTypeReconcilerArgs(source)
//	return r.eventTypeReconciler.Reconcile(ctx, source, args)
//}

func (r *Reconciler) newEventTypeReconcilerArgs(source *sourcesv1alpha1.GitHubSource) *eventtype.ReconcilerArgs {
	specs := make([]eventingv1alpha1.EventTypeSpec, 0)
	for _, et := range source.Spec.EventTypes {
		spec := eventingv1alpha1.EventTypeSpec{
			Type:   sourcesv1alpha1.GitHubEventType(et),
			Source: sourcesv1alpha1.GitHubEventSource(source.Spec.OwnerAndRepository),
			Broker: source.Spec.Sink.Name,
		}
		specs = append(specs, spec)
	}
	return &eventtype.ReconcilerArgs{
		Specs:     specs,
		Namespace: source.Namespace,
		Labels:    resources.Labels(source.Name),
		Kind:      source.Spec.Sink.Kind,
	}
}

func (r *Reconciler) addFinalizer(s *sourcesv1alpha1.GitHubSource) {
	finalizers := sets.NewString(s.Finalizers...)
	finalizers.Insert(finalizerName)
	s.Finalizers = finalizers.List()
}

func (r *Reconciler) removeFinalizer(s *sourcesv1alpha1.GitHubSource) {
	finalizers := sets.NewString(s.Finalizers...)
	finalizers.Delete(finalizerName)
	s.Finalizers = finalizers.List()
}

func (r *Reconciler) UpdateFromLoggingConfigMap(cfg *corev1.ConfigMap) {
	if cfg != nil {
		delete(cfg.Data, "_example")
	}

	logcfg, err := pkgLogging.NewConfigFromConfigMap(cfg)
	if err != nil {
		logging.FromContext(r.loggingContext).Warn("failed to create logging config from configmap", zap.String("cfg.Name", cfg.Name))
		return
	}
	r.loggingConfig = logcfg
	logging.FromContext(r.loggingContext).Info("Update from logging ConfigMap", zap.Any("ConfigMap", cfg))
}

func (r *Reconciler) UpdateFromMetricsConfigMap(cfg *corev1.ConfigMap) {
	if cfg != nil {
		delete(cfg.Data, "_example")
	}

	r.metricsConfig = &metrics.ExporterOptions{
		Domain:    metrics.Domain(),
		Component: component,
		ConfigMap: cfg.Data,
	}
	logging.FromContext(r.loggingContext).Info("Update from metrics ConfigMap", zap.Any("ConfigMap", cfg))
}
