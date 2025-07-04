//go:build e2e
// +build e2e

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

package shared

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	"sigs.k8s.io/cluster-api/util"
)

func SetupNamespace(ctx context.Context, specName string, e2eCtx *E2EContext) *corev1.Namespace {
	By(fmt.Sprintf("Creating a namespace for hosting the %q test spec", specName))
	namespace := framework.CreateNamespace(ctx, framework.CreateNamespaceInput{
		Creator: e2eCtx.Environment.BootstrapClusterProxy.GetClient(),
		Name:    fmt.Sprintf("%s-%s", specName, util.RandomString(6)),
	})
	return namespace
}

func SetupSpecNamespace(ctx context.Context, specName string, e2eCtx *E2EContext) *corev1.Namespace {
	By(fmt.Sprintf("Creating a namespace for hosting the %q test spec", specName))
	namespace, cancelWatches := framework.CreateNamespaceAndWatchEvents(ctx, framework.CreateNamespaceAndWatchEventsInput{
		Creator:   e2eCtx.Environment.BootstrapClusterProxy.GetClient(),
		ClientSet: e2eCtx.Environment.BootstrapClusterProxy.GetClientSet(),
		Name:      fmt.Sprintf("%s-%s", specName, util.RandomString(6)),
		LogFolder: filepath.Join(e2eCtx.Settings.ArtifactFolder, "clusters", e2eCtx.Environment.BootstrapClusterProxy.GetName()),
	})

	e2eCtx.Environment.Namespaces[namespace] = cancelWatches
	Expect(e2eCtx.E2EConfig).ToNot(BeNil(), "Invalid argument. e2eConfig can't be nil")
	Expect(e2eCtx.E2EConfig.Variables).To(HaveKey(KubernetesVersion))

	return namespace
}

func DumpSpecResourcesAndCleanup(ctx context.Context, specName string, namespace *corev1.Namespace, e2eCtx *E2EContext) {
	By(fmt.Sprintf("Dumping all the Cluster API resources in the %q namespace", namespace.Name))
	// Dump all Cluster API related resources to artifacts before deleting them.
	cancelWatches := e2eCtx.Environment.Namespaces[namespace]
	DumpSpecResources(ctx, e2eCtx, namespace)
	By(fmt.Sprintf("Dumping all EC2 instances in the %q namespace", namespace.Name))
	DumpMachines(ctx, e2eCtx, namespace)
	if !e2eCtx.Settings.SkipCleanup {
		intervals := e2eCtx.E2EConfig.GetIntervals(specName, "wait-delete-cluster")
		By(fmt.Sprintf("Deleting all clusters in the %q namespace with intervals %q", namespace.Name, intervals))
		framework.DeleteAllClustersAndWait(ctx, framework.DeleteAllClustersAndWaitInput{
			ClusterProxy:         e2eCtx.Environment.BootstrapClusterProxy,
			ClusterctlConfigPath: e2eCtx.Environment.ClusterctlConfigPath,
			Namespace:            namespace.Name,
			ArtifactFolder:       e2eCtx.Settings.ArtifactFolder,
		}, intervals...)

		By(fmt.Sprintf("Deleting namespace used for hosting the %q test spec", specName))
		framework.DeleteNamespace(ctx, framework.DeleteNamespaceInput{
			Deleter: e2eCtx.Environment.BootstrapClusterProxy.GetClient(),
			Name:    namespace.Name,
		})
	}
	if cancelWatches != nil {
		cancelWatches()
	}
	delete(e2eCtx.Environment.Namespaces, namespace)
}

// AWSStackLogCollector collects logs from the AWS stack.
type AWSStackLogCollector struct {
	E2EContext *E2EContext
}

// CollectInfrastructureLogs collects log from the infrastructure.
func (k AWSStackLogCollector) CollectInfrastructureLogs(_ context.Context, _ crclient.Client, _ *clusterv1.Cluster, _ string) error {
	return nil
}

func DumpMachines(ctx context.Context, e2eCtx *E2EContext, namespace *corev1.Namespace) {
	machines := MachinesForSpec(ctx, e2eCtx.Environment.BootstrapClusterProxy, namespace)
	instances, err := allMachines(ctx, e2eCtx)
	if err != nil {
		return
	}
	instanceID := ""
	for _, m := range machines.Items {
		for _, i := range instances {
			if i.name == m.Name {
				instanceID = i.instanceID
				break
			}
		}
		if instanceID == "" {
			return
		}
		DumpMachine(ctx, e2eCtx, m, instanceID, nil)
	}
}

func DumpMachinesFromProxy(ctx context.Context, e2eCtx *E2EContext, namespace *corev1.Namespace, proxy framework.ClusterProxy) {
	machines := MachinesForSpec(ctx, proxy, namespace)
	instances, err := allMachines(ctx, e2eCtx)
	if err != nil {
		return
	}
	instanceID := ""
	for _, m := range machines.Items {
		for _, i := range instances {
			if i.name == m.Name {
				instanceID = i.instanceID
				break
			}
		}
		if instanceID == "" {
			return
		}
		clusterName := proxy.GetName()
		DumpMachine(ctx, e2eCtx, m, instanceID, &clusterName)
	}
}

func MachinesForSpec(ctx context.Context, clusterProxy framework.ClusterProxy, namespace *corev1.Namespace) *infrav1.AWSMachineList {
	lister := clusterProxy.GetClient()
	list := new(infrav1.AWSMachineList)
	if err := lister.List(ctx, list, crclient.InNamespace(namespace.GetName())); err != nil {
		fmt.Fprintln(GinkgoWriter, "couldn't find machines")
		return nil
	}
	return list
}

func DumpMachine(ctx context.Context, e2eCtx *E2EContext, machine infrav1.AWSMachine, instanceID string, cluster *string) {
	logPath := filepath.Join(e2eCtx.Settings.ArtifactFolder, "clusters", e2eCtx.Environment.BootstrapClusterProxy.GetName())
	if cluster != nil {
		logPath = filepath.Join(e2eCtx.Settings.ArtifactFolder, "clusters", *cluster)
	}
	machineLogBase := path.Join(logPath, "instances", machine.Namespace, machine.Name)
	metaLog := path.Join(machineLogBase, "instance.log")
	if err := os.MkdirAll(filepath.Dir(metaLog), 0o750); err != nil {
		fmt.Fprintf(GinkgoWriter, "Couldn't create directory for file: path=%q, err=%s\n", metaLog, err)
	}
	f, err := os.OpenFile(metaLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "instance found: instance-id=%q\n", instanceID)
	commandsForMachine(
		ctx,
		e2eCtx,
		f,
		instanceID,
		[]command{
			{
				title: "systemd",
				cmd:   "journalctl --no-pager --output=short-precise | grep -v  'audit:\\|audit\\['",
			},
			{
				title: "kern",
				cmd:   "journalctl --no-pager --output=short-precise -k",
			},
			{
				title: "containerd-info",
				cmd:   "crictl info",
			},
			{
				title: "cloud-final",
				cmd:   "journalctl --no-pager -u cloud-final",
			},
			{
				title: "kubelet",
				cmd:   "journalctl --no-pager -u kubelet.service",
			},
			{
				title: "containerd",
				cmd:   "journalctl --no-pager -u containerd.service",
			},
		},
	)
}

func DumpSpecResources(ctx context.Context, e2eCtx *E2EContext, namespace *corev1.Namespace) {
	framework.DumpAllResources(ctx, framework.DumpAllResourcesInput{
		KubeConfigPath:       e2eCtx.Environment.BootstrapClusterProxy.GetKubeconfigPath(),
		ClusterctlConfigPath: e2eCtx.Environment.ClusterctlConfigPath,
		Lister:               e2eCtx.Environment.BootstrapClusterProxy.GetClient(),
		Namespace:            namespace.Name,
		LogPath:              filepath.Join(e2eCtx.Settings.ArtifactFolder, "clusters", e2eCtx.Environment.BootstrapClusterProxy.GetName(), "resources"),
	})
}

func DumpSpecResourcesFromProxy(ctx context.Context, e2eCtx *E2EContext, namespace *corev1.Namespace, proxy framework.ClusterProxy) {
	framework.DumpAllResources(ctx, framework.DumpAllResourcesInput{
		KubeConfigPath:       proxy.GetKubeconfigPath(),
		ClusterctlConfigPath: e2eCtx.Environment.ClusterctlConfigPath,
		Lister:               proxy.GetClient(),
		Namespace:            namespace.Name,
		LogPath:              filepath.Join(e2eCtx.Settings.ArtifactFolder, "clusters", proxy.GetName(), "resources"),
	})
}

// ConditionFn returns true if a condition exists.
type ConditionFn func() bool

// ConditionalIt will only perform the It block if the condition function returns true
// Inspired from Cilium: https://github.com/cilium/cilium/blob/03bfb2bece5108549b3d613e119059758035d448/test/ginkgo-ext/scopes.go#L658
func ConditionalIt(conditionFn ConditionFn, text string, body func()) bool {
	if conditionFn() {
		return It(text, body)
	}

	return It(text, func() {
		Skip("skipping due to unmet condition")
	})
}

// LoadE2EConfig loads the e2econfig from the specified path.
func LoadE2EConfig(configPath string) *clusterctl.E2EConfig {
	// TODO: This is commented out as it assumes kubeadm and errors if its not there
	// Remove localLoadE2EConfig and use the line below when this issue is resolved:
	// https://github.com/kubernetes-sigs/cluster-api/issues/3983
	// config := clusterctl.LoadE2EConfig(context.TODO(), clusterctl.LoadE2EConfigInput{ConfigPath: configPath})
	config := localLoadE2EConfig(configPath)

	Expect(config).ToNot(BeNil(), "Failed to load E2E config from %s", configPath)
	return config
}

// SetEnvVar sets an environment variable in the process. If marked private,
// the value is not printed.
func SetEnvVar(key, value string, private bool) {
	printableValue := "*******"
	if !private {
		printableValue = value
	}

	fmt.Fprintf(GinkgoWriter, time.Now().Format(time.StampMilli)+": "+"INFO"+": "+"Setting environment variable: key=%s, value=%s"+"\n", key, printableValue)
	os.Setenv(key, value)
}

func CreateAWSClusterControllerIdentity(k8sclient crclient.Client) {
	controllerIdentity := &infrav1.AWSClusterControllerIdentity{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infrav1.GroupVersion.String(),
			Kind:       string(infrav1.ControllerIdentityKind),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: infrav1.AWSClusterControllerIdentityName,
		},
		Spec: infrav1.AWSClusterControllerIdentitySpec{
			AWSClusterIdentitySpec: infrav1.AWSClusterIdentitySpec{
				AllowedNamespaces: &infrav1.AllowedNamespaces{},
			},
		},
	}
	_ = k8sclient.Create(context.TODO(), controllerIdentity)
}

func Byf(format string, a ...interface{}) {
	By(fmt.Sprintf(format, a...))
}
