// +build integ
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

package mcs

import (
	"context"
	"errors"
	"io/ioutil"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	mcsapisClient "sigs.k8s.io/mcs-api/pkg/client/clientset/versioned"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/tests/integration/pilot/common"
)

var (
	i istio.Instance

	// Below are various preconfigured echo deployments. Whenever possible, tests should utilize these
	// to avoid excessive creation/tear down of deployments. In general, a test should only deploy echo if
	// its doing something unique to that specific test.
	apps = &common.EchoDeployments{}
)

func TestMain(m *testing.M) {
	framework.
		NewSuite(m).
		RequireEnvironmentVersion("1.17").
		Setup(istio.Setup(&i, func(ctx resource.Context, cfg *istio.Config) {
			cfg.ControlPlaneValues = `
values:
  pilot:
    env:
      PILOT_ENABLE_MCS_SERVICEEXPORT: "true"`
		})).
		Setup(func(ctx resource.Context) error {
			crd, err := ioutil.ReadFile("../testdata/mcs-serviceexport-crd.yaml")
			if err != nil {
				return err
			}
			err = ctx.Config().ApplyYAML("", string(crd))
			if err != nil {
				return err
			}
			return common.SetupApps(ctx, i, apps)
		}).
		Run()
}

// ensuring that existing routing functionality is unaffected
func TestTraffic(t *testing.T) {
	framework.
		NewTest(t).
		Features("traffic.routing", "traffic.reachability", "traffic.shifting").
		Run(func(ctx framework.TestContext) {
			common.RunAllTrafficTests(ctx, apps)
		})
}

func TestServiceExports(t *testing.T) {
	// 1. assert for the presence of a serviceexport for service `a` in `test-ns1`
	// 2. delete service `a` in `test-ns1` and assert for no serviceexport
	// 3. assert no serviceexports in kube-system

	framework.NewTest(t).
		Features("traffic.mcs.serviceexport").
		RequiresSingleCluster().
		Run(func(ctx framework.TestContext) {
			cluster := ctx.Clusters().Default()

			// restarting the istiod deployment because we short-circuit the serviceexport controller if the CRD isn't there on istiod startup
			istiodPods, err := cluster.CoreV1().Pods("istio-system").List(context.TODO(), v1.ListOptions{
				LabelSelector: "app=istiod",
			})
			if err != nil {
				t.Fatalf("Failed getting istiod pods to restart with error %v", err)
			}
			for _, pod := range istiodPods.Items {
				err = cluster.CoreV1().Pods("istio-system").Delete(context.TODO(), pod.Name, v1.DeleteOptions{})
				if err != nil {
					t.Fatalf("Failed deleting istiod pod with error %v", err)
				}
			}

			podReady := false
			for !podReady {
				istiodPods, err := cluster.CoreV1().Pods("istio-system").List(context.TODO(), v1.ListOptions{
					LabelSelector: "app=istiod",
				})
				if err != nil {
					t.Fatalf("Failed getting istiod pods to restart with error %v", err)
				}
				for _, pod := range istiodPods.Items {
					if pod.Status.ContainerStatuses[0].Ready {
						podReady = true
					}
				}
			}

			// need to give the container some time to get properly spun up
			time.Sleep(30 * time.Second)

			mcsapis, err := mcsapisClient.NewForConfig(cluster.RESTConfig())
			if err != nil {
				t.Fatalf("Failed to get the MCS API client, failing test with error %v", err)
			}

			// creating namespaces and services ourselves
			ns := corev1.Namespace{}
			ns.Name = "svc-namespace"
			_, err = cluster.CoreV1().Namespaces().Create(context.TODO(), &ns, v1.CreateOptions{})

			if err != nil {
				t.Fatalf("Failed during namespace setup with error %v", err)
			}

			svc := corev1.Service{}
			svc.Spec = corev1.ServiceSpec{
				Ports: []corev1.ServicePort{corev1.ServicePort{
					Name:       "http",
					Protocol:   "TCP",
					Port:       80,
					TargetPort: intstr.FromInt(9837),
				}},
			}
			svc.Name = "svc1"
			_, err = cluster.CoreV1().Services("svc-namespace").Create(context.TODO(), &svc, v1.CreateOptions{})

			if err != nil {
				t.Fatalf("Failed during service setup with error %v", err)
			}

			retry.UntilSuccessOrFail(t, func() error {
				serviceExport, err := mcsapis.MulticlusterV1alpha1().ServiceExports("svc-namespace").Get(context.TODO(), "svc1", v1.GetOptions{})
				if err != nil {
					return err
				}

				if serviceExport == nil {
					return errors.New("expected serviceexport not found")
				}

				return nil
			})

			err = cluster.CoreV1().Services("svc-namespace").Delete(context.TODO(), "svc1", v1.DeleteOptions{})

			if err != nil {
				t.Fatalf("Failed manually deleting service with error %v", err)
			}

			retry.UntilSuccessOrFail(t, func() error {
				_, err := mcsapis.MulticlusterV1alpha1().ServiceExports("svc-namespace").Get(context.TODO(), "svc1", v1.GetOptions{})

				if err != nil && k8sErrors.IsNotFound(err) {
					return nil // we don't want a serviceexport to exist in kube-system
				}

				if err != nil {
					return err
				}

				return errors.New("found serviceExport when one should not have existed")
			})

			retry.UntilSuccessOrFail(t, func() error {
				services, err := cluster.CoreV1().Services("kube-system").List(context.TODO(), v1.ListOptions{})
				if err != nil {
					return err
				}

				svcName := services.Items[0].Name

				_, err = mcsapis.MulticlusterV1alpha1().ServiceExports("kube-system").Get(context.TODO(), svcName, v1.GetOptions{})

				if err != nil && k8sErrors.IsNotFound(err) {
					return nil // we don't want a serviceexport to exist in kube-system
				}

				if err != nil {
					return err
				}

				return errors.New("found serviceExport when one should not have been created")
			})

			_ = cluster.CoreV1().Namespaces().Delete(context.TODO(), "svc-namespace", v1.DeleteOptions{})
		})
}
