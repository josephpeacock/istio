// Copyright Istio Authors
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

package controller

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
	"sigs.k8s.io/mcs-api/pkg/client/clientset/versioned"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/queue"
	"istio.io/pkg/log"
)

type ServiceExportController struct {
	client        versioned.Interface
	serviceClient corev1.CoreV1Interface

	queue           queue.Instance
	serviceInformer cache.SharedInformer

	environment *model.Environment

	// We use this flag to short-circuit the logic and stop the controller if the CRD does not exist (or is deleted)
	crdExists bool
}

func NewServiceExportController(kubeClient kube.Client, environment *model.Environment) (*ServiceExportController, error) {
	serviceExportController := &ServiceExportController{
		client:        kubeClient.MCSApis(),
		serviceClient: kubeClient.Kube().CoreV1(),
		queue:         queue.NewQueue(time.Second),
		environment:   environment,
		crdExists:     true,
	}

	serviceExportController.serviceInformer = kubeClient.KubeInformer().Core().V1().Services().Informer()
	serviceExportController.serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { serviceExportController.onServiceAdd(obj) },
		DeleteFunc: func(obj interface{}) { serviceExportController.onServiceDelete(obj) },
	})

	return serviceExportController, nil
}

func (sc *ServiceExportController) onServiceAdd(obj interface{}) {
	sc.queue.Push(func() error {
		serviceObj, err := convertToService(obj)
		if err != nil {
			return err
		}
		return sc.handleNewService(serviceObj)
	})
}

func (sc *ServiceExportController) onServiceDelete(obj interface{}) {
	sc.queue.Push(func() error {
		serviceObj, err := convertToService(obj)
		if err != nil {
			return err
		}
		return sc.handleDeletedService(serviceObj)
	})
}

func (sc *ServiceExportController) Run(stopCh <-chan struct{}) {
	cache.WaitForCacheSync(stopCh, sc.serviceInformer.HasSynced)
	log.Infof("Syncing existing services and serviceexports...")
	sc.doInitialFullSync()
	log.Infof("serviceexport sync complete")
	log.Infof("ServiceExport controller started")
	go sc.queue.Run(stopCh)
}

func (sc *ServiceExportController) handleNewService(obj *v1.Service) error {
	if !sc.crdExists {
		return nil
	}
	if sc.environment.IsServiceClusterLocal(obj) {
		return nil // don't do anything for marked clusterlocal services
	}
	return sc.createServiceExportIfNotPresent(obj)
}

func (sc *ServiceExportController) handleDeletedService(obj *v1.Service) error {
	return sc.deleteServiceExportIfPresent(obj)
}

func (sc *ServiceExportController) createServiceExportIfNotPresent(service *v1.Service) error {
	serviceExport := v1alpha1.ServiceExport{}
	serviceExport.Namespace = service.Namespace
	serviceExport.Name = service.Name

	_, err := sc.client.MulticlusterV1alpha1().ServiceExports(service.Namespace).Create(context.TODO(), &serviceExport, metav1.CreateOptions{})

	if err != nil && errors.IsAlreadyExists(err) {
		err = nil // This is the error thrown by the client if there is already an object with the name in the namespace. If that's true, we do nothing
	}

	if err != nil && errors.IsNotFound(err) {
		log.Errorf("ServiceExport CRD Not found, shutting down MCS ServiceExport sync. Please add the CRD then restart the istiod deployment")
		sc.crdExists = false
	}
	return err
}

func (sc *ServiceExportController) deleteServiceExportIfPresent(service *v1.Service) error {
	// cannot use the auto-generated client as it hardcodes the namespace in the client struct, and we can't have one client per watched ns
	err := sc.client.MulticlusterV1alpha1().ServiceExports(service.Namespace).Delete(context.TODO(), service.Name, metav1.DeleteOptions{})

	if err != nil && errors.IsNotFound(err) {
		err = nil // If it's already gone, then we're happy
	}
	return err
}

func (sc *ServiceExportController) doInitialFullSync() {
	services := sc.serviceInformer.GetStore().List()
	for _, service := range services {
		sc.onServiceAdd(service)
	}
}
