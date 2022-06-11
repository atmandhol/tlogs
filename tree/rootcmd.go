package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // combined authprovider import
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog"
)

func run(kind string, name string, ns string) error {

	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	config.QPS = 1000
	config.Burst = 1000
	if err != nil {
		panic(err)
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	dc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return err
	}

	apis, err := findAPIs(dc)
	if err != nil {
		return err
	}

	var api apiResource
	if k, ok := overrideType(kind, apis); ok {
		klog.V(2).Infof("kind=%s override found: %s", kind, k.GroupVersionResource())
		api = k
	} else {
		apiResults := apis.lookup(kind)
		klog.V(5).Infof("kind matches=%v", apiResults)
		if len(apiResults) == 0 {
			return fmt.Errorf("could not find api kind %q", kind)
		} else if len(apiResults) > 1 {
			names := make([]string, 0, len(apiResults))
			for _, a := range apiResults {
				names = append(names, fullAPIName(a))
			}
			return fmt.Errorf("ambiguous kind %q. use one of these as the KIND disambiguate: [%s]", kind,
				strings.Join(names, ", "))
		}
		api = apiResults[0]
	}

	var ri dynamic.ResourceInterface
	if api.r.Namespaced {
		ri = dyn.Resource(api.GroupVersionResource()).Namespace(ns)
	} else {
		ri = dyn.Resource(api.GroupVersionResource())
	}
	obj, err := ri.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get %s/%s: %w", kind, name, err)
	}

	apiObjects, err := getAllResources(dyn, apis.resources(), ns)
	if err != nil {
		return fmt.Errorf("error while querying api objects: %w", err)
	}

	objs := newObjectDirectory(apiObjects)
	if len(objs.ownership[obj.GetUID()]) == 0 {
		fmt.Println("No resources are owned by this object through ownerReferences.")
		return nil
	}
	fmt.Println(objs)
	// treeView(os.Stderr, objs, *obj)
	return nil
}

func main() {
	run("workload", "petc", "ns1")
}
