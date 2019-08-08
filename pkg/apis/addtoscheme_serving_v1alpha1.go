package apis

import (
	"github.com/openshift-knative/knative-serving-operator/pkg/apis/serving/v1alpha1"
	"k8s.io/kube-aggregator/pkg/apis/apiregistration/v1beta1"
)

func init() {
	// Register the types with the Scheme so the components can map objects to GroupVersionKinds and back
	AddToSchemes = append(AddToSchemes, v1alpha1.SchemeBuilder.AddToScheme, v1beta1.SchemeBuilder.AddToScheme)
}
