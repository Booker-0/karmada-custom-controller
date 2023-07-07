package deployment

import (
	"context"
	"fmt"
	"strings"

	"github.com/karmada-io/karmada/pkg/util/helper"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	clusterv1alpha1 "github.com/karmada-io/karmada/pkg/apis/cluster/v1alpha1"
	policy1alpha1 "github.com/karmada-io/karmada/pkg/apis/policy/v1alpha1"
	workv1alpha1 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha1"
	karmadautil "github.com/karmada-io/karmada/pkg/util"
	"github.com/karmada-io/karmada/pkg/util/names"
)

const (
	ControllerName = "deployment-controller"
)

var workGVR = schema.GroupVersionResource{
	Group:    workv1alpha1.GroupVersion.Group,
	Version:  workv1alpha1.GroupVersion.Version,
	Resource: "works",
}

var _ reconcile.Reconciler = &Controller{}

// Controller reconciles a ContainerSet object
type Controller struct {
	client.Client
	scheme        *runtime.Scheme
	recorder      record.EventRecorder
	dynamicClient dynamic.Interface
}

// Reconcile  The function does not differentiate between create, update or deletion events.
// Instead it simply reads the state of the cluster at the time it is called.
func (c *Controller) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	deployment := &appsv1.Deployment{}
	clusterList := &clusterv1alpha1.ClusterList{}

	if err := c.Client.List(ctx, clusterList); err != nil {
		klog.Errorf("Failed to list clusters, error: %v", err)
		return ctrl.Result{}, nil
	}
	// 如果找不到 deployment ，则认为 deployment 已经删除，则删除 work
	if err := c.Client.Get(ctx, request.NamespacedName, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			klog.Warningf("Namespace %s %v", request.Namespace, err)
			if err := c.removeWorks(request, clusterList.Items); err != nil {
				klog.Errorf("delete namespace %q deployment %q work failed. err: %v", request.NamespacedName, request.Name, request.String(), err)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, err
	}
	// 通过 DeletionTimestamp 的值判断 deployment 状态，非空时删除 work
	clusters := c.skipClusters(deployment, clusterList.Items)
	if len(clusters) == 0 {
		return ctrl.Result{}, nil
	}

	if !deployment.DeletionTimestamp.IsZero() {
		if err := c.removeWorks(request, clusterList.Items); err != nil {
			klog.Errorf("delete namespace %q deployment %q work failed. err: %v", request.NamespacedName, request.Name, request.String(), err)
			return ctrl.Result{Requeue: true}, err
		}
		return ctrl.Result{}, nil
	}

	if v, ok := deployment.GetAnnotations()["bootstrapping.karmada.io/deployments-force"]; ok && v != "true" {
		c.buildPropagationPolicy(deployment, clusters)
		return ctrl.Result{}, nil
	}

	err := c.buildWorks(deployment, clusters)
	if err != nil {
		klog.Errorf("Failed to build work for namespace %s. Error: %v.", deployment.GetName(), err)
		return ctrl.Result{Requeue: true}, err
	}

	return reconcile.Result{}, nil
}

func (c *Controller) skipClusters(deployment *appsv1.Deployment, clusters []clusterv1alpha1.Cluster) []string {
	var validClusters []string

	annotations := deployment.GetAnnotations()
	if v, ok := annotations["bootstrapping.karmada.io/deployments-global"]; ok && v == "true" {
		klog.Infof("namespace %q deployment %q global distribution.", deployment.Namespace, deployment.Name)
		for _, cluster := range clusters {
			validClusters = append(validClusters, cluster.Name)
		}
		return validClusters
	}

	if _, ok := annotations["bootstrapping.karmada.io/deployments-members"]; !ok {
		klog.Infof("namespace %q deployment %q no global distribution is set, nor distributed to the specified member cluster, skip.", deployment.Namespace, deployment.Name)
		return nil
	}

	members := strings.Split(annotations["bootstrapping.karmada.io/deployments-members"], ",")
	for _, cluster := range clusters {
		for _, member := range members {
			if cluster.Name == member {
				validClusters = append(validClusters, cluster.Name)
				continue
			}
		}
	}

	klog.Infof("member clusters: %v,valid number of valid clusters: %v", members, validClusters)
	return validClusters
}

//在删除 deployment 对象后，然后通过标签找到并删除 work 对象
func (c *Controller) removeWorks(request ctrl.Request, clusters []clusterv1alpha1.Cluster) error {
	for _, cluster := range clusters {
		workNamespace, _ := names.GenerateExecutionSpaceName(cluster.Name)

		worksList, err := c.dynamicClient.Resource(workGVR).Namespace(workNamespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("bootstrapping.karmada.io/%s", request.Name),
		})
		if err != nil {
			return nil
		}

		if len(worksList.Items) == 0 {
			return nil
		}
		for _, work := range worksList.Items {
			if err := c.dynamicClient.Resource(workGVR).Namespace(workNamespace).Delete(context.TODO(), work.GetName(), metav1.DeleteOptions{}); err != nil {
				continue
			}
			klog.Infof("Delete cluster %q namespace %q deployment %q work successful.", cluster.Name, request.Namespace, request.Name)
		}
	}

	return nil
}

//生成的 work 对象需要取消 OwnerReferences 字段的声明，因为 work 对象是创建在成员集群对应的 Namespace 中，和 Deployment 不在一个 Namespace。OwnerReferences 只支持集群级别的资源和同 Namespace 级别的资源。反之就会因为找不到 Owner 被 Kubernetes 的 GC 控制器清理掉。
func (c *Controller) buildWorks(deployment *appsv1.Deployment, clusters []string) error {
	uncastObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deployment)
	if err != nil {
		klog.Errorf("Failed to transform deployment %s. Error: %v", deployment.GetName(), err)
		return nil
	}
	deploymentObj := &unstructured.Unstructured{Object: uncastObj}

	for _, cluster := range clusters {
		workNamespace, _ := names.GenerateExecutionSpaceName(cluster)
		if err != nil {
			klog.Errorf("Failed to generate execution space name for member cluster %s, err is %v", cluster, err)
			return err
		}

		workName := names.GenerateWorkName(deploymentObj.GetKind(), deploymentObj.GetName(), deploymentObj.GetNamespace())
		objectMeta := metav1.ObjectMeta{
			Name:       workName,
			Namespace:  workNamespace,
			Finalizers: []string{karmadautil.ExecutionControllerFinalizer},
			/*                      OwnerReferences: []metav1.OwnerReference{
			        *metav1.NewControllerRef(deployment, deployment.GroupVersionKind()),
			},*/
			Labels: map[string]string{fmt.Sprintf("bootstrapping.karmada.io/%s", deployment.Name): "true"},
		}
		klog.Infof("BuildWorks: WorkNamespace %q WorkName %q DeploymentNamespace %q DeploymentName %q", objectMeta.Namespace, objectMeta.Name, deployment.Namespace, deployment.Name)
		karmadautil.MergeLabel(deploymentObj, workv1alpha1.WorkNamespaceLabel, workNamespace)
		karmadautil.MergeLabel(deploymentObj, workv1alpha1.WorkNameLabel, workName)
		if err = helper.CreateOrUpdateWork(c.Client, objectMeta, deploymentObj); err != nil {
			return err
		}
	}
	return nil
}

//这里使用了一个简单的资源模板，声明了 OwnerReferences 字段，Deployment 删除后，等待 Kubernetes 的 GC 控制器删除即可。因为在同一个 Namespace，所以我们什么都不需要做。
//创建 PropagationPolicy 对象使用控制器管理器的 controllerutil 库。
// buildPropagationPolicy create PropagationPolicy
func (c *Controller) buildPropagationPolicy(deployment *appsv1.Deployment, clusters []string) {
	pp := &policy1alpha1.PropagationPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: policy1alpha1.GroupVersion.String(),
			Kind:       "PropagationPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployment.Name,
			Namespace: deployment.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(deployment, deployment.GroupVersionKind()),
			},
		},
		Spec: policy1alpha1.PropagationSpec{
			ResourceSelectors: []policy1alpha1.ResourceSelector{
				{
					APIVersion: deployment.APIVersion,
					Kind:       "Deployment",
					Name:       deployment.Name,
					Namespace:  deployment.Namespace,
				},
			},
			Placement: policy1alpha1.Placement{
				ClusterAffinity: &policy1alpha1.ClusterAffinity{
					ClusterNames: clusters,
				},
				ReplicaScheduling: &policy1alpha1.ReplicaSchedulingStrategy{
					ReplicaDivisionPreference: policy1alpha1.ReplicaDivisionPreferenceWeighted,
					ReplicaSchedulingType:     policy1alpha1.ReplicaSchedulingTypeDivided,
					WeightPreference: &policy1alpha1.ClusterPreferences{
						StaticWeightList: []policy1alpha1.StaticClusterWeight{
							{
								TargetCluster: policy1alpha1.ClusterAffinity{
									ClusterNames: clusters,
								},
								Weight: 1,
							},
						},
					},
				},
			},
		},
	}

	result, err := controllerutil.CreateOrUpdate(context.TODO(), c.Client, pp, func() error { return nil })
	if err != nil {
		klog.Errorf("Failed transform PropagationPolicy %s. err: %v", pp.GetName(), err)
		return
	}
	if result == controllerutil.OperationResultCreated {
		klog.Infof("Namespace %q Create PropagationPolicy %q successfully.", pp.GetNamespace(), pp.GetName())
	} else if result == controllerutil.OperationResultUpdated {
		klog.Infof("Namespace %q Update PropagationPolicy %q successfully.", pp.GetNamespace(), pp.GetName())
	} else {
		klog.V(3).Infof("Namespace %q Update PropagationPolicy %q is up to date.", pp.GetNamespace(), pp.GetName())
	}
}

//设置控制器管理器从 Karmada ApiServer With Deployment 资源的事件(Event)
//控制器管理器会监视资源的创建/更新/删除事件，并触发 Reconcile 函数作为响应，不断地对比资源对象的期望状态和实际状态
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	predicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return true
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return true
		},
		//GenericEvent用来处理未知类型的Event，比如非集群内资源事件，一般不会使用。
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}

	/*
	   return ctrl.NewControllerManagedBy(mgr).
	       // Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
	       // For().
	       Complete(r)*/
	return ctrl.NewControllerManagedBy(mgr).For(&appsv1.Deployment{}).WithEventFilter(predicate).Complete(c)
}

// NewController returns a new Controller
func NewController(mgr manager.Manager, dynamicClient dynamic.Interface) *Controller {
	return &Controller{
		Client:        mgr.GetClient(),
		scheme:        mgr.GetScheme(),
		recorder:      mgr.GetEventRecorderFor(ControllerName),
		dynamicClient: dynamicClient,
	}
}

// AddToManager create controller and register to controller manager
//需要把 Karmada 的资源注册到控制器管理器，否则控制器管理器会找不到 Karmada 的资源。
func AddToManager(mgr manager.Manager) error {
	// Setup Scheme for k8s appv1 resources
	if err := appsv1.AddToScheme(mgr.GetScheme()); err != nil {
		return err
	}
	// Setup Scheme for karmada clusterv1alpha1 resources
	if err := clusterv1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		return err
	}
	if err := policy1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		return err
	}
	// Setup Scheme for karmada workv1alpha1 resources workv1alpha1
	if err := workv1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}

	return NewController(mgr, dynamicClient).SetupWithManager(mgr)
}
