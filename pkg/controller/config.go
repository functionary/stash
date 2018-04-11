package controller

import (
	"time"

	hooks "github.com/appscode/kubernetes-webhook-util/admission/v1beta1"
	cs "github.com/appscode/stash/client/clientset/versioned"
	stashinformers "github.com/appscode/stash/client/informers/externalversions"
	"github.com/appscode/stash/pkg/eventer"
	core "k8s.io/api/core/v1"
	crd_cs "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Config struct {
	EnableRBAC     bool
	StashImageTag  string
	DockerRegistry string
	MaxNumRequeues int
	NumThreads     int
	OpsAddress     string
	ResyncPeriod   time.Duration
}

type ControllerConfig struct {
	Config

	ClientConfig   *rest.Config
	KubeClient     kubernetes.Interface
	StashClient    cs.Interface
	CRDClient      crd_cs.ApiextensionsV1beta1Interface
	AdmissionHooks []hooks.AdmissionHook
}

func NewControllerConfig(clientConfig *rest.Config) *ControllerConfig {
	return &ControllerConfig{
		ClientConfig: clientConfig,
	}
}

func (c *ControllerConfig) New() (*StashController, error) {
	tweakListOptions := func(opt *metav1.ListOptions) {
		opt.IncludeUninitialized = true
	}
	ctrl := &StashController{
		Config:               c.Config,
		kubeClient:           c.KubeClient,
		stashClient:          c.StashClient,
		crdClient:            c.CRDClient,
		kubeInformerFactory:  informers.NewFilteredSharedInformerFactory(c.KubeClient, c.ResyncPeriod, core.NamespaceAll, tweakListOptions),
		stashInformerFactory: stashinformers.NewSharedInformerFactory(c.StashClient, c.ResyncPeriod),
		recorder:             eventer.NewEventRecorder(c.KubeClient, "stash-controller"),
	}

	if err := ctrl.ensureCustomResourceDefinitions(); err != nil {
		return nil, err
	}
	if ctrl.EnableRBAC {
		if err := ctrl.ensureSidecarClusterRole(); err != nil {
			return nil, err
		}
	}

	ctrl.initNamespaceWatcher()
	ctrl.initResticWatcher()
	ctrl.initRecoveryWatcher()
	ctrl.initDeploymentWatcher()
	ctrl.initDeploymentConfigWatcher()
	ctrl.initDaemonSetWatcher()
	ctrl.initStatefulSetWatcher()
	ctrl.initRCWatcher()
	ctrl.initReplicaSetWatcher()
	ctrl.initJobWatcher()

	return ctrl, nil
}
