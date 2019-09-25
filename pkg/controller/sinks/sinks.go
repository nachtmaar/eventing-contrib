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

package sinks

import (
	"context"
	"fmt"
	"k8s.io/client-go/dynamic"
	eventingduck "knative.dev/eventing/pkg/duck"

	corev1 "k8s.io/api/core/v1"
	"knative.dev/pkg/apis/duck"
	duckv1alpha1 "knative.dev/pkg/apis/duck/v1alpha1"
)

// GetSinkURI retrieves the sink URI from the object referenced by the given
// ObjectReference.
func GetSinkURI(ctx context.Context, dynamicClientSet dynamic.Interface, sink *corev1.ObjectReference, namespace string) (string, error) {
	if sink == nil {
		return "", fmt.Errorf("sink ref is nil")
	}

	u, err := eventingduck.ObjectReference(ctx, dynamicClientSet, namespace, sink)

	t := duckv1alpha1.AddressableType{}
	t.SetGroupVersionKind(sink.GroupVersionKind())
	err = duck.FromUnstructured(u, &t)
	objIdentifier := fmt.Sprintf("\"%s/%s\" (%s)", t.GetNamespace(), t.GetName(), t.GroupVersionKind())
	if err != nil {
		return "", fmt.Errorf("failed to deserialize sink %s: %v", objIdentifier, err)
	}
	// Special case v1/Service to allow it be addressable
	if t.GroupVersionKind().Kind == "Service" && t.GroupVersionKind().Version == "v1" {
		return fmt.Sprintf("http://%s.%s.svc/", t.GetName(), t.GetNamespace()), nil
	}

	if t.Status.Address == nil {
		return "", fmt.Errorf("sink %s does not contain address", objIdentifier)
	}

	url := t.Status.Address.GetURL()
	if url.Host == "" {
		return "", fmt.Errorf("sink %s contains an empty hostname", objIdentifier)
	}
	return url.String(), nil
}
