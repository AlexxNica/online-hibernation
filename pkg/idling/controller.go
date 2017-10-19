package idling

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/hawkular/hawkular-client-go/metrics"
	"github.com/openshift/online-hibernation/pkg/cache"
	"github.com/openshift/origin/pkg/cmd/util/clientcmd"

	buildapi "github.com/openshift/origin/pkg/build/api"
	unidlingapi "github.com/openshift/origin/pkg/unidling/api"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/util/workqueue"
)

// IdlerConfig is the configuration for the hawkular client and idler.
type IdlerConfig struct {
	Exclude            map[string]bool
	Url                string
	IdleSyncPeriod     time.Duration
	IdleQueryPeriod    time.Duration
	CA                 string
	Token              string
	HawkularInsecure   bool
	Threshold          int
	SyncWorkers        int
	IdleDryRun         bool
	InCluster          bool
	ProjectSleepPeriod time.Duration
}

type Idler struct {
	factory   *clientcmd.Factory
	config    *IdlerConfig
	resources *cache.Cache
	queue     workqueue.RateLimitingInterface
	stopChannel <-chan struct{}
}

func NewIdler(ic *IdlerConfig, f *clientcmd.Factory, c *cache.Cache) *Idler {
	ctrl := &Idler{
		config:    ic,
		factory:   f,
		resources: c,
		queue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "auto-idler"),
	}
	return ctrl
}

// Main function for controller
func (idler *Idler) Run(stopChan <-chan struct{}) {
	idler.stopChannel = stopChan
	// Spawn a goroutine to run project sync
	go wait.Until(idler.sync, idler.config.IdleSyncPeriod, stopChan)
	for i := 1; i <= idler.config.SyncWorkers; i++ {
		go wait.Until(idler.startWorker(), time.Second, stopChan)
	}
	<-stopChan
	glog.V(2).Infof("Auto-idler: Shutting down controller")
	idler.queue.ShutDown()
}

// Spawns goroutines to sync projects
func (idler *Idler) sync() {
	glog.V(1).Infof("Auto-idler: Running project sync")
	projects, err := idler.resources.Indexer.ByIndex("ofKind", cache.ProjectKind)
	if err != nil {
		glog.Errorf("Auto-idler: %s", err)
		return
	}
	for _, namespace := range projects {
		ns := namespace.(*cache.ResourceObject).Name
		idler.queue.Add(ns)
	}
}

func (idler *Idler) startWorker() func() {
	workFunc := func() bool {
		ns, quit := idler.queue.Get()
		if quit {
			return true
		}
		defer idler.queue.Done(ns)
		if err := idler.syncProject(ns.(string)); err != nil {
			glog.Errorf("Error( %s ), requeuing: %v", ns.(string), err)
			idler.queue.AddRateLimited(ns)
			return false
		} else {
			idler.queue.Forget(ns)
			return false
		}
	}
	return func() {
		for {
			if quit := workFunc(); quit {
				glog.V(2).Infof("Auto-idler: worker shutting down")
				return
			}
		}
	}
}

// Sync projects and idle if necessary.
func (idler *Idler) syncProject(namespace string) error {
	if idler.config.Exclude[namespace] {
		return nil
	}
	project, err := idler.resources.GetProject(namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("%s", err))
	}
	if project.IsAsleep {
		glog.V(2).Infof("Auto-idler: Ignoring project( %s ), currently in force-sleep", namespace)
		return nil
	}
	// If true, endpoints in namespace are currently idled,
	// i.e. endpoints in project have idling annotations and len(scalablePods)==0
	// Project is not currently idled if there are any running scalable pods in namespace.
	idled, err := CheckIdledState(idler.resources, namespace, project.IsAsleep)
	if err != nil {
		return errors.New(fmt.Sprintf("%s", err))
	}
	if idled {
		glog.V(2).Infof("Auto-idler: Project( %s )sync complete", namespace)
		return nil
	}
	services, err := idler.resources.GetProjectServices(namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("%s", err))
	}
	if len(services) == 0 {
		glog.V(2).Infof("Auto-idler: Project( %s )has no services, exiting", namespace)
		return nil
	}
	// Get project again here, to catch any changes to 'IsAsleep' status
	project, err = idler.resources.GetProject(namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("%s", err))
	}
	if project.IsAsleep {
		glog.V(2).Infof("Auto-idler: Ignoring project( %s ), currently in force-sleep", namespace)
		return nil
	}
	glog.V(2).Infof("Auto-idler: Syncing project: %s ", project.Name)
	projPods, err := idler.resources.GetProjectPods(namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("%s", err))
	}
	for _, pod := range projPods {
		// Skip the project sync if any pod in the project has not been running for a complete idling cycle, or if controller hasn't been running
		// for complete idling cycle.
		if time.Since(pod.(*cache.ResourceObject).RunningTimes[0].Start) < idler.config.IdleQueryPeriod {
			glog.V(2).Infof("Auto-idler: Ignoring project( %s ), either pod( %s )or controller hasn't been running for complete idling cycle", namespace, pod.(*cache.ResourceObject).Name)
			glog.V(2).Infof("Auto-idler: Project( %s )sync complete", namespace)
			return nil
		}
	}
	if err := idler.idleIfInactive(namespace); err != nil {
		return errors.New(fmt.Sprintf("%s", err))
	}
	glog.V(2).Infof("Auto-idler: Project( %s )sync complete", namespace)
	return nil
}

func (idler *Idler) idleIfInactive(namespace string) error {
	project, err := idler.resources.GetProject(namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("%s", err))
	}
	// Extra check here, not sure it's necessary
	if project.IsAsleep {
		glog.V(2).Infof("Auto-idler: Ignoring project( %s ), currently in force-sleep.", namespace)
		return nil
	}
	svcs, err := idler.resources.GetProjectServices(namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("%s", err))
	}
	projectPods, err := idler.resources.GetProjectPods(namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("%s", err))
	}
	var scalablePods []interface{}
	for _, svc := range svcs {
		// Only consider pods with services for idling.
		podsWService := idler.resources.GetPodsForService(svc.(*cache.ResourceObject), projectPods)
		for _, pod := range podsWService {
			scalablePods = append(scalablePods, pod)
		}
	}
	// if there are no running pods, project can't be idled
	if len(scalablePods) == 0 {
		glog.V(2).Infof("Auto-idler: No scalable pods in project( %s ), exiting...", namespace)
		return nil
	}

	for _, podObj := range projectPods {
		pod := podObj.(*cache.ResourceObject)
		if _, ok := pod.Labels[buildapi.BuildAnnotation]; ok {
			if pod.IsStarted() {
				glog.V(2).Infof("Auto-idler: Ignoring project( %s ), builder pod( %s ) found in project.", namespace, pod.Name)
				return nil
			}
		}
		if time.Since(pod.RunningTimes[0].Start) < idler.config.IdleQueryPeriod {
			glog.V(2).Infof("Auto-idler: Ignoring project( %s ), either pod( %s )or controller hasn't been running for complete idling cycle.", namespace, pod.Name)
			return nil
		}
	}
	tags := make(map[string]string)
	var filters []metrics.Modifier
	tags["descriptor_name"] = "network/rx"
	tags["type"] = "pod"

	p := idler.getMetricsParams(namespace)

	period := -1 * idler.config.IdleQueryPeriod
	endtime := time.Now()
	starttime := endtime.Add(period)
	threshold := idler.config.Threshold
	filters = append(filters, metrics.Filters(metrics.TagsFilter(tags), metrics.BucketsFilter(1), metrics.StartTimeFilter(starttime), metrics.EndTimeFilter(endtime), metrics.StackedFilter()))

	c, err := metrics.NewHawkularClient(p)
	if err != nil {
		return errors.New(fmt.Sprintf("Auto-idler: %s", err))
	}
	bp, err := c.ReadBuckets(metrics.Counter, filters...)
	if err != nil {
		return errors.New(fmt.Sprintf("Auto-idler: %s", err))
	}

	for _, value := range bp {
		activity := value.Max - value.Min
		glog.V(2).Infof("Auto-idler: Project( %s )NETWORK_ACTIVITY(bytes): %v", namespace, int(activity))

		if int(activity) < threshold {
			glog.V(3).Infof("Auto-idler: Project( %s )MaxNetworkRX: %#v", namespace, value.Max)
			glog.V(3).Infof("Auto-idler: Project( %s )MinNetworkRX: %#v", namespace, value.Min)
			if !idler.config.IdleDryRun {
				glog.V(2).Infof("Auto-idler: Project( %s )activity below idling threshold, idling....", namespace)
				err := idler.autoIdleProjectServices(namespace)
				if err != nil {
					return errors.New(fmt.Sprintf("Auto-idler: %s", err))
				}
			} else {
				glog.V(2).Infof("Auto-idler: Project( %s )network activity below idling threshold, but currently in DryRun mode.", namespace)
			}
		} else {
			glog.V(3).Infof("Auto-idler: Project( %s )MaxNetworkRX: %#v", namespace, value.Max)
			glog.V(3).Infof("Auto-idler: Project( %s )MinNetworkRX: %#v", namespace, value.Min)
			glog.V(2).Infof("Auto-idler: Project( %s )network activity is above idling threshold.", namespace)
		}
	}
	return nil
}

// getMetricsParams initializes Hawkular Parameters.
func (idler *Idler) getMetricsParams(namespace string) metrics.Parameters {
	p := metrics.Parameters{
		Tenant: namespace,
		Url:    idler.config.Url,
		Token:  idler.config.Token,
	}

	// Authentication parameters
	tC := &tls.Config{}
	if idler.config.InCluster && !idler.config.HawkularInsecure {
		caCert := []byte(idler.config.CA)

		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)

		tC.RootCAs = caCertPool
	} else {
		if idler.config.CA == "" {
			tC.InsecureSkipVerify = true
		}
	}

	if idler.config.HawkularInsecure {
		tC.InsecureSkipVerify = true
	}

	p.TLSConfig = tC

	glog.V(2).Infof("Auto-idler: Initialised Hawkular for project( %s )", namespace)
	return p
}

// CheckIdledState returns true if project is idled, false if project is not currently idled
// This function checks pod count and endpoint annotations.  This is not 100% fail-proof, bc
// oc idle code itself has limitations.
func CheckIdledState(c *cache.Cache, namespace string, IsAsleep bool) (bool, error) {
	idled := false
	svcs, err := c.GetProjectServices(namespace)
	if err != nil {
		return idled, errors.New(fmt.Sprintf("Auto-idler: %s", err))
	}
	var runningPods []interface{}
	projPods, err := c.GetProjectPods(namespace)
	if err != nil {
		return idled, errors.New(fmt.Sprintf("Auto-idler: %s", err))
	}
	for _, p := range projPods {
		if p.(*cache.ResourceObject).IsStarted() {
			runningPods = append(runningPods, p)
		}
	}
	var scalablePods []interface{}
	for _, svc := range svcs {
		// Only consider pods with services for idling.
		podsWService := c.GetPodsForService(svc.(*cache.ResourceObject), runningPods)
		for _, pod := range podsWService {
			scalablePods = append(scalablePods, pod)
		}
	}
	// if there are any running pods, project is not idled
	if len(scalablePods) != 0 {
		idled = false
		return idled, nil
	}

	// TODO: Remove any idling annotations from pods with endpoints in the project
	// Currently, if there are 2 services in a project that share an RC, it's
	// possible for stale idling annotations to be left in an unidled project
	// in the endpoints of the service that was not explicitly un-idled, ie if
	// the project was manually scaled up or if just 1 of the svcs routes were
	// accessed.  This should be fixed in the oc idle code itself, rather than
	// in this hibernation controller.  For now, we should document this and
	// steer users away from setting more than 1 service per RC.
	endpointInterface := c.KubeClient.Endpoints(namespace)
	epList, err := endpointInterface.List(kapi.ListOptions{})
	if err != nil {
		return idled, errors.New(fmt.Sprintf("Auto-idler: %s", err))
	}
	for _, ep := range epList.Items {
		_, idledAtExists := ep.ObjectMeta.Annotations[unidlingapi.IdledAtAnnotation]
		_, targetExists := ep.ObjectMeta.Annotations[unidlingapi.UnidleTargetAnnotation]
		if targetExists && idledAtExists {
			idled = true
			if !IsAsleep {
				glog.V(2).Infof("Project( %s )in idled state", namespace)
			}
			return idled, nil
		}

	}
	idled = false
	glog.V(3).Infof("Project( %s )not currently idled", namespace)
	return idled, nil
}

// The goal with this is to re-create `oc idle` to the extent we need to, in order to provide automatic re-scaling on project wake
// `oc idle` links controllers to endpoints by:
//   1. taking in an endpoint (service name)
//   2. getting the pods on that endpoint
//   3. getting the controller on each pod (and, in the case of DCs, getting the controller on that controller)
// This approach is:
//   1. loop through all services in project
//   2. for each service, get pods on service by label selector
//   3. get the controller on each pod (and, in the case of DCs, get the controller on that controller)
//   4. get endpoint with the same name as the service
func (idler *Idler) autoIdleProjectServices(namespace string) error {
	nowTime := time.Now()
	project, err := idler.resources.GetProject(namespace)
	if err != nil {
		return err
	}
	if project.IsAsleep {
		glog.V(2).Infof("Auto-idler: Project( %s )currently in force-sleep, exiting...", namespace)
		return nil
	}
	glog.V(0).Infof("Auto-idler: Adding previous scale annotation in project( %s )", namespace)
	err = AddProjectPreviousScaleAnnotation(idler.resources, namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("Error adding project( %s )previous scale annotation: %s", namespace, err))
	}
	glog.V(2).Infof("Auto-idler: Scaling DCs in project %s", namespace)
	err = ScaleProjectDCs(idler.resources, namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("Error scaling DCs in project %s: %s", namespace, err))
	}

	glog.V(2).Infof("Auto-idler: Scaling RCs in project %s", namespace)
	err = ScaleProjectRCs(idler.resources, namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("Error scaling RCs in project %s: %s", namespace, err))
	}
	glog.V(2).Infof("Auto-idler: Deleting pods in project %s", namespace)
	err = DeleteProjectPods(idler.resources, namespace)
	if err != nil {
		return errors.New(fmt.Sprintf("Error deleting pods in project %s: %s", namespace, err))
	}
	glog.V(0).Infof("Auto-idler: Adding idled-at annotation in project( %s )", namespace)
	err = AddProjectIdledAtAnnotation(idler.resources, namespace, nowTime, idler.config.ProjectSleepPeriod)
	if err != nil {
		return errors.New(fmt.Sprintf("Error adding idled-at annotation in project( %s ): %v", namespace, err))
	}
	return nil
}
