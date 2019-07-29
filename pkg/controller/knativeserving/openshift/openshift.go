package openshift

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	servingv1alpha1 "github.com/openshift-knative/knative-serving-operator/pkg/apis/serving/v1alpha1"
	"github.com/openshift-knative/knative-serving-operator/pkg/controller/knativeserving/common"
	configv1 "github.com/openshift/api/config/v1"

	mf "github.com/jcrossley3/manifestival"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

const (
	maistraOperatorNamespace     = "istio-operator"
	maistraControlPlaneNamespace = "istio-system"
	caBundleConfigMapName        = "config-service-ca"

	// The SA are added to priviledged for injection of istio-proxy https://maistra.io/docs/getting_started/application-requirements/
	// Relaxing security constraints is only necessary during the OpenShift Service Mesh Technology Preview phase (as per the docs).
	serviceAccountName = "system:serviceaccount:knative-serving:controller"
	// The SA are added to anyuid user to give permission to cluster-local-gateway
	saNameForClusterLocalGateway = "system:serviceaccount:istio-system:cluster-local-gateway-service-account"

	// The secret in which the tls certificate for the autoscaler will be written.
	autoscalerTlsSecretName = "autoscaler-adapter-tls"
)

var (
	sccNames = map[string]string{
		"privileged": serviceAccountName,
		"anyuid":     saNameForClusterLocalGateway,
	}
	extension = common.Extension{
		Transformers: []mf.Transformer{ingress, egress, deploymentController, annotateAutoscalerService, augmentAutoscalerDeployment},
		PreInstalls:  []common.Extender{addUsersToSCCs, ensureMaistra, caBundleConfigMap},
		PostInstalls: []common.Extender{ensureOpenshiftIngress, installServiceMonitor},
	}
	log    = logf.Log.WithName("openshift")
	api    client.Client
	scheme *runtime.Scheme
)

// Configure OpenShift if we're soaking in it
func Configure(c client.Client, s *runtime.Scheme) (*common.Extension, error) {
	if routeExists, err := anyKindExists(c, "", schema.GroupVersionKind{"route.openshift.io", "v1", "route"}); err != nil {
		return nil, err
	} else if !routeExists {
		// Not running in OpenShift
		return nil, nil
	}

	// Register scheme
	if err := configv1.Install(s); err != nil {
		log.Error(err, "Unable to register scheme")
		return nil, err
	}

	api = c
	scheme = s
	return &extension, nil
}

// ensureMaistra ensures Maistra is installed in the cluster
func ensureMaistra(instance *servingv1alpha1.KnativeServing) error {
	namespace := instance.GetNamespace()

	log.Info("Ensuring Istio is installed in OpenShift")

	if operatorExists, err := maistraOperatorExists(namespace); err != nil {
		return err
	} else if !operatorExists {
		if istioExists, err := istioExists(namespace); err != nil {
			return err
		} else if istioExists {
			log.Info("Maistra Operator not present but Istio CRDs already installed - assuming Istio is already setup")
			return nil
		}
		// Maistra not installed
		if err := installMaistra(api); err != nil {
			return err
		}
	} else {
		log.Info("Maistra already installed")
	}

	return nil
}

func maistraOperatorExists(namespace string) (bool, error) {
	return anyKindExists(api, namespace,
		// Maistra >0.10
		schema.GroupVersionKind{Group: "maistra.io", Version: "v1", Kind: "servicemeshcontrolplane"},
		// Maistra 0.10
		schema.GroupVersionKind{Group: "istio.openshift.com", Version: "v1alpha3", Kind: "controlplane"},
		// Maistra <0.10
		schema.GroupVersionKind{Group: "istio.openshift.com", Version: "v1alpha1", Kind: "installation"},
	)
}

func istioExists(namespace string) (bool, error) {
	return anyKindExists(api, namespace,
		schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1alpha3", Kind: "virtualservice"},
	)
}

func serviceMonitorExists(namespace string) (bool, error) {
	return anyKindExists(api, namespace,
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "servicemonitor"},
	)
}

// ensureOpenshiftIngress ensures knative-openshift-ingress operator is installed
func ensureOpenshiftIngress(instance *servingv1alpha1.KnativeServing) error {
	namespace := instance.GetNamespace()
	const path = "deploy/resources/openshift-ingress/openshift-ingress-0.0.6.yaml"
	log.Info("Ensuring Knative OpenShift Ingress operator is installed")
	if manifest, err := mf.NewManifest(path, false, api); err == nil {
		transforms := []mf.Transformer{mf.InjectOwner(instance)}
		if len(namespace) > 0 {
			transforms = append(transforms, mf.InjectNamespace(namespace))
		}
		if err = manifest.Transform(transforms...); err == nil {
			err = manifest.ApplyAll()
		}
		if err != nil {
			log.Error(err, "Unable to install Maistra operator")
			return err
		}
	} else {
		log.Error(err, "Unable to create Knative OpenShift Ingress operator install manifest")
		return err
	}
	return nil
}

func installServiceMonitor(instance *servingv1alpha1.KnativeServing) error {
	namespace := instance.GetNamespace()
	log.Info("Installing ServiceMonitor")
	const path = "deploy/resources/monitoring/service_monitor.yaml"

	if serviceMonitorExists, err := serviceMonitorExists(namespace); err != nil {
		return err
	} else if !serviceMonitorExists {
		log.Info("ServiceMonitor CRD is not installed. Skip to install ServiceMonitor")
		return nil
	}

	// Add label openshift.io/cluster-monitoring to namespace
	ns := &corev1.Namespace{}
	if err := api.Get(context.TODO(), client.ObjectKey{Name: namespace}, ns); err != nil {
		return err
	}

	const monitoringLabel = "openshift.io/cluster-monitoring"
	ns.Labels[monitoringLabel] = "true"
	if err := api.Update(context.TODO(), ns); err != nil {
		log.Error(err, fmt.Sprintf("Could not add label %q to namespace %q", monitoringLabel, namespace))
		return err
	}

	// Install ServiceMonitor
	manifest, err := mf.NewManifest(path, false, api)
	if err != nil {
		log.Error(err, "Unable to create ServiceMonitor install manifest")
		return err
	}
	transforms := []mf.Transformer{mf.InjectOwner(instance)}
	if len(namespace) > 0 {
		transforms = append(transforms, mf.InjectNamespace(namespace))
	}
	if err := manifest.Transform(transforms...); err != nil {
		log.Error(err, "Unable to transform service monitor manifest")
		return err
	}
	if err := manifest.ApplyAll(); err != nil {
		log.Error(err, "Unable to install ServiceMonitor")
		return err
	}
	return nil
}

func installMaistra(c client.Client) error {
	if err := installMaistraOperator(api); err != nil {
		return err
	}
	if err := installMaistraControlPlane(api); err != nil {
		return err
	}
	return nil
}

func installMaistraOperator(c client.Client) error {
	const path = "deploy/resources/maistra/maistra-operator-0.10.yaml"
	log.Info("Installing Maistra operator")
	if manifest, err := mf.NewManifest(path, false, c); err == nil {
		if err = ensureNamespace(c, maistraOperatorNamespace); err != nil {
			log.Error(err, "Unable to create Maistra operator namespace", "namespace", maistraOperatorNamespace)
			return err
		}
		if err = manifest.Transform(mf.InjectNamespace(maistraOperatorNamespace)); err == nil {
			err = manifest.ApplyAll()
		}
		if err != nil {
			log.Error(err, "Unable to install Maistra operator")
			return err
		}
	} else {
		log.Error(err, "Unable to create Maistra operator install manifest")
		return err
	}
	return nil
}

func installMaistraControlPlane(c client.Client) error {
	const path = "deploy/resources/maistra/maistra-controlplane-0.10.0.yaml"
	log.Info("Installing Maistra ControlPlane")
	if manifest, err := mf.NewManifest(path, false, c); err == nil {
		if err = ensureNamespace(c, maistraControlPlaneNamespace); err != nil {
			log.Error(err, "Unable to create Maistra ControlPlane namespace", "namespace", maistraControlPlaneNamespace)
			return err
		}
		if err = manifest.Transform(mf.InjectNamespace(maistraControlPlaneNamespace)); err == nil {
			err = manifest.ApplyAll()
		}
		if err != nil {
			log.Error(err, "Unable to install Maistra ControlPlane")
			return err
		}
	} else {
		log.Error(err, "Unable to create Maistra ControlPlane manifest")
		return err
	}
	return nil
}

func ingress(u *unstructured.Unstructured) error {
	if u.GetKind() == "ConfigMap" && u.GetName() == "config-domain" {
		ingressConfig := &configv1.Ingress{}
		if err := api.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, ingressConfig); err != nil {
			if !meta.IsNoMatchError(err) {
				return err
			}
			return nil
		}
		domain := ingressConfig.Spec.Domain
		if len(domain) > 0 {
			data := map[string]string{domain: ""}
			common.UpdateConfigMap(u, data, log)
		}
	}
	return nil
}

func egress(u *unstructured.Unstructured) error {
	if u.GetKind() == "ConfigMap" && u.GetName() == "config-network" {
		networkConfig := &configv1.Network{}
		if err := api.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, networkConfig); err != nil {
			if !meta.IsNoMatchError(err) {
				return err
			}
			return nil
		}
		network := strings.Join(networkConfig.Spec.ServiceNetwork, ",")
		if len(network) > 0 {
			data := map[string]string{"istio.sidecar.includeOutboundIPRanges": network}
			common.UpdateConfigMap(u, data, log)
		}
	}
	return nil
}

func addUsersToSCCs(instance *servingv1alpha1.KnativeServing) error {
	for sccName, user := range sccNames {
		if err := addToSCC(sccName, user); err != nil {
			return err
		}
	}
	return nil
}

func addToSCC(sccName, user string) error {
	scc := &unstructured.Unstructured{}
	scc.SetAPIVersion("security.openshift.io/v1")
	scc.SetKind("SecurityContextConstraints")

	err := api.Get(context.TODO(), client.ObjectKey{Name: sccName}, scc)
	if err != nil {
		return err
	}

	// Verify if SA has already been assigned to the SCC
	existing, exists, _ := unstructured.NestedStringSlice(scc.UnstructuredContent(), "users")
	if exists {
		for _, e := range existing {
			if e == user {
				return nil
			}
		}
		existing = append(existing, user)
	}

	err = unstructured.SetNestedStringSlice(scc.UnstructuredContent(), existing, "users")
	if err != nil {
		return err
	}
	err = api.Update(context.TODO(), scc)
	if err != nil {
		return err
	}
	log.Info(fmt.Sprintf("Added ServiceAccount %q to SecurityContextConstraints %q", user, sccName))
	return nil
}

func deploymentController(u *unstructured.Unstructured) error {
	const volumeName = "service-ca"
	if u.GetKind() == "Deployment" && u.GetName() == "controller" {

		deploy := &appsv1.Deployment{}
		if err := scheme.Convert(u, deploy, nil); err != nil {
			return err
		}

		volumes := deploy.Spec.Template.Spec.Volumes
		for _, v := range volumes {
			if v.Name == volumeName {
				return nil
			}
		}
		deploy.Spec.Template.Spec.Volumes = append(volumes, v1.Volume{
			Name: volumeName,
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: caBundleConfigMapName,
					},
				},
			},
		})

		containers := deploy.Spec.Template.Spec.Containers
		containers[0].VolumeMounts = append(containers[0].VolumeMounts, v1.VolumeMount{
			Name:      volumeName,
			MountPath: "/var/run/secrets/kubernetes.io/servicecerts",
		})
		containers[0].Env = append(containers[0].Env, v1.EnvVar{
			Name:  "SSL_CERT_FILE",
			Value: "/var/run/secrets/kubernetes.io/servicecerts/service-ca.crt",
		})
		if err := scheme.Convert(deploy, u, nil); err != nil {
			return err
		}
	}
	return nil
}

func caBundleConfigMap(instance *servingv1alpha1.KnativeServing) error {
	cm := &v1.ConfigMap{}
	if err := api.Get(context.TODO(), types.NamespacedName{Name: caBundleConfigMapName, Namespace: instance.GetNamespace()}, cm); err != nil {
		if errors.IsNotFound(err) {
			// Define a new configmap
			cm.Name = caBundleConfigMapName
			cm.Annotations = make(map[string]string)
			cm.Annotations["service.alpha.openshift.io/inject-cabundle"] = "true"
			cm.Namespace = instance.GetNamespace()
			cm.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(instance, instance.GroupVersionKind())})
			err = api.Create(context.TODO(), cm)
			if err != nil {
				return err
			}
			// ConfigMap created successfully
			return nil
		}
		return err
	}

	return nil
}

// anyKindExists returns true if any of the gvks (GroupVersionKind) exist
func anyKindExists(c client.Client, namespace string, gvks ...schema.GroupVersionKind) (bool, error) {
	for _, gvk := range gvks {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)
		if err := c.List(context.TODO(), &client.ListOptions{Namespace: namespace}, list); err != nil {
			if !meta.IsNoMatchError(err) {
				return false, err
			}
		} else {
			log.Info("Detected", "gvk", gvk.String())
			return true, nil
		}
	}
	return false, nil
}

func itemsExist(c client.Client, kind string, apiVersion string, namespace string) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetKind(kind)
	list.SetAPIVersion(apiVersion)
	if err := c.List(context.TODO(), &client.ListOptions{Namespace: namespace}, list); err != nil {
		return false, err
	}
	return len(list.Items) > 0, nil
}

func ensureNamespace(c client.Client, ns string) error {
	namespace := &v1.Namespace{}
	namespace.Name = ns
	if err := c.Create(context.TODO(), namespace); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

// annotateAutoscalerService annotates the autoscaler service with an Openshift annotation
// that causes it to generate a certificate for the cluster to use internally.
// Adapted from: https://docs.openshift.com/container-platform/4.1/monitoring/exposing-custom-application-metrics-for-autoscaling.html
func annotateAutoscalerService(u *unstructured.Unstructured) error {
	const annotationKey = "service.alpha.openshift.io/serving-cert-secret-name"
	if u.GetKind() == "Service" && u.GetName() == "autoscaler" {
		annotations := u.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[annotationKey] = autoscalerTlsSecretName
		u.SetAnnotations(annotations)
	}
	return nil
}

// augmentAutoscalerDeployment mounts the secret generated by 'annotateAutoscalerService' into
// the autoscaler deployment and makes sure the custom-metrics API uses the mounted certs properly.
func augmentAutoscalerDeployment(u *unstructured.Unstructured) error {
	const volumeName = "volume-serving-cert"
	const mountPath = "/var/run/serving-cert"
	if u.GetKind() == "Deployment" && u.GetName() == "autoscaler" {
		deploy := &appsv1.Deployment{}
		if err := scheme.Convert(u, deploy, nil); err != nil {
			return err
		}

		volumes := deploy.Spec.Template.Spec.Volumes
		// Skip it all if the volume already exists.
		for _, v := range volumes {
			if v.Name == volumeName {
				return nil
			}
		}
		deploy.Spec.Template.Spec.Volumes = append(volumes, v1.Volume{
			Name: volumeName,
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					SecretName: autoscalerTlsSecretName,
				},
			},
		})

		// Mount the volume into the first (and only) container.
		container := &deploy.Spec.Template.Spec.Containers[0]
		container.VolumeMounts = append(container.VolumeMounts, v1.VolumeMount{
			Name:      volumeName,
			MountPath: mountPath,
			ReadOnly:  true,
		})

		// Add the respective parameters to the command to pick the certificate + key up.
		certFile := filepath.Join(mountPath, "tls.crt")
		keyFile := filepath.Join(mountPath, "tls.key")
		container.Args = []string{"--secure-port=8443", "--tls-cert-file=" + certFile, "--tls-private-key-file=" + keyFile}
		if err := scheme.Convert(deploy, u, nil); err != nil {
			return err
		}
	}
	return nil
}
