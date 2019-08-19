package common

import (
	mf "github.com/jcrossley3/manifestival"
	servingv1alpha1 "github.com/openshift-knative/knative-serving-operator/pkg/apis/serving/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var log = logf.Log.WithName("common")

type Platforms []func(client.Client, *runtime.Scheme, *mf.Manifest) (*Extension, error)
type Extender func(*servingv1alpha1.KnativeServing) error
type Extensions []Extension
type Extension struct {
	Transformers []mf.Transformer
	PreInstalls  []Extender
	PostInstalls []Extender
}

func (platforms Platforms) Extend(c client.Client, scheme *runtime.Scheme, manifest *mf.Manifest) (result Extensions, err error) {
	for _, fn := range platforms {
		ext, err := fn(c, scheme, manifest)
		if err != nil {
			return result, err
		}
		if ext != nil {
			result = append(result, *ext)
		}
	}
	return
}

func (exts Extensions) Transform(instance *servingv1alpha1.KnativeServing) []mf.Transformer {
	result := []mf.Transformer{
		mf.InjectOwner(instance),
		mf.InjectNamespace(instance.GetNamespace()),
	}
	for _, extension := range exts {
		result = append(result, extension.Transformers...)
	}
	// Let any config in instance override everything else
	return append(result, func(u *unstructured.Unstructured) error {
		if u.GetKind() == "ConfigMap" {
			if data, ok := instance.Spec.Config[u.GetName()[len(`config-`):]]; ok {
				UpdateConfigMap(u, data, log)
			}
		}
		return nil
	})
}

func (exts Extensions) PreInstall(instance *servingv1alpha1.KnativeServing) error {
	for _, extension := range exts {
		for _, f := range extension.PreInstalls {
			if err := f(instance); err != nil {
				return err
			}
		}
	}
	return nil
}

func (exts Extensions) PostInstall(instance *servingv1alpha1.KnativeServing) error {
	for _, extension := range exts {
		for _, f := range extension.PostInstalls {
			if err := f(instance); err != nil {
				return err
			}
		}
	}
	return nil
}
