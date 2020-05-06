// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manifest

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ghodss/yaml"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp" // For kubeclient GCP auth
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	kubectlutil "k8s.io/kubectl/pkg/util/deployment"

	"istio.io/operator/pkg/apis/istio/v1alpha2"
	"istio.io/operator/pkg/kubectlcmd"
	"istio.io/operator/pkg/name"
	"istio.io/operator/pkg/object"
	"istio.io/operator/pkg/util"
	"istio.io/operator/pkg/version"
	"istio.io/pkg/log"
)

const (
	// cRDPollInterval is how often the state of CRDs is polled when waiting for their creation.
	cRDPollInterval = 500 * time.Millisecond
	// cRDPollTimeout is the maximum wait time for all CRDs to be created.
	cRDPollTimeout = 60 * time.Second

	// operatorReconcileStr indicates that the operator will reconcile the resource.
	operatorReconcileStr = "Reconcile"
)

var (
	// operatorLabelStr indicates Istio operator is managing this resource.
	operatorLabelStr = name.OperatorAPINamespace + "/managed"
	// istioComponentLabelStr indicates which Istio component a resource belongs to.
	istioComponentLabelStr = name.OperatorAPINamespace + "/component"
	// istioVersionLabelStr indicates the Istio version of the installation.
	istioVersionLabelStr = name.OperatorAPINamespace + "/version"
)

// ComponentApplyOutput is used to capture errors and stdout/stderr outputs for a command, per component.
type ComponentApplyOutput struct {
	// Stdout is the stdout output.
	Stdout string
	// Stderr is the stderr output.
	Stderr string
	// Error is the error output.
	Err error
	// Manifest is the manifest applied to the cluster.
	Manifest string
}

type CompositeOutput map[name.ComponentName]*ComponentApplyOutput

type componentNameToListMap map[name.ComponentName][]name.ComponentName
type componentTree map[name.ComponentName]interface{}

// deployment holds associated replicaSets for a deployment
type deployment struct {
	replicaSets *appsv1.ReplicaSet
	deployment  *appsv1.Deployment
}

var (
	componentDependencies = componentNameToListMap{
		name.IstioBaseComponentName: {
			name.PilotComponentName,
			name.PolicyComponentName,
			name.TelemetryComponentName,
			name.GalleyComponentName,
			name.CitadelComponentName,
			name.NodeAgentComponentName,
			name.CertManagerComponentName,
			name.SidecarInjectorComponentName,
			name.IngressComponentName,
			name.EgressComponentName,
			name.CNIComponentName,
			name.PrometheusOperatorComponentName,
			name.PrometheusComponentName,
			name.GrafanaComponentName,
			name.KialiComponentName,
			name.TracingComponentName,
		},
	}

	installTree      = make(componentTree)
	dependencyWaitCh = make(map[name.ComponentName]chan struct{})
	kubectl          = kubectlcmd.New()

	k8sRESTConfig *rest.Config
)

func init() {
	buildInstallTree()
	for _, parent := range componentDependencies {
		for _, child := range parent {
			dependencyWaitCh[child] = make(chan struct{}, 1)
		}
	}

}

// ParseK8SYAMLToIstioControlPlaneSpec parses a IstioControlPlane CustomResource YAML string and unmarshals in into
// an IstioControlPlaneSpec object. It returns the object and an API group/version with it.
func ParseK8SYAMLToIstioControlPlaneSpec(yml string) (*v1alpha2.IstioControlPlaneSpec, *schema.GroupVersionKind, error) {
	o, err := object.ParseYAMLToK8sObject([]byte(yml))
	if err != nil {
		return nil, nil, err
	}
	y, err := yaml.Marshal(o.UnstructuredObject().Object["spec"])
	if err != nil {
		return nil, nil, err
	}
	icp := &v1alpha2.IstioControlPlaneSpec{}
	if err := util.UnmarshalWithJSONPB(string(y), icp); err != nil {
		return nil, nil, err
	}
	gvk := o.GroupVersionKind()
	return icp, &gvk, nil
}

// RenderToDir writes manifests to a local filesystem directory tree.
func RenderToDir(manifests name.ManifestMap, outputDir string, dryRun, verbose bool) error {
	logAndPrint("Component dependencies tree: \n%s", installTreeString())
	logAndPrint("Rendering manifests to output dir %s", outputDir)
	return renderRecursive(manifests, installTree, outputDir, dryRun, verbose)
}

func renderRecursive(manifests name.ManifestMap, installTree componentTree, outputDir string, dryRun, verbose bool) error {
	for k, v := range installTree {
		componentName := string(k)
		ym := manifests[k]
		if ym == "" {
			logAndPrint("Manifest for %s not found, skip.", componentName)
			continue
		}
		logAndPrint("Rendering: %s", componentName)
		dirName := filepath.Join(outputDir, componentName)
		if !dryRun {
			if err := os.MkdirAll(dirName, os.ModePerm); err != nil {
				return fmt.Errorf("could not create directory %s; %s", outputDir, err)
			}
		}
		fname := filepath.Join(dirName, componentName) + ".yaml"
		logAndPrint("Writing manifest to %s", fname)
		if !dryRun {
			if err := ioutil.WriteFile(fname, []byte(ym), 0644); err != nil {
				return fmt.Errorf("could not write manifest config; %s", err)
			}
		}

		kt, ok := v.(componentTree)
		if !ok {
			// Leaf
			return nil
		}
		if err := renderRecursive(manifests, kt, dirName, dryRun, verbose); err != nil {
			return err
		}
	}
	return nil
}

// InstallOptions contains the startup options for applying the manifest.
type InstallOptions struct {
	// DryRun performs all steps except actually applying the manifests or creating output dirs/files.
	DryRun bool
	// Verbose enables verbose debug output.
	Verbose bool
	// Wait for resources to be ready after install.
	Wait bool
	// Maximum amount of time to wait for resources to be ready after install when Wait=true.
	WaitTimeout time.Duration
	// Path to the kubeconfig file.
	Kubeconfig string
	// Name of the kubeconfig context to use.
	Context string
}

// ApplyAll applies all given manifests using kubectl client.
func ApplyAll(manifests name.ManifestMap, version version.Version, opts *InstallOptions) (CompositeOutput, error) {
	log.Infof("Preparing manifests for these components:")
	for c := range manifests {
		log.Infof("- %s", c)
	}
	log.Infof("Component dependencies tree: \n%s", installTreeString())
	if err := initK8SRestClient(opts.Kubeconfig, opts.Context); err != nil {
		return nil, err
	}
	return applyRecursive(manifests, version, opts)
}

func applyRecursive(manifests name.ManifestMap, version version.Version, opts *InstallOptions) (CompositeOutput, error) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := CompositeOutput{}
	allAppliedObjects := object.K8sObjects{}
	for c, m := range manifests {
		c := c
		m := m
		wg.Add(1)
		go func() {
			if s := dependencyWaitCh[c]; s != nil {
				log.Infof("%s is waiting on a prerequisite...", c)
				<-s
				log.Infof("Prerequisite for %s has completed, proceeding with install.", c)
			}
			applyOut, appliedObjects := applyManifest(c, m, version, opts)
			mu.Lock()
			out[c] = applyOut
			allAppliedObjects = append(allAppliedObjects, appliedObjects...)
			mu.Unlock()

			// Signal all the components that depend on us.
			for _, ch := range componentDependencies[c] {
				log.Infof("unblocking child %s.", ch)
				dependencyWaitCh[ch] <- struct{}{}
			}
			wg.Done()
		}()
	}
	wg.Wait()
	if opts.Wait {
		return out, waitForResources(allAppliedObjects, opts)
	}
	return out, nil
}

func applyManifest(componentName name.ComponentName, manifestStr string, version version.Version,
	opts *InstallOptions) (*ComponentApplyOutput, object.K8sObjects) {
	stdout, stderr := "", ""
	appliedObjects := object.K8sObjects{}
	objects, err := object.ParseK8sObjectsFromYAMLManifest(manifestStr)
	if err != nil {
		return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
	}
	componentLabel := fmt.Sprintf("%s=%s", istioComponentLabelStr, componentName)

	// TODO: remove this when `kubectl --prune` supports empty objects
	//  (https://github.com/kubernetes/kubernetes/issues/40635)
	// Delete all resources for a disabled component
	if len(objects) == 0 {
		extraArgsGet := []string{"--all-namespaces", "--selector", componentLabel}
		stdoutGet, stderrGet, err := kubectl.GetAll(opts.Kubeconfig, opts.Context, "", "yaml", extraArgsGet...)
		if err != nil {
			stdout += "\n" + stdoutGet
			stderr += "\n" + stderrGet
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}
		items, err := GetKubectlGetItems(stdoutGet)
		if err != nil {
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}
		if len(items) == 0 {
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}

		logAndPrint("- Pruning objects for disabled component %s...", componentName)
		delObjects, err := object.ParseK8sObjectsFromYAMLManifest(stdoutGet)
		if err != nil {
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}
		extraArgsDel := []string{"--selector", componentLabel}
		stdoutDel, stderrDel, err := kubectl.Delete(opts.DryRun, opts.Verbose, opts.Kubeconfig, opts.Context, "", stdoutGet, extraArgsDel...)
		stdout += "\n" + stdoutDel
		stderr += "\n" + stderrDel
		if err != nil {
			logAndPrint("✘ Finished pruning objects for disabled component %s.", componentName)
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}
		appliedObjects = append(appliedObjects, delObjects...)
		logAndPrint("✔ Finished pruning objects for disabled component %s.", componentName)
		return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
	}

	namespace := ""
	for _, o := range objects {
		o.AddLabels(map[string]string{istioComponentLabelStr: string(componentName)})
		o.AddLabels(map[string]string{operatorLabelStr: operatorReconcileStr})
		o.AddLabels(map[string]string{istioVersionLabelStr: version.String()})
		if o.Namespace != "" {
			// All objects in a component have the same namespace.
			namespace = o.Namespace
		}
	}
	objects.Sort(defaultObjectOrder())

	extraArgs := []string{"--force"}
	// Base components include namespaces and CRDs, pruning them will remove user configs, which makes it hard to roll back.
	if componentName != name.IstioBaseComponentName {
		extraArgs = append(extraArgs, "--prune", "--selector", componentLabel)
	}
	logAndPrint("- Applying manifest for component %s...", componentName)
	nsObjects := nsKindObjects(objects)
	if len(nsObjects) > 0 {
		mns, err := nsObjects.JSONManifest()
		if err != nil {
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}

		stdoutNs, stderrNs, err := kubectl.Apply(opts.DryRun, opts.Verbose, opts.Kubeconfig, opts.Context, namespace, mns, extraArgs...)
		stdout += "\n" + stdoutNs
		stderr += "\n" + stderrNs
		if err != nil {
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}

		if err := waitForResources(nsObjects, opts); err != nil {
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}
	}
	appliedObjects = append(appliedObjects, nsObjects...)

	crdObjects := cRDKindObjects(objects)
	if len(crdObjects) > 0 {
		mcrd, err := crdObjects.JSONManifest()
		if err != nil {
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}

		stdoutCRD, stderrCRD, err := kubectl.Apply(opts.DryRun, opts.Verbose, opts.Kubeconfig, opts.Context, namespace, mcrd, extraArgs...)
		stdout += "\n" + stdoutCRD
		stderr += "\n" + stderrCRD
		if err != nil {
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}
		// Not all Istio components are robust to not yet created CRDs.
		if err := waitForCRDs(objects, opts.DryRun); err != nil {
			return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
		}
	}
	appliedObjects = append(appliedObjects, crdObjects...)

	nonNsCrdObjects := objectsNotInLists(objects, nsObjects, crdObjects)
	m, err := nonNsCrdObjects.JSONManifest()
	if err != nil {
		return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
	}
	stdoutNonNsCrd, stderrNonNsCrd, err := kubectl.Apply(opts.DryRun, opts.Verbose, opts.Kubeconfig, opts.Context, namespace, m, extraArgs...)
	stdout += "\n" + stdoutNonNsCrd
	stderr += "\n" + stderrNonNsCrd
	appliedObjects = append(appliedObjects, nonNsCrdObjects...)
	mark := "✔"
	if err != nil {
		mark = "✘"
	}
	logAndPrint("%s Finished applying manifest for component %s.", mark, componentName)
	return buildComponentApplyOutput(stdout, stderr, appliedObjects, err), appliedObjects
}

func GetKubectlGetItems(stdoutGet string) ([]interface{}, error) {
	yamlGet := make(map[string]interface{})
	err := yaml.Unmarshal([]byte(stdoutGet), &yamlGet)
	if err != nil {
		return nil, err
	}
	if yamlGet["kind"] != "List" {
		return nil, fmt.Errorf("`kubectl get` returned a yaml whose kind is not List")
	}
	if _, ok := yamlGet["items"]; !ok {
		return nil, fmt.Errorf("`kubectl get` returned a yaml without 'items' in the root")
	}
	switch items := yamlGet["items"].(type) {
	case []interface{}:
		return items, nil
	}
	return nil, fmt.Errorf("`kubectl get` returned a yaml incorrecnt type 'items' in the root")
}

func buildComponentApplyOutput(stdout string, stderr string, objects object.K8sObjects, err error) *ComponentApplyOutput {
	manifest, _ := objects.YAMLManifest()
	return &ComponentApplyOutput{
		Stdout:   stdout,
		Stderr:   stderr,
		Manifest: manifest,
		Err:      err,
	}
}

func defaultObjectOrder() func(o *object.K8sObject) int {
	return func(o *object.K8sObject) int {
		gk := o.Group + "/" + o.Kind
		switch gk {
		// Create CRDs asap - both because they are slow and because we will likely create instances of them soon
		case "apiextensions.k8s.io/CustomResourceDefinition":
			return -1000

			// We need to create ServiceAccounts, Roles before we bind them with a RoleBinding
		case "/ServiceAccount", "rbac.authorization.k8s.io/ClusterRole":
			return 1
		case "rbac.authorization.k8s.io/ClusterRoleBinding":
			return 2

			// Pods might need configmap or secrets - avoid backoff by creating them first
		case "/ConfigMap", "/Secrets":
			return 100

			// Create the pods after we've created other things they might be waiting for
		case "extensions/Deployment", "app/Deployment":
			return 1000

			// Autoscalers typically act on a deployment
		case "autoscaling/HorizontalPodAutoscaler":
			return 1001

			// Create services late - after pods have been started
		case "/Service":
			return 10000

		default:
			return 1000
		}
	}
}

func cRDKindObjects(objects object.K8sObjects) object.K8sObjects {
	var ret object.K8sObjects
	for _, o := range objects {
		if o.Kind == "CustomResourceDefinition" {
			ret = append(ret, o)
		}
	}
	return ret
}

func nsKindObjects(objects object.K8sObjects) object.K8sObjects {
	var ret object.K8sObjects
	for _, o := range objects {
		if o.Kind == "Namespace" {
			ret = append(ret, o)
		}
	}
	return ret
}

func objectsNotInLists(objects object.K8sObjects, lists ...object.K8sObjects) object.K8sObjects {
	var ret object.K8sObjects

	filterMap := make(map[*object.K8sObject]bool)
	for _, list := range lists {
		for _, object := range list {
			filterMap[object] = true
		}
	}

	for _, o := range objects {
		if !filterMap[o] {
			ret = append(ret, o)
		}
	}
	return ret
}

func waitForCRDs(objects object.K8sObjects, dryRun bool) error {
	if dryRun {
		log.Info("Not waiting for CRDs in dry run mode.")
		return nil
	}

	log.Info("Waiting for CRDs to be applied.")
	cs, err := apiextensionsclient.NewForConfig(k8sRESTConfig)
	if err != nil {
		return fmt.Errorf("k8s client error: %s", err)
	}

	var crdNames []string
	for _, o := range cRDKindObjects(objects) {
		crdNames = append(crdNames, o.Name)
	}

	errPoll := wait.Poll(cRDPollInterval, cRDPollTimeout, func() (bool, error) {
	descriptor:
		for _, crdName := range crdNames {
			crd, errGet := cs.ApiextensionsV1beta1().CustomResourceDefinitions().Get(crdName, metav1.GetOptions{})
			if errGet != nil {
				return false, errGet
			}
			for _, cond := range crd.Status.Conditions {
				switch cond.Type {
				case apiextensionsv1beta1.Established:
					if cond.Status == apiextensionsv1beta1.ConditionTrue {
						log.Infof("established CRD %q", crdName)
						continue descriptor
					}
				case apiextensionsv1beta1.NamesAccepted:
					if cond.Status == apiextensionsv1beta1.ConditionFalse {
						log.Warnf("name conflict: %v", cond.Reason)
					}
				}
			}
			log.Infof("missing status condition for %q", crdName)
			return false, nil
		}
		return true, nil
	})

	if errPoll != nil {
		log.Errorf("failed to verify CRD creation; %s", errPoll)
		return fmt.Errorf("failed to verify CRD creation: %s", errPoll)
	}

	log.Info("Finished applying CRDs.")
	return nil
}

// waitForResources polls to get the current status of all pods, PVCs, and Services
// until all are ready or a timeout is reached
// TODO - plumb through k8s client and remove global `k8sRESTConfig`
func waitForResources(objects object.K8sObjects, opts *InstallOptions) error {
	if opts.DryRun {
		logAndPrint("Not waiting for resources ready in dry run mode.")
		return nil
	}

	cs, err := kubernetes.NewForConfig(k8sRESTConfig)
	if err != nil {
		return fmt.Errorf("k8s client error: %s", err)
	}

	var notReady []string

	errPoll := wait.Poll(2*time.Second, opts.WaitTimeout, func() (bool, error) {
		pods := []v1.Pod{}
		services := []v1.Service{}
		deployments := []deployment{}
		namespaces := []v1.Namespace{}

		for _, o := range objects {
			kind := o.GroupVersionKind().Kind
			switch kind {
			case "Namespace":
				namespace, err := cs.CoreV1().Namespaces().Get(o.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				namespaces = append(namespaces, *namespace)
			case "Pod":
				pod, err := cs.CoreV1().Pods(o.Namespace).Get(o.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				pods = append(pods, *pod)
			case "ReplicationController":
				rc, err := cs.CoreV1().ReplicationControllers(o.Namespace).Get(o.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				list, err := getPods(cs, rc.Namespace, rc.Spec.Selector)
				if err != nil {
					return false, err
				}
				pods = append(pods, list...)
			case "Deployment":
				currentDeployment, err := cs.AppsV1().Deployments(o.Namespace).Get(o.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				_, _, newReplicaSet, err := kubectlutil.GetAllReplicaSets(currentDeployment, cs.AppsV1())
				if err != nil || newReplicaSet == nil {
					return false, err
				}
				newDeployment := deployment{
					newReplicaSet,
					currentDeployment,
				}
				deployments = append(deployments, newDeployment)
			case "DaemonSet":
				ds, err := cs.AppsV1().DaemonSets(o.Namespace).Get(o.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				list, err := getPods(cs, ds.Namespace, ds.Spec.Selector.MatchLabels)
				if err != nil {
					return false, err
				}
				pods = append(pods, list...)
			case "StatefulSet":
				sts, err := cs.AppsV1().StatefulSets(o.Namespace).Get(o.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				list, err := getPods(cs, sts.Namespace, sts.Spec.Selector.MatchLabels)
				if err != nil {
					return false, err
				}
				pods = append(pods, list...)
			case "ReplicaSet":
				rs, err := cs.AppsV1().ReplicaSets(o.Namespace).Get(o.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				list, err := getPods(cs, rs.Namespace, rs.Spec.Selector.MatchLabels)
				if err != nil {
					return false, err
				}
				pods = append(pods, list...)
			case "Service":
				svc, err := cs.CoreV1().Services(o.Namespace).Get(o.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				services = append(services, *svc)
			}
		}

		dr, dnr := deploymentsReady(deployments)
		nsr, nnr := namespacesReady(namespaces)
		pr, pnr := podsReady(pods)
		sr, snr := servicesReady(services)
		isReady := dr && nsr && pr && sr
		if !isReady {
			logAndPrint("  Waiting for resources to become ready...")
		}
		notReady = joinStringSlices(nnr, dnr, pnr, snr)
		return isReady, nil
	})

	if errPoll != nil {
		msg := fmt.Sprintf("resources not ready after %v: %v\n%s", opts.WaitTimeout, errPoll, strings.Join(notReady, "\n"))
		return errors.New(msg)
	}
	return nil
}

func getPods(client kubernetes.Interface, namespace string, selector map[string]string) ([]v1.Pod, error) {
	list, err := client.CoreV1().Pods(namespace).List(metav1.ListOptions{
		FieldSelector: fields.Everything().String(),
		LabelSelector: labels.Set(selector).AsSelector().String(),
	})
	return list.Items, err
}

func namespacesReady(namespaces []v1.Namespace) (bool, []string) {
	var notReady []string
	for _, namespace := range namespaces {
		if !isNamespaceReady(&namespace) {
			notReady = append(notReady, "Namespace/"+namespace.Name)
		}
	}
	return len(notReady) == 0, notReady
}

func podsReady(pods []v1.Pod) (bool, []string) {
	var notReady []string
	for _, pod := range pods {
		if !isPodReady(&pod) {
			notReady = append(notReady, "Pod/"+pod.Namespace+"/"+pod.Name)
		}
	}
	return len(notReady) == 0, notReady
}

func isNamespaceReady(namespace *v1.Namespace) bool {
	return namespace.Status.Phase == v1.NamespaceActive
}

func isPodReady(pod *v1.Pod) bool {
	if len(pod.Status.Conditions) > 0 {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == v1.PodReady &&
				condition.Status == v1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

func deploymentsReady(deployments []deployment) (bool, []string) {
	var notReady []string
	for _, v := range deployments {
		if v.replicaSets.Status.ReadyReplicas < *v.deployment.Spec.Replicas {
			notReady = append(notReady, "Deployment/"+v.deployment.Namespace+"/"+v.deployment.Name)
		}
	}
	return len(notReady) == 0, notReady
}

func servicesReady(svc []v1.Service) (bool, []string) {
	var notReady []string
	for _, s := range svc {
		if s.Spec.Type == v1.ServiceTypeExternalName {
			continue
		}
		if s.Spec.ClusterIP != v1.ClusterIPNone && s.Spec.ClusterIP == "" {
			notReady = append(notReady, "Service/"+s.Namespace+"/"+s.Name)
			continue
		}
		if s.Spec.Type == v1.ServiceTypeLoadBalancer && s.Status.LoadBalancer.Ingress == nil {
			notReady = append(notReady, "Service/"+s.Namespace+"/"+s.Name)
			continue
		}
	}
	return len(notReady) == 0, notReady
}

func buildInstallTree() {
	// Starting with root, recursively insert each first level child into each node.
	insertChildrenRecursive(name.IstioBaseComponentName, installTree, componentDependencies)
}

func insertChildrenRecursive(componentName name.ComponentName, tree componentTree, children componentNameToListMap) {
	tree[componentName] = make(componentTree)
	for _, child := range children[componentName] {
		insertChildrenRecursive(child, tree[componentName].(componentTree), children)
	}
}

func installTreeString() string {
	var sb strings.Builder
	buildInstallTreeString(name.IstioBaseComponentName, "", &sb)
	return sb.String()
}

func buildInstallTreeString(componentName name.ComponentName, prefix string, sb io.StringWriter) {
	_, _ = sb.WriteString(prefix + string(componentName) + "\n")
	if _, ok := installTree[componentName].(componentTree); !ok {
		return
	}
	for k := range installTree[componentName].(componentTree) {
		buildInstallTreeString(k, prefix+"  ", sb)
	}
}

func initK8SRestClient(kubeconfig, context string) error {
	var err error
	if k8sRESTConfig != nil {
		return nil
	}
	k8sRESTConfig, err = defaultRestConfig(kubeconfig, context)
	if err != nil {
		return err
	}
	return nil
}

func defaultRestConfig(kubeconfig, configContext string) (*rest.Config, error) {
	config, err := BuildClientConfig(kubeconfig, configContext)
	if err != nil {
		return nil, err
	}
	config.APIPath = "/api"
	config.GroupVersion = &v1.SchemeGroupVersion
	config.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: scheme.Codecs}
	return config, nil
}

// BuildClientConfig is a helper function that builds client config from a kubeconfig filepath.
// It overrides the current context with the one provided (empty to use default).
//
// This is a modified version of k8s.io/client-go/tools/clientcmd/BuildConfigFromFlags with the
// difference that it loads default configs if not running in-cluster.
func BuildClientConfig(kubeconfig, context string) (*rest.Config, error) {
	if kubeconfig != "" {
		info, err := os.Stat(kubeconfig)
		if err != nil || info.Size() == 0 {
			// If the specified kubeconfig doesn't exists / empty file / any other error
			// from file stat, fall back to default
			kubeconfig = ""
		}
	}

	//Config loading rules:
	// 1. kubeconfig if it not empty string
	// 2. In cluster config if running in-cluster
	// 3. Config(s) in KUBECONFIG environment variable
	// 4. Use $HOME/.kube/config
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig
	loadingRules.ExplicitPath = kubeconfig
	configOverrides := &clientcmd.ConfigOverrides{
		ClusterDefaults: clientcmd.ClusterDefaults,
		CurrentContext:  context,
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides).ClientConfig()
}

func logAndPrint(v ...interface{}) {
	s := fmt.Sprintf(v[0].(string), v[1:]...)
	log.Infof(s)
	fmt.Println(s)
}

func joinStringSlices(s ...[]string) []string {
	var out []string
	for _, ss := range s {
		out = append(out, ss...)
	}
	return out
}
