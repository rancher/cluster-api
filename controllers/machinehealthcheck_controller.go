/*
Copyright 2020 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	mhcClusterNameIndex  = "spec.clusterName"
	machineNodeNameIndex = "status.nodeRef.name"
)

// MachineHealthCheckReconciler reconciles a MachineHealthCheck object
type MachineHealthCheckReconciler struct {
	Client client.Client
	Log    logr.Logger

	controller           controller.Controller
	recorder             record.EventRecorder
	scheme               *runtime.Scheme
	clusterNodeInformers map[types.NamespacedName]cache.Informer
}

func (r *MachineHealthCheckReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	controller, err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.MachineHealthCheck{}).
		Watches(
			&source.Kind{Type: &clusterv1.Cluster{}},
			&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.clusterToMachineHealthCheck)},
		).
		Watches(
			&source.Kind{Type: &clusterv1.Machine{}},
			&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.machineToMachineHealthCheck)},
		).
		WithOptions(options).
		Build(r)

	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	// Add index to MachineHealthCheck for listing by Cluster Name
	if err := mgr.GetCache().IndexField(&clusterv1.MachineHealthCheck{},
		mhcClusterNameIndex,
		r.indexMachineHealthCheckByClusterName,
	); err != nil {
		return errors.Wrap(err, "error setting index fields")
	}

	// Add index to Machine for listing by Node reference
	if err := mgr.GetCache().IndexField(&clusterv1.Machine{},
		machineNodeNameIndex,
		r.indexMachineByNodeName,
	); err != nil {
		return errors.Wrap(err, "error setting index fields")
	}

	r.controller = controller
	r.recorder = mgr.GetEventRecorderFor("machinehealthcheck-controller")
	r.scheme = mgr.GetScheme()
	r.clusterNodeInformers = make(map[types.NamespacedName]cache.Informer)
	return nil
}

func (r *MachineHealthCheckReconciler) Reconcile(req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctx := context.Background()
	logger := r.Log.WithValues("machinehealthcheck", req.Name, "namespace", req.Namespace)

	// Fetch the MachineHealthCheck instance
	m := &clusterv1.MachineHealthCheck{}
	if err := r.Client.Get(ctx, req.NamespacedName, m); err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return ctrl.Result{}, nil
		}

		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to fetch MachineHealthCheck")
		return ctrl.Result{}, err
	}

	cluster, err := util.GetClusterByName(ctx, r.Client, m.Namespace, m.Spec.ClusterName)
	if err != nil {
		logger.Error(err, "Failed to fetch Cluster for MachineHealthCheck")
		return ctrl.Result{}, errors.Wrapf(err, "failed to get Cluster %q for MachineHealthCheck %q in namespace %q",
			m.Spec.ClusterName, m.Name, m.Namespace)
	}

	// Return early if the object or Cluster is paused.
	if util.IsPaused(cluster, m) {
		logger.V(3).Info("reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(m, r.Client)
	if err != nil {
		logger.Error(err, "Failed to build patch helper")
		return ctrl.Result{}, err
	}

	defer func() {
		// Always attempt to patch the object and status after each reconciliation.
		if err := patchHelper.Patch(ctx, m); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	// Reconcile labels.
	if m.Labels == nil {
		m.Labels = make(map[string]string)
	}
	m.Labels[clusterv1.ClusterLabelName] = m.Spec.ClusterName

	result, err := r.reconcile(ctx, cluster, m)
	if err != nil {
		logger.Error(err, "Failed to reconcile MachineHealthCheck")
		r.recorder.Eventf(m, corev1.EventTypeWarning, "ReconcileError", "%v", err)

		// Requeue immediately if any errors occurred
		return ctrl.Result{}, err
	}

	return result, nil
}

func (r *MachineHealthCheckReconciler) reconcile(ctx context.Context, cluster *clusterv1.Cluster, m *clusterv1.MachineHealthCheck) (ctrl.Result, error) {
	// Ensure the MachineHealthCheck is owned by the Cluster it belongs to
	m.OwnerReferences = util.EnsureOwnerRef(m.OwnerReferences, metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       cluster.Name,
		UID:        cluster.UID,
	})

	logger := r.Log.WithValues("machinehealthcheck", m.Name, "namespace", m.Namespace)
	logger = logger.WithValues("cluster", cluster.Name)

	// Create client for target cluster
	clusterClient, err := remote.NewClusterClient(r.Client, cluster, r.scheme)
	if err != nil {
		logger.Error(err, "Error building target cluster client")
		return ctrl.Result{}, err
	}

	err = r.watchClusterNodes(ctx, r.Client, cluster)
	if err != nil {
		logger.Error(err, "Error watching nodes on target cluster")
		return ctrl.Result{}, err
	}

	// fetch all targets
	logger.V(3).Info("Finding targets")
	targets, err := r.getTargetsFromMHC(clusterClient, cluster, m)
	if err != nil {
		logger.Error(err, "Failed to fetch targets from MachineHealthCheck")
		return ctrl.Result{}, err
	}
	totalTargets := len(targets)
	m.Status.ExpectedMachines = int32(totalTargets)

	// Default to 10 minutes but override if set in MachineHealthCheck
	timeoutForMachineToHaveNode := 10 * time.Minute
	if m.Spec.NodeStartupTimeout != nil {
		timeoutForMachineToHaveNode = m.Spec.NodeStartupTimeout.Duration
	}

	// health check all targets and reconcile mhc status
	currentHealthy, needRemediationTargets, nextCheckTimes := r.healthCheckTargets(targets, logger, timeoutForMachineToHaveNode)
	m.Status.CurrentHealthy = int32(currentHealthy)

	// remediate
	for _, t := range needRemediationTargets {
		logger.V(3).Info("Target meets unhealthy criteria, triggers remediation", "target", t.string())
		// TODO(JoelSpeed): Implement remediation logic
	}

	if minNextCheck := minDuration(nextCheckTimes); minNextCheck > 0 {
		logger.V(3).Info("Some targets might go unhealthy. Ensuring a requeue happens", "requeueIn", minNextCheck.Truncate(time.Second).String())
		return ctrl.Result{RequeueAfter: minNextCheck}, nil
	}

	logger.V(3).Info("No more targets meet unhealthy criteria")

	return ctrl.Result{}, nil
}

func (r *MachineHealthCheckReconciler) indexMachineHealthCheckByClusterName(object runtime.Object) []string {
	mhc, ok := object.(*clusterv1.MachineHealthCheck)
	if !ok {
		r.Log.Error(errors.New("incorrect type"), "expected a MachineHealthCheck", "type", fmt.Sprintf("%T", object))
		return nil
	}

	return []string{mhc.Spec.ClusterName}
}

// clusterToMachineHealthCheck maps events from Cluster objects to
// MachineHealthCheck objects that belong to the Cluster
func (r *MachineHealthCheckReconciler) clusterToMachineHealthCheck(o handler.MapObject) []reconcile.Request {
	c, ok := o.Object.(*clusterv1.Cluster)
	if !ok {
		r.Log.Error(errors.New("incorrect type"), "expected a Cluster", "type", fmt.Sprintf("%T", o))
		return nil
	}

	mhcList := &clusterv1.MachineHealthCheckList{}
	if err := r.Client.List(
		context.TODO(),
		mhcList,
		client.InNamespace(c.Namespace),
		client.MatchingFields{mhcClusterNameIndex: c.Name},
	); err != nil {
		r.Log.Error(err, "Unable to list MachineHealthChecks", "cluster", c.Name, "namespace", c.Namespace)
		return nil
	}

	// This list should only contain MachineHealthChecks which belong to the given Cluster
	requests := []reconcile.Request{}
	for _, mhc := range mhcList.Items {
		key := types.NamespacedName{Namespace: mhc.Namespace, Name: mhc.Name}
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}

func namespacedName(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

// machineToMachineHealthCheck maps events from Machine objects to
// MachineHealthCheck objects that monitor the given machine
func (r *MachineHealthCheckReconciler) machineToMachineHealthCheck(o handler.MapObject) []reconcile.Request {
	m, ok := o.Object.(*clusterv1.Machine)
	if !ok {
		r.Log.Error(errors.New("incorrect type"), "expected a Machine", "type", fmt.Sprintf("%T", o))
		return nil
	}

	mhcList := &clusterv1.MachineHealthCheckList{}
	if err := r.Client.List(
		context.Background(),
		mhcList,
		&client.ListOptions{Namespace: m.Namespace},
		client.MatchingFields{mhcClusterNameIndex: m.Spec.ClusterName},
	); err != nil {
		r.Log.Error(err, "Unable to list MachineHealthChecks", "machine", m.Name, "namespace", m.Namespace)
		return nil
	}

	var requests []reconcile.Request
	for k := range mhcList.Items {
		if r.hasMatchingLabels(&mhcList.Items[k], m) {
			key := types.NamespacedName{Namespace: mhcList.Items[k].Namespace, Name: mhcList.Items[k].Name}
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
	}
	return requests
}

func (r *MachineHealthCheckReconciler) nodeToMachineHealthCheck(o handler.MapObject) []reconcile.Request {
	node, ok := o.Object.(*corev1.Node)
	if !ok {
		r.Log.Error(errors.New("incorrect type"), "expected a Node", "type", fmt.Sprintf("%T", o))
		return nil
	}

	machine, err := r.getMachineFromNode(node.Name)
	if machine == nil || err != nil {
		r.Log.Error(err, "Unable to retrieve machine from node", "node", namespacedName(node).String())
		return nil
	}

	mhcList := &clusterv1.MachineHealthCheckList{}
	if err := r.Client.List(
		context.Background(),
		mhcList,
		&client.ListOptions{Namespace: machine.Namespace},
		client.MatchingFields{mhcClusterNameIndex: machine.Spec.ClusterName},
	); err != nil {
		r.Log.Error(err, "Unable to list MachineHealthChecks", "node", node.Name, "machine", machine.Name, "namespace", machine.Namespace)
		return nil
	}

	var requests []reconcile.Request
	for k := range mhcList.Items {
		if r.hasMatchingLabels(&mhcList.Items[k], machine) {
			key := types.NamespacedName{Namespace: mhcList.Items[k].Namespace, Name: mhcList.Items[k].Name}
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
	}
	return requests
}

func (r *MachineHealthCheckReconciler) getMachineFromNode(nodeName string) (*clusterv1.Machine, error) {
	machineList := &clusterv1.MachineList{}
	if err := r.Client.List(
		context.TODO(),
		machineList,
		client.MatchingFields{machineNodeNameIndex: nodeName},
	); err != nil {
		return nil, errors.Wrap(err, "failed getting machine list")
	}
	if len(machineList.Items) != 1 {
		return nil, errors.New(fmt.Sprintf("expecting one machine for node %v, got: %v", nodeName, machineList.Items))
	}
	return &machineList.Items[0], nil
}

func (r *MachineHealthCheckReconciler) watchClusterNodes(ctx context.Context, c client.Client, cluster *clusterv1.Cluster) error {
	key := types.NamespacedName{Namespace: cluster.Name, Name: cluster.Name}
	if _, ok := r.clusterNodeInformers[key]; ok {
		// watch was already set up for this cluster
		return nil
	}

	config, err := remote.RESTConfig(c, cluster)
	if err != nil {
		return errors.Wrap(err, "error fetching remote cluster config")
	}

	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "error constructing remote cluster client")
	}

	// TODO(JoelSpeed): See if we use the resync period from the manager instead of 0
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	nodeInformer := factory.Core().V1().Nodes().Informer()
	go nodeInformer.Run(ctx.Done())

	err = r.controller.Watch(
		&source.Informer{Informer: nodeInformer},
		&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.nodeToMachineHealthCheck)},
	)
	if err != nil {
		return errors.Wrap(err, "error watching nodes on target cluster")
	}

	if r.clusterNodeInformers == nil {
		r.clusterNodeInformers = make(map[types.NamespacedName]cache.Informer)
	}

	r.clusterNodeInformers[key] = nodeInformer
	return nil
}

func (r *MachineHealthCheckReconciler) indexMachineByNodeName(object runtime.Object) []string {
	machine, ok := object.(*clusterv1.Machine)
	if !ok {
		r.Log.Error(errors.New("incorrect type"), "expected a Machine", "type", fmt.Sprintf("%T", object))
		return nil
	}

	if machine.Status.NodeRef != nil {
		return []string{machine.Status.NodeRef.Name}
	}

	return nil
}

// hasMatchingLabels verifies that the MachineHealthCheck's label selector
// matches the given Machine
func (r *MachineHealthCheckReconciler) hasMatchingLabels(machineHealthCheck *clusterv1.MachineHealthCheck, machine *clusterv1.Machine) bool {
	// This should never fail, validating webhook should catch this first
	selector, err := metav1.LabelSelectorAsSelector(&machineHealthCheck.Spec.Selector)
	if err != nil {
		return false
	}
	// If a MachineHealthCheck with a nil or empty selector creeps in, it should match nothing, not everything.
	if selector.Empty() {
		return false
	}
	if !selector.Matches(labels.Set(machine.Labels)) {
		return false
	}
	return true
}
