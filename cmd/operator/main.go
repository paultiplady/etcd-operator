// Copyright 2016 The etcd-operator Authors
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

package main

import (
	//"context"
	"flag"
	//"fmt"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/paultiplady/gcp-operator/pkg/controller"
	"github.com/paultiplady/gcp-operator/pkg/garbagecollection"
	"github.com/paultiplady/gcp-operator/pkg/util/k8sutil"
	"github.com/paultiplady/gcp-operator/pkg/util/k8sutil/election"
	"github.com/paultiplady/gcp-operator/pkg/util/k8sutil/election/resourcelock"
	"github.com/paultiplady/gcp-operator/version"

	"github.com/Sirupsen/logrus"
	"github.com/paultiplady/gcp-operator/pkg/util/retryutil"
	//"golang.org/x/time/rate"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	//"k8s.io/client-go/pkg/labels"
	"k8s.io/client-go/tools/record"
	"github.com/paultiplady/gcp-operator/pkg/util/googutil"
)

var (
	//analyticsEnabled bool
	//pvProvisioner    string
	namespace        string
	name             string
	//awsSecret        string
	//awsConfig        string
	//s3Bucket         string
	gcInterval       time.Duration
)

var (
	leaseDuration = 15 * time.Second
	renewDuration = 5 * time.Second
	retryPeriod   = 3 * time.Second
)

func init() {
	//flag.BoolVar(&analyticsEnabled, "analytics", true, "Send analytical event (Cluster Created/Deleted etc.) to Google Analytics")
	//
	//flag.StringVar(&pvProvisioner, "pv-provisioner", "kubernetes.io/gce-pd", "persistent volume provisioner type")
	//flag.StringVar(&awsSecret, "backup-aws-secret", "",
	//	"The name of the kube secret object that stores the AWS credential file. The file name must be 'credentials'.")
	//flag.StringVar(&awsConfig, "backup-aws-config", "",
	//	"The name of the kube configmap object that stores the AWS config file. The file name must be 'config'.")
	//flag.StringVar(&s3Bucket, "backup-s3-bucket", "", "The name of the AWS S3 bucket to store backups in.")
	//// chaos level will be removed once we have a formal tool to inject failures.
	//flag.IntVar(&chaosLevel, "chaos-level", -1, "DO NOT USE IN PRODUCTION - level of chaos injected into the etcd clusters created by the operator.")
	//flag.BoolVar(&printVersion, "version", false, "Show version and quit")
	flag.DurationVar(&gcInterval, "gc-interval", 10*time.Minute, "GC interval")
	flag.Parse()

	// Workaround for watching TPR resource.
	restCfg, err := k8sutil.InClusterConfig()
	if err != nil {
		panic(err)
	}
	controller.MasterHost = restCfg.Host
	restcli, err := k8sutil.NewTPRClient()
	if err != nil {
		panic(err)
	}
	controller.KubeHttpCli = restcli.Client
}

func main() {
	namespace = os.Getenv("MY_POD_NAMESPACE")
	if len(namespace) == 0 {
		logrus.Fatalf("must set env MY_POD_NAMESPACE")
	}
	name = os.Getenv("MY_POD_NAME")
	if len(name) == 0 {
		logrus.Fatalf("must set env MY_POD_NAME")
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c)
	go func() {
		logrus.Infof("received signal: %v", <-c)
		os.Exit(1)
	}()

	//if printVersion {
	//	fmt.Println("etcd-operator Version:", version.Version)
	//	fmt.Println("Git SHA:", version.GitSHA)
	//	fmt.Println("Go Version:", runtime.Version())
	//	fmt.Printf("Go OS/Arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	//	os.Exit(0)
	//}

	logrus.Infof("etcd-operator Version: %v", version.Version)
	logrus.Infof("Git SHA: %s", version.GitSHA)
	logrus.Infof("Go Version: %s", runtime.Version())
	logrus.Infof("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH)

	id, err := os.Hostname()
	if err != nil {
		logrus.Fatalf("failed to get hostname: %v", err)
	}

	// TODO: replace this to client-go once leader election pacakge is imported
	//       https://github.com/kubernetes/client-go/issues/28
	rl := &resourcelock.EndpointsLock{
		EndpointsMeta: api.ObjectMeta{
			Namespace: namespace,
			Name:      "gcp-operator",
		},
		Client: k8sutil.MustNewKubeClient(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: &record.FakeRecorder{},
		},
	}

	election.RunOrDie(election.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDuration,
		RetryPeriod:   retryPeriod,
		Callbacks: election.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				logrus.Fatalf("leader election lost")
			},
		},
	})
	panic("unreachable")
}

func run(stop <-chan struct{}) {
	cfg := newControllerConfig()

	go periodicFullGC(cfg.KubeClient, cfg.Namespace, gcInterval)

	for {
		c := controller.New(cfg)
		err := c.Run()
		switch err {
		case controller.ErrVersionOutdated:
		default:
			logrus.Fatalf("controller Run() ended with failure: %v", err)
		}
	}
}

func newControllerConfig() controller.Config {
	kubeClient := k8sutil.MustNewKubeClient()
	googleClient := googutil.NewGoogleClientOrDie()

	serviceAccount, err := getMyPodServiceAccount(kubeClient)
	if err != nil {
		logrus.Fatalf("fail to get my pod's service account: %v", err)
	}

	cfg := controller.Config{
		Namespace:      namespace,
		ServiceAccount: serviceAccount,
		KubeClient: kubeClient,
		GoogleClient: googleClient,
	}

	return cfg
}

func getMyPodServiceAccount(kubeClient kubernetes.Interface) (string, error) {
	var sa string
	err := retryutil.Retry(5*time.Second, 100, func() (bool, error) {
		pod, err := kubeClient.CoreV1().Pods(namespace).Get(name)
		if err != nil {
			logrus.Errorf("fail to get operator pod (%s): %v", name, err)
			return false, nil
		}
		sa = pod.Spec.ServiceAccountName
		return true, nil
	})
	return sa, err
}

func periodicFullGC(kubecli kubernetes.Interface, ns string, d time.Duration) {
	gc := garbagecollection.New(kubecli, ns)
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		<-timer.C
		err := gc.FullyCollect()
		if err != nil {
			logrus.Warningf("failed to cleanup resources: %v", err)
		}
	}
}
