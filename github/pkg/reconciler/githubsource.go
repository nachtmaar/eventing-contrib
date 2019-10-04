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
	"errors"
	"fmt"
	"knative.dev/eventing/pkg/duck"
	"reflect"

	"k8s.io/apimachinery/pkg/api/equality"
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
	eventingcontribversioned "knative.dev/eventing-contrib/github/pkg/client/clientset/versioned"
	eventingv1alpha1 "knative.dev/eventing/pkg/apis/eventing/v1alpha1"
	"knative.dev/pkg/logging"
	servingv1alpha1 "knative.dev/serving/pkg/apis/serving/v1alpha1"
	servingversioned "knative.dev/serving/pkg/client/clientset/versioned"

	sourcesv1alpha1 "knative.dev/eventing-contrib/github/pkg/apis/sources/v1alpha1"
	"knative.dev/eventing-contrib/github/pkg/reconciler/resources"
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

	// image of receive adapter for GitHub
	receiveAdapterImage  string
	// handles GitHub webhook lifecycle
	webhookClient        webhookClient

	// used to get GitHubSource
	githubsourceInformer githubsourcev1alph1ainformers.GitHubSourceInformer
	// used to get Github secret
	secretLister    v1.SecretLister
	// used to get knative service
	ksvcLister      servingv1alphalisters.ServiceLister
	// used to create knative service
	servingClient   servingversioned.Interface
	// used to modify GithubSource object
	githubSourcesClientSet eventingcontribversioned.Interface

	loggingContext       context.Context
	sinkReconciler       *duck.SinkReconciler
	loggingConfig        *pkgLogging.Config
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
		logger.Error("invalid resource key", zap.Any("key", key))
		return nil
	}

	src, err := r.githubsourceInformer.Lister().GitHubSources(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		logger.Error("could not find github source", zap.Any("key", key))
		return nil
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

	if accessor.GetDeletionTimestamp() == nil {
		return r.reconcile(ctx, githubsource)
	}
	return r.finalize(ctx, githubsource)
}

func (r *Reconciler) reconcile(ctx context.Context, source *sourcesv1alpha1.GitHubSource) error {
	// TODO(nachtmaar) use desugared logger everywhere
	logger := logging.FromContext(ctx).Desugar()

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

	if source.Spec.Sink == nil {
		source.Status.MarkNoSink("Missing", "Sink missing from spec")
		return errors.New("spec.sink missing")
	}

	sinkObjRef := source.Spec.Sink
	if sinkObjRef.Namespace == "" {
		sinkObjRef.Namespace = source.Namespace
	}
	githubsourceDesc := fmt.Sprintf("%s/%s,%s", source.Namespace, source.Name, source.GroupVersionKind().String())
	uri, err := r.sinkReconciler.GetSinkURI(sinkObjRef, source, githubsourceDesc)
	if err != nil {
		source.Status.MarkNoSink("NotFound", "%s", err)
		return err
	}
	source.Status.MarkSink(uri)

	logger.Debug("checking if knative service is already created")
	ksvc, err := r.getOwnedService(ctx, source)
	if err != nil {
		if apierrors.IsNotFound(err) {
			ksvc = resources.MakeService(source, r.receiveAdapterImage)
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
	logger.Debug("checking if webhook can be created")
	if routeCondition != nil && routeCondition.Status == corev1.ConditionTrue && ksvc.Status.URL != nil {
		receiveAdapterDomain := ksvc.Status.URL.Host
		// TODO: Mark Deployed for the ksvc
		// TODO: Mark some condition for the webhook status?
		r.addFinalizer(ctx, source)
		if source.Status.WebhookIDKey == "" {
			args := &webhookArgs{
				source:                source,
				domain:                receiveAdapterDomain,
				accessToken:           accessToken,
				secretToken:           secretToken,
				alternateGitHubAPIURL: source.Spec.GitHubAPIURL,
			}

			logger.Info("creating webhook", zap.Any("domain", args.domain))
			hookID, err := r.createWebhook(ctx, args)
			if err != nil {
				return err
			}
			source.Status.WebhookIDKey = hookID
			r.updateStatusHandleError(ctx, source)
		}
	} else {
		return nil
	}

	//TODO(nachtmaar)
	err = r.reconcileEventTypes(ctx, source)
	if err != nil {
		return err
	}
	source.Status.MarkEventTypes()

	return nil
}

func (r *Reconciler) updateStatusHandleError(ctx context.Context, source *sourcesv1alpha1.GitHubSource) {
	logger := logging.FromContext(ctx).Desugar()
	if _, err := r.updateStatus(ctx, source); err != nil {
		logger.Error("could not update status", zap.Error(err))
	}
}

func (r *Reconciler) finalize(ctx context.Context, source *sourcesv1alpha1.GitHubSource) error {
	// Always remove the finalizer. If there's a failure cleaning up, an event
	// will be recorded allowing the webhook to be removed manually by the
	// operator.
	r.removeFinalizer(ctx, source)

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

func (r *Reconciler) addFinalizer(ctx context.Context, source *sourcesv1alpha1.GitHubSource) {
	finalizers := sets.NewString(source.Finalizers...)
	finalizers.Insert(finalizerName)
	source.Finalizers = finalizers.List()

	r.updateFinalizers(ctx, source)
}

func (r *Reconciler) removeFinalizer(ctx context.Context, source *sourcesv1alpha1.GitHubSource) {
	finalizers := sets.NewString(source.Finalizers...)
	finalizers.Delete(finalizerName)
	source.Finalizers = finalizers.List()

	r.updateFinalizers(ctx, source)
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

// TODO(nachtmaar) eventtype.go with pkg/controller runtime
func (r *Reconciler) reconcileEventTypes(ctx context.Context, src *sourcesv1alpha1.GitHubSource) error {
	current, err := r.getEventTypes(ctx, src)
	if err != nil {
		logging.FromContext(ctx).Error("Unable to get existing event types", zap.Error(err))
		return err
	}

	expected, err := r.makeEventTypes(src)
	if err != nil {
		return err
	}

	toCreate, toDelete := r.computeDiff(current, expected)

	for _, eventType := range toDelete {
		if err = r.EventingClientSet.EventingV1alpha1().EventTypes(src.Namespace).Delete(eventType.Name, &metav1.DeleteOptions{}); err != nil {
			logging.FromContext(ctx).Error("Error deleting eventType", zap.Any("eventType", eventType))
			return err
		}
	}

	for _, eventType := range toCreate {
		if _, err = r.EventingClientSet.EventingV1alpha1().EventTypes(src.Namespace).Create(&eventType); err != nil {
			logging.FromContext(ctx).Error("Error creating eventType", zap.Any("eventType", eventType))
			return err
		}
	}

	return nil
}

func (r *Reconciler) getEventTypes(ctx context.Context, src *sourcesv1alpha1.GitHubSource) ([]eventingv1alpha1.EventType, error) {
	etl, err := r.EventingClientSet.EventingV1alpha1().EventTypes(src.Namespace).List(metav1.ListOptions{
		LabelSelector: r.getLabelSelector(src).String(),
	})
	if err != nil {
		logging.FromContext(ctx).Error("Unable to list event types: %v", zap.Error(err))
		return nil, err
	}
	eventTypes := make([]eventingv1alpha1.EventType, 0)
	for _, et := range etl.Items {
		if metav1.IsControlledBy(&et, src) {
			eventTypes = append(eventTypes, et)
		}
	}
	return eventTypes, nil
}

func (r *Reconciler) makeEventTypes(src *sourcesv1alpha1.GitHubSource) ([]eventingv1alpha1.EventType, error) {
	eventTypes := make([]eventingv1alpha1.EventType, 0)

	// Only create EventTypes for Broker sinks.
	// We add this check here in case the GitHubSource was changed from Broker to non-Broker sink.
	// If so, we need to delete the existing ones, thus we return empty expected.
	if src.Spec.Sink.Kind != "Broker" {
		return eventTypes, nil
	}

	args := &resources.EventTypeArgs{
		Src: src,
		// TODO(nachtmaar)
		Source: "todo", // r.source,
	}
	for _, et := range src.Spec.EventTypes {
		githubEventType := sourcesv1alpha1.GitHubEventType(et)
		args.Type = githubEventType
		eventType := resources.MakeEventType(args)
		eventTypes = append(eventTypes, eventType)
	}
	return eventTypes, nil
}

func (r *Reconciler) updateFinalizers(ctx context.Context, source *sourcesv1alpha1.GitHubSource) *sourcesv1alpha1.GitHubSource {
	// TODO: use events rather than logger
	logger := logging.FromContext(ctx).Desugar()

	githubSource, err := r.githubsourceInformer.Lister().GitHubSources(source.Namespace).Get(source.Name)
	if err != nil {
		logger.Error("could not get GitHubSource", zap.Error(err))
		return nil
	}

	// If there's nothing to update, just return.
	if reflect.DeepEqual(githubSource.Status, source.Status) {
		return nil
	}

	// Don't modify the informers copy.
	existing := githubSource.DeepCopy()
	existing.Finalizers = source.Finalizers

	obj, err := r.githubSourcesClientSet.SourcesV1alpha1().GitHubSources(source.Namespace).Update(existing)
	logger.Error("could not set finalizers for GitHubSource", zap.Error(err))
	return obj
}

// TODO(nachtmaar) use on all status updates ?
func (r *Reconciler) updateStatus(ctx context.Context, src *sourcesv1alpha1.GitHubSource) (*sourcesv1alpha1.GitHubSource, error) {
	githubSource, err := r.githubsourceInformer.Lister().GitHubSources(src.Namespace).Get(src.Name)
	if err != nil {
		return nil, err
	}

	// If there's nothing to update, just return.
	if reflect.DeepEqual(githubSource.Status, src.Status) {
		return githubSource, nil
	}

	// Don't modify the informers copy.
	existing := githubSource.DeepCopy()
	existing.Status = src.Status

	return r.githubSourcesClientSet.SourcesV1alpha1().GitHubSources(src.Namespace).UpdateStatus(existing)
}

func (r *Reconciler) computeDiff(current []eventingv1alpha1.EventType, expected []eventingv1alpha1.EventType) ([]eventingv1alpha1.EventType, []eventingv1alpha1.EventType) {
	toCreate := make([]eventingv1alpha1.EventType, 0)
	toDelete := make([]eventingv1alpha1.EventType, 0)
	currentMap := asMap(current, keyFromEventType)
	expectedMap := asMap(expected, keyFromEventType)

	// Iterate over the slices instead of the maps for predictable UT expectations.
	for _, e := range expected {
		if c, ok := currentMap[keyFromEventType(&e)]; !ok {
			toCreate = append(toCreate, e)
		} else {
			if !equality.Semantic.DeepEqual(e.Spec, c.Spec) {
				toDelete = append(toDelete, c)
				toCreate = append(toCreate, e)
			}
		}
	}
	// Need to check whether the current EventTypes are not in the expected map. If so, we have to delete them.
	// This could happen if the GithubSource CO changes its broker.
	for _, c := range current {
		if _, ok := expectedMap[keyFromEventType(&c)]; !ok {
			toDelete = append(toDelete, c)
		}
	}
	return toCreate, toDelete
}

func asMap(eventTypes []eventingv1alpha1.EventType, keyFunc func(*eventingv1alpha1.EventType) string) map[string]eventingv1alpha1.EventType {
	eventTypesAsMap := make(map[string]eventingv1alpha1.EventType, 0)
	for _, eventType := range eventTypes {
		key := keyFunc(&eventType)
		eventTypesAsMap[key] = eventType
	}
	return eventTypesAsMap
}

func keyFromEventType(eventType *eventingv1alpha1.EventType) string {
	return fmt.Sprintf("%s_%s_%s_%s", eventType.Spec.Type, eventType.Spec.Source, eventType.Spec.Schema, eventType.Spec.Broker)
}

func (r *Reconciler) getLabelSelector(src *sourcesv1alpha1.GitHubSource) labels.Selector {
	return labels.SelectorFromSet(resources.Labels(src.Name))
}
