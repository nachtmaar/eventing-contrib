package resources

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"knative.dev/eventing-contrib/github/pkg/apis/sources/v1alpha1"
	eventingv1alpha1 "knative.dev/eventing/pkg/apis/eventing/v1alpha1"
	"knative.dev/eventing/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EventTypeArgs are the arguments needed to create an EventType for an GitHubSource.
type EventTypeArgs struct {
	Src    *v1alpha1.GitHubSource
	Type   string
	Source string
}

// MakeEventType creates the in-memory representation of the EventType for the specified GitHubSource.
func MakeEventType(args *EventTypeArgs) eventingv1alpha1.EventType {
	return eventingv1alpha1.EventType{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GenerateFixedName(args.Src, utils.ToDNS1123Subdomain(args.Type)),
			Labels:    Labels(args.Src.Name),
			Namespace: args.Src.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(args.Src, schema.GroupVersionKind{
					Group:   v1alpha1.SchemeGroupVersion.Group,
					Version: v1alpha1.SchemeGroupVersion.Version,
					Kind:    "GitHubSource",
				}),
			},
		},
		Spec: eventingv1alpha1.EventTypeSpec{
			Type:   args.Type,
			Source: args.Source,
			Broker: args.Src.Spec.Sink.Name,
		},
	}
}
