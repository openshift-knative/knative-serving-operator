package openshift

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	servingv1alpha1 "github.com/openshift-knative/knative-serving-operator/pkg/apis/serving/v1alpha1"
	"github.com/openshift-knative/knative-serving-operator/pkg/controller/knativeserving/common"
	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"

	"github.com/coreos/go-semver/semver"
	mf "github.com/jcrossley3/manifestival"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	apiregistrationv1beta1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

const (
	caBundleConfigMapName = "config-service-ca"

	// The secret in which the tls certificate for the autoscaler will be written.
	autoscalerTlsSecretName = "autoscaler-adapter-tls"
	// knativeServingInstalledNamespace is the ns where knative serving operator has been installed
	knativeServingInstalledNamespace = "NAMESPACE"
	// service monitor created successfully when monitoringLabel added to namespace
	monitoringLabel = "openshift.io/cluster-monitoring"
	// revision log URL template
	revisionlogUrlTemplate = "logging.revision-url-template"
	// openshift logging namespace
	openshiftLoggingNamespace = "openshift-logging"
	// logging visualization
	loggingVisualization = "kibana"
)

var (
	extension = common.Extension{
		Transformers: []mf.Transformer{ingress, egress, deploymentController, annotateAutoscalerService, augmentAutoscalerDeployment, addCaBundleToApiservice, configureLogURLTemplate},
		PreInstalls:  []common.Extender{checkVersion, installNetworkPolicies, caBundleConfigMap},
		PostInstalls: []common.Extender{installServiceMonitor},
	}
	log              = logf.Log.WithName("openshift")
	api              client.Client
	scheme           *runtime.Scheme
	requestAcceptors = []common.RequestAcceptor{acceptClusterLoggingRequests}
)

// Configure OpenShift if we're soaking in it
func Configure(c client.Client, s *runtime.Scheme, manifest *mf.Manifest) (*common.Extension, error) {

	inOpenShift, err := isOpenShift(c)
	if err != nil {
		return nil, err
	}

	if !inOpenShift {
		return nil, nil
	}

	if err := registerSchemes(s); err != nil {
		return nil, err
	}

	var filtered []unstructured.Unstructured
	for _, u := range manifest.Resources {
		if u.GetKind() == "APIService" && u.GetName() == "v1beta1.custom.metrics.k8s.io" {
			log.Info("Dropping APIService for v1beta1.custom.metrics.k8s.io")
			continue
		}
		filtered = append(filtered, u)
	}
	manifest.Resources = filtered

	api = c
	return &extension, nil
}

// BuildRequestAcceptors configures request acceptors for openshift platform
func BuildRequestAcceptors(c client.Client, s *runtime.Scheme) ([]common.RequestAcceptor, error) {

	inOpenShift, err := isOpenShift(c)
	if err != nil {
		return nil, err
	}

	if !inOpenShift {
		return nil, nil
	}

	// register schemes so that acceptor can use OpenShift specific APIs
	if err := registerSchemes(s); err != nil {
		return nil, err
	}

	return requestAcceptors, nil
}

// Returns true if we are running in OpenShift
func isOpenShift(c client.Client) (bool, error) {
	routeExists, err := anyKindExists(c, "", schema.GroupVersionKind{"route.openshift.io", "v1", "route"})
	if err != nil {
		return false, err
	}
	return routeExists, nil
}

func registerSchemes(s *runtime.Scheme) error {

	// scheme has been registered already
	if scheme != nil {
		return nil
	}

	// Register config v1 scheme
	if err := configv1.Install(s); err != nil {
		log.Error(err, "Unable to register configv1 scheme")
		return err
	}

	// Register route v1 scheme
	if err := routev1.Install(s); err != nil {
		log.Error(err, "Unable to register routev1 scheme")
		return err
	}

	if err := apiregistrationv1beta1.AddToScheme(s); err != nil {
		log.Error(err, "Unable to register apiservice scheme")
		return err
	}

	scheme = s
	return nil
}

func checkVersion(instance *servingv1alpha1.KnativeServing) error {
	minVersion := semver.New("4.1.13")

	clusterVersion := &configv1.ClusterVersion{}
	if err := api.Get(context.TODO(), client.ObjectKey{Name: "version"}, clusterVersion); err != nil {
		return err
	}

	current, err := semver.NewVersion(clusterVersion.Status.Desired.Version)
	if err != nil {
		log.Error(err, "could not parse version string")
		return err
	}

	if strings.Contains(string(current.PreRelease), "ci") ||
		strings.Contains(string(current.PreRelease), "nightly") {
		log.Info("CI/Nightly version detected, bypassing version check")
		return nil
	}

	if current.LessThan(*minVersion) {
		msg := fmt.Sprintf("version constraint not fulfilled: minimum version: %s, current version: %s", minVersion.String(), current.String())
		instance.Status.MarkDependencyMissing(msg)
		log.Error(errors.New(msg), msg)
		return nil
	}
	log.Info("version constraint fulfilled", "version", current.String())
	return nil
}

func serviceMonitorExists(namespace string) (bool, error) {
	return anyKindExists(api, namespace,
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "servicemonitor"},
	)
}

func installServiceMonitor(instance *servingv1alpha1.KnativeServing) error {
	const (
		path         = "deploy/resources/monitoring/service_monitor.yaml"
		operatorPath = "deploy/resources/monitoring/operator_service_monitor.yaml"
		rolePath     = "deploy/resources/monitoring/role_service_monitor.yaml"
	)
	namespace := instance.GetNamespace()
	log.Info("Installing ServiceMonitor")
	if err := createServiceMonitor(instance, namespace, path); err != nil {
		return err
	}
	log.Info("Installing role and roleBinding")
	if err := createRoleAndRoleBinding(instance, namespace, rolePath); err != nil {
		return err
	}
	// getOperatorNamespace return namespace where knative-serving-operator has been installed
	operatorNamespace, err := getOperatorNamespace()
	if err != nil {
		log.Info("no namespace defined, skipping ServiceMonitor installation for the operator")
		return nil
	}
	log.Info("Installing ServiceMonitor for Operator")
	if err := createServiceMonitor(instance, operatorNamespace, operatorPath); err != nil {
		return err
	}
	log.Info("Installing role and roleBinding for Operator")
	if err := createRoleAndRoleBinding(instance, operatorNamespace, rolePath); err != nil {
		return err
	}
	return nil
}

// addCaBundleToApiservice adds service.alpha.openshift.io/inject-cabundle annotation and
// set insecureSkipTLSVerify to be false.
func addCaBundleToApiservice(u *unstructured.Unstructured) error {
	if u.GetKind() == "APIService" && u.GetName() == "v1beta1.custom.metrics.k8s.io" {
		apiService := &apiregistrationv1beta1.APIService{}
		if err := scheme.Convert(u, apiService, nil); err != nil {
			return err
		}

		apiService.Spec.InsecureSkipTLSVerify = false
		if apiService.ObjectMeta.Annotations == nil {
			apiService.ObjectMeta.Annotations = make(map[string]string)
		}
		apiService.ObjectMeta.Annotations["service.alpha.openshift.io/inject-cabundle"] = "true"
		if err := scheme.Convert(apiService, u, nil); err != nil {
			return err
		}
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
		if apierrors.IsNotFound(err) {
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

// Update logging URL template for Knative service's revision with concrete kibana hostname if cluster logging has been installed
func configureLogURLTemplate(u *unstructured.Unstructured) error {
	if u.GetKind() == "ConfigMap" && u.GetName() == "config-observability" {
		// attempt to locate kibana route which is available if openshift-logging has been configured
		route := &routev1.Route{}
		if err := api.Get(context.TODO(), types.NamespacedName{Name: loggingVisualization, Namespace: openshiftLoggingNamespace}, route); err != nil {
			common.UpdateConfigMap(u, map[string]string{revisionlogUrlTemplate: ""}, log)
			return nil
		}
		// retrieve host from kibana route, construct a concrete logUrl template with actual host name, update config-observability
		if len(route.Status.Ingress) > 0 {
			host := route.Status.Ingress[0].Host
			if host != "" {
				url := "https://" + host + "/app/kibana#/discover?_a=(index:.all,query:'kubernetes.labels.serving_knative_dev%5C%2FrevisionUID:${REVISION_UID}')"
				data := map[string]string{revisionlogUrlTemplate: url}
				common.UpdateConfigMap(u, data, log)
			}
		}
	}
	return nil
}

func installNetworkPolicies(instance *servingv1alpha1.KnativeServing) error {
	namespace := instance.GetNamespace()
	log.Info("Installing Network Policies")
	const path = "deploy/resources/network/network_policies.yaml"

	manifest, err := mf.NewManifest(path, false, api)
	if err != nil {
		log.Error(err, "Unable to create Network Policy install manifest")
		return err
	}
	transforms := []mf.Transformer{mf.InjectOwner(instance)}
	if len(namespace) > 0 {
		transforms = append(transforms, mf.InjectNamespace(namespace))
	}
	if err := manifest.Transform(transforms...); err != nil {
		log.Error(err, "Unable to transform network policy manifest")
		return err
	}
	if err := manifest.ApplyAll(); err != nil {
		log.Error(err, "Unable to install Network Policies")
		return err
	}
	return nil
}

func getOperatorNamespace() (string, error) {
	ns, found := os.LookupEnv(knativeServingInstalledNamespace)
	if !found {
		return "", fmt.Errorf("%s must be set", knativeServingInstalledNamespace)
	}
	return ns, nil
}

func addMonitoringLabelToNamespace(namespace string) error {
	ns := &v1.Namespace{}
	if err := api.Get(context.TODO(), client.ObjectKey{Name: namespace}, ns); err != nil {
		return err
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels[monitoringLabel] = "true"
	if err := api.Update(context.TODO(), ns); err != nil {
		log.Error(err, fmt.Sprintf("could not add label %q to namespace %q", monitoringLabel, namespace))
		return err
	}
	return nil
}

func createServiceMonitor(instance *servingv1alpha1.KnativeServing, namespace, path string) error {
	if serviceMonitorExists, err := serviceMonitorExists(namespace); err != nil {
		return err
	} else if !serviceMonitorExists {
		log.Info("ServiceMonitor CRD is not installed. Skip to install ServiceMonitor")
		return nil
	}
	// Add label openshift.io/cluster-monitoring to namespace
	if err := addMonitoringLabelToNamespace(namespace); err != nil {
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
		log.Error(err, "Unable to transform serviceMonitor manifest")
		return err
	}
	if err := manifest.ApplyAll(); err != nil {
		log.Error(err, "Unable to install ServiceMonitor")
		return err
	}
	return nil
}

func createRoleAndRoleBinding(instance *servingv1alpha1.KnativeServing, namespace, path string) error {
	manifest, err := mf.NewManifest(path, false, api)
	if err != nil {
		log.Error(err, "Unable to create role and roleBinding ServiceMonitor install manifest")
		return err
	}
	transforms := []mf.Transformer{mf.InjectOwner(instance)}
	if len(namespace) > 0 {
		transforms = append(transforms, mf.InjectNamespace(namespace))
	}
	if err := manifest.Transform(transforms...); err != nil {
		log.Error(err, "Unable to transform role and roleBinding serviceMonitor manifest")
		return err
	}
	if err := manifest.ApplyAll(); err != nil {
		log.Error(err, "Unable to create role and roleBinding for ServiceMonitor")
		return err
	}
	return nil
}

func acceptClusterLoggingRequests(a handler.MapObject) bool {
	return a.Meta.GetNamespace() == openshiftLoggingNamespace && a.Meta.GetName() == loggingVisualization
}
