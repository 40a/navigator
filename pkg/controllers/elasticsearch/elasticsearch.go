package elasticsearch

import (
	"fmt"
	"reflect"
	"time"

	"github.com/Sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1beta1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	depl "k8s.io/client-go/informers/extensions/v1beta1"
	"k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1beta1"
	corelisters "k8s.io/client-go/listers/core/v1"
	extensionslisters "k8s.io/client-go/listers/extensions/v1beta1"
	"k8s.io/client-go/pkg/api"
	apiv1 "k8s.io/client-go/pkg/api/v1"
	apps "k8s.io/client-go/pkg/apis/apps/v1beta1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"gitlab.jetstack.net/marshal/colonel/pkg/api/v1"
	"gitlab.jetstack.net/marshal/colonel/pkg/controllers"
	informersv1 "gitlab.jetstack.net/marshal/colonel/pkg/informers/v1"
	listersv1 "gitlab.jetstack.net/marshal/colonel/pkg/listers/v1"
)

type ElasticsearchController struct {
	kubeClient *kubernetes.Clientset

	esLister       listersv1.ElasticsearchClusterLister
	esListerSynced cache.InformerSynced

	deployLister       extensionslisters.DeploymentLister
	deployListerSynced cache.InformerSynced

	statefulSetLister       appslisters.StatefulSetLister
	statefulSetListerSynced cache.InformerSynced

	serviceAccountLister       corelisters.ServiceAccountLister
	serviceAccountListerSynced cache.InformerSynced

	serviceLister       corelisters.ServiceLister
	serviceListerSynced cache.InformerSynced

	queue                       workqueue.RateLimitingInterface
	elasticsearchClusterControl ElasticsearchClusterControl
}

func NewElasticsearch(
	es informersv1.ElasticsearchClusterInformer,
	deploys depl.DeploymentInformer,
	statefulsets appsinformers.StatefulSetInformer,
	serviceaccounts coreinformers.ServiceAccountInformer,
	services coreinformers.ServiceInformer,
	cl *kubernetes.Clientset,
) *ElasticsearchController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(cl.Core().RESTClient()).Events("")})
	recorder := eventBroadcaster.NewRecorder(api.Scheme, apiv1.EventSource{Component: "elasticsearchCluster"})

	elasticsearchController := &ElasticsearchController{
		kubeClient: cl,
		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "elasticsearchCluster"),
	}

	es.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: elasticsearchController.enqueueElasticsearchCluster,
		UpdateFunc: func(old, cur interface{}) {
			if reflect.DeepEqual(old, cur) {
				return
			}
			elasticsearchController.enqueueElasticsearchCluster(cur)
		},
		DeleteFunc: elasticsearchController.enqueueElasticsearchClusterDelete,
	})
	elasticsearchController.esLister = es.Lister()
	elasticsearchController.esListerSynced = es.Informer().HasSynced

	deploys.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: elasticsearchController.handleDeploy,
		UpdateFunc: func(old, cur interface{}) {
			if reflect.DeepEqual(old, cur) {
				return
			}
			elasticsearchController.handleDeploy(cur)
		},
		DeleteFunc: func(obj interface{}) {
			elasticsearchController.handleDeploy(obj)
		},
	})
	elasticsearchController.deployLister = deploys.Lister()
	elasticsearchController.deployListerSynced = deploys.Informer().HasSynced

	statefulsets.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: elasticsearchController.handleStatefulSet,
		UpdateFunc: func(old, new interface{}) {
			if reflect.DeepEqual(old, new) {
				return
			}
			elasticsearchController.handleStatefulSet(new)
		},
		DeleteFunc: elasticsearchController.handleStatefulSet,
	})
	elasticsearchController.statefulSetLister = statefulsets.Lister()
	elasticsearchController.statefulSetListerSynced = statefulsets.Informer().HasSynced

	serviceaccounts.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: elasticsearchController.handleServiceAccount,
		UpdateFunc: func(old, new interface{}) {
			if reflect.DeepEqual(old, new) {
				return
			}
			elasticsearchController.handleServiceAccount(new)
		},
		DeleteFunc: elasticsearchController.handleServiceAccount,
	})
	elasticsearchController.serviceAccountLister = serviceaccounts.Lister()
	elasticsearchController.serviceAccountListerSynced = serviceaccounts.Informer().HasSynced

	services.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: elasticsearchController.handleService,
		UpdateFunc: func(old, new interface{}) {
			if reflect.DeepEqual(old, new) {
				return
			}
			elasticsearchController.handleService(new)
		},
		DeleteFunc: elasticsearchController.handleService,
	})
	elasticsearchController.serviceLister = services.Lister()
	elasticsearchController.serviceListerSynced = services.Informer().HasSynced

	elasticsearchController.elasticsearchClusterControl = NewElasticsearchClusterControl(
		elasticsearchController.statefulSetLister,
		elasticsearchController.deployLister,
		elasticsearchController.serviceAccountLister,
		elasticsearchController.serviceLister,
		NewElasticsearchClusterNodePoolControl(
			cl,
			elasticsearchController.deployLister,
			recorder,
		),
		NewStatefulElasticsearchClusterNodePoolControl(
			cl,
			elasticsearchController.statefulSetLister,
			recorder,
		),
		NewElasticsearchClusterServiceAccountControl(
			cl,
			recorder,
		),
		// client service controller
		NewElasticsearchClusterServiceControl(
			cl,
			recorder,
			ServiceControlConfig{
				NameSuffix: "clients",
				EnableHTTP: true,
				Roles:      []string{"client"},
			},
		),
		// discovery service controller
		NewElasticsearchClusterServiceControl(
			cl,
			recorder,
			ServiceControlConfig{
				NameSuffix:  "discovery",
				Annotations: map[string]string{"service.alpha.kubernetes.io/tolerate-unready-endpoints": "true"},
			},
		),
		recorder,
	)

	return elasticsearchController
}

func (e *ElasticsearchController) Run(workers int, stopCh <-chan struct{}) {
	defer e.queue.ShutDown()

	logrus.Infof("Starting Elasticsearch controller")

	if !cache.WaitForCacheSync(stopCh, e.deployListerSynced, e.esListerSynced, e.statefulSetListerSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
	}

	for i := 0; i < workers; i++ {
		go wait.Until(e.worker, time.Second, stopCh)
	}

	<-stopCh
	logrus.Infof("Shutting down Elasticsearch controller")
}

func (e *ElasticsearchController) worker() {
	logrus.Infof("start worker loop")
	for e.processNextWorkItem() {
		logrus.Infof("processed work item")
	}
	logrus.Infof("exiting worker loop")
}

func (e *ElasticsearchController) processNextWorkItem() bool {
	key, quit := e.queue.Get()
	if quit {
		return false
	}
	defer e.queue.Done(key)

	if k, ok := key.(string); ok {
		if err := e.sync(k); err != nil {
			logrus.Infof("Error syncing ElasticsearchCluster %v, requeuing: %v", key.(string), err)
			e.queue.AddRateLimited(key)
		} else {
			e.queue.Forget(key)
		}
	} else if es, ok := key.(*v1.ElasticsearchCluster); ok {
		t := metav1.NewTime(time.Now())
		es.DeletionTimestamp = &t
		if err := e.elasticsearchClusterControl.SyncElasticsearchCluster(es); err != nil {
			logrus.Infof("Error syncing ElasticsearchCluster %v, requeuing: %v", es.Name, err)
		}
		e.queue.Forget(key)
	}

	return true
}

// TODO: properly log errors to an events sink
// TODO: move verification out of this function
func (e *ElasticsearchController) sync(key string) error {
	startTime := time.Now()
	defer func() {
		logrus.Infof("Finished syncing elasticsearchcluster %q (%v)", key, time.Now().Sub(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	es, err := e.esLister.ElasticsearchClusters(namespace).Get(name)
	if errors.IsNotFound(err) {
		logrus.Infof("ElasticsearchCluster has been deleted %v", key)
		return nil
	}
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to retrieve ElasticsearchCluster %v from store: %v", key, err))
		return err
	}

	return e.elasticsearchClusterControl.SyncElasticsearchCluster(es)
}

func (e *ElasticsearchController) enqueueElasticsearchCluster(obj interface{}) {
	key, err := controllers.KeyFunc(obj)
	if err != nil {
		// TODO: log error
		logrus.Infof("Cound't get key for object %+v: %v", obj, err)
		return
	}
	e.queue.Add(key)
}

func (e *ElasticsearchController) enqueueElasticsearchClusterDelete(obj interface{}) {
	e.queue.Add(obj)
}

func (e *ElasticsearchController) handleDeploy(obj interface{}) {
	var deploy *extensions.Deployment
	var ok bool
	if deploy, ok = obj.(*extensions.Deployment); !ok {
		logrus.Errorf("error decoding deployment, invalid type")
		return
	}
	if ownerRef := managedOwnerRef(deploy.ObjectMeta); ownerRef != nil {
		logrus.Debugf("getting elasticsearchcluster '%s/%s'", deploy.Namespace, ownerRef.Name)
		cluster, err := e.esLister.ElasticsearchClusters(deploy.Namespace).Get(ownerRef.Name)

		if err != nil {
			logrus.Infof("ignoring orphaned deployment '%s' of elasticsearchcluster '%s'", deploy.Name, ownerRef.Name)
			return
		}

		e.enqueueElasticsearchCluster(cluster)
		return
	}
}

func (e *ElasticsearchController) handleStatefulSet(obj interface{}) {
	var ss *apps.StatefulSet
	var ok bool
	if ss, ok = obj.(*apps.StatefulSet); !ok {
		logrus.Errorf("error decoding statefulset, invalid type")
		return
	}
	if ownerRef := managedOwnerRef(ss.ObjectMeta); ownerRef != nil {
		cluster, err := e.esLister.ElasticsearchClusters(ss.Namespace).Get(ownerRef.Name)

		if err != nil {
			logrus.Infof("ignoring orphaned statefulset '%s' of elasticsearchcluster '%s'", ss.Name, ownerRef.Name)
			return
		}

		e.enqueueElasticsearchCluster(cluster)
		return
	}
}

func (e *ElasticsearchController) handleServiceAccount(obj interface{}) {
	var ss *apiv1.ServiceAccount
	var ok bool
	if ss, ok = obj.(*apiv1.ServiceAccount); !ok {
		logrus.Errorf("error decoding serviceaccount, invalid type")
		return
	}
	if ownerRef := managedOwnerRef(ss.ObjectMeta); ownerRef != nil {
		cluster, err := e.esLister.ElasticsearchClusters(ss.Namespace).Get(ownerRef.Name)

		if err != nil {
			logrus.Infof("ignoring orphaned serviceaccount '%s' of elasticsearchcluster '%s'", ss.Name, ownerRef.Name)
			return
		}

		e.enqueueElasticsearchCluster(cluster)
		return
	}
}

func (e *ElasticsearchController) handleService(obj interface{}) {
	var ss *apiv1.Service
	var ok bool
	if ss, ok = obj.(*apiv1.Service); !ok {
		logrus.Errorf("error decoding service, invalid type")
		return
	}
	if ownerRef := managedOwnerRef(ss.ObjectMeta); ownerRef != nil {
		cluster, err := e.esLister.ElasticsearchClusters(ss.Namespace).Get(ownerRef.Name)

		if err != nil {
			logrus.Infof("ignoring orphaned service '%s' of elasticsearchcluster '%s'", ss.Name, ownerRef.Name)
			return
		}

		e.enqueueElasticsearchCluster(cluster)
		return
	}
}

func verifyElasticsearchCluster(c *v1.ElasticsearchCluster) error {
	// TODO: add verification that at least one client, master and data node pool exist
	if c.Spec.Version == "" {
		return fmt.Errorf("cluster version number must be specified")
	}

	for _, np := range c.Spec.NodePools {
		if err := verifyNodePool(np); err != nil {
			return err
		}
	}

	return nil
}

func verifyNodePool(np *v1.ElasticsearchClusterNodePool) error {
	for _, role := range np.Roles {
		switch role {
		case "data", "client", "master":
		default:
			return fmt.Errorf("invalid role '%s' specified. must be one of 'data', 'client' or 'master'", role)
		}
	}

	if np.State != nil {
		if !np.State.Stateful && np.State.Persistence.Enabled {
			return fmt.Errorf("a non-stateful node pool cannot have persistence enabled")
		}
	}

	return nil
}
