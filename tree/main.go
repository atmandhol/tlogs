package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // combined authprovider import
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
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
		api = k
	} else {
		apiResults := apis.lookup(kind)
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
	return nil
}

// overrideType hardcodes lookup overrides for certain service types
func overrideType(kind string, v *resourceMap) (apiResource, bool) {
	kind = strings.ToLower(kind)

	switch kind {
	case "svc", "service", "services": // Knative also registers "Service", prefer v1.Service
		out := v.lookup("service.v1.")
		if len(out) != 0 {
			return out[0], true
		}

	case "deploy", "deployment", "deployments": // most clusters will have Deployment in apps/v1 and extensions/v1beta1, extensions/v1/beta2
		out := v.lookup("deployment.v1.apps")
		if len(out) != 0 {
			return out[0], true
		}
		out = v.lookup("deployment.v1beta1.extensions")
		if len(out) != 0 {
			return out[0], true
		}
	}
	return apiResource{}, false
}

// objectDirectory stores objects and owner relationships between them.
type objectDirectory struct {
	items     map[types.UID]unstructured.Unstructured
	ownership map[types.UID]map[types.UID]bool
}

// newObjectDirectory builds object lookup and hierarchy.
func newObjectDirectory(objs []unstructured.Unstructured) objectDirectory {
	v := objectDirectory{
		items:     make(map[types.UID]unstructured.Unstructured),
		ownership: make(map[types.UID]map[types.UID]bool),
	}
	for _, obj := range objs {
		v.items[obj.GetUID()] = obj
		for _, ownerRef := range obj.GetOwnerReferences() {
			if v.ownership[ownerRef.UID] == nil {
				v.ownership[ownerRef.UID] = make(map[types.UID]bool)
			}
			v.ownership[ownerRef.UID][obj.GetUID()] = true
		}
	}
	return v
}

// getAllResources finds all API objects in specified API resources in all namespaces (or non-namespaced).
func getAllResources(client dynamic.Interface, apis []apiResource, ns string) ([]unstructured.Unstructured, error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []unstructured.Unstructured

	var errResult error
	for _, api := range apis {
		if !api.r.Namespaced {
			continue
		}
		wg.Add(1)
		go func(a apiResource) {
			defer wg.Done()
			v, err := queryAPI(client, a, ns)
			if err != nil {
				errResult = err
				return
			}
			mu.Lock()
			out = append(out, v...)
			mu.Unlock()
		}(api)
	}

	wg.Wait()
	return out, errResult
}

func queryAPI(client dynamic.Interface, api apiResource, ns string) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured

	var next string

	for {
		var intf dynamic.ResourceInterface
		nintf := client.Resource(api.GroupVersionResource())
		intf = nintf.Namespace(ns)
		resp, err := intf.List(context.TODO(), metav1.ListOptions{
			Limit:    250,
			Continue: next,
		})
		if err != nil {
			return nil, fmt.Errorf("listing resources failed (%s): %w", api.GroupVersionResource(), err)
		}
		out = append(out, resp.Items...)

		next = resp.GetContinue()
		if next == "" {
			break
		}
	}
	return out, nil
}

type apiResource struct {
	r  metav1.APIResource
	gv schema.GroupVersion
}

func (a apiResource) GroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    a.gv.Group,
		Version:  a.gv.Version,
		Resource: a.r.Name,
	}
}

type resourceNameLookup map[string][]apiResource

type resourceMap struct {
	list []apiResource
	m    resourceNameLookup
}

func (rm *resourceMap) lookup(s string) []apiResource {
	return rm.m[strings.ToLower(s)]
}

func (rm *resourceMap) resources() []apiResource { return rm.list }

func fullAPIName(a apiResource) string {
	sgv := a.GroupVersionResource()
	return strings.Join([]string{sgv.Resource, sgv.Version, sgv.Group}, ".")
}

func findAPIs(client discovery.DiscoveryInterface) (*resourceMap, error) {
	resList, err := client.ServerPreferredResources()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch api groups from kubernetes: %w", err)
	}

	rm := &resourceMap{
		m: make(resourceNameLookup),
	}
	for _, group := range resList {
		gv, err := schema.ParseGroupVersion(group.GroupVersion)
		if err != nil {
			return nil, fmt.Errorf("%q cannot be parsed into groupversion: %w", group.GroupVersion, err)
		}

		for _, apiRes := range group.APIResources {
			if !contains(apiRes.Verbs, "list") {
				continue
			}
			v := apiResource{
				gv: gv,
				r:  apiRes,
			}
			names := apiNames(apiRes, gv)
			for _, name := range names {
				rm.m[name] = append(rm.m[name], v)
			}
			rm.list = append(rm.list, v)
		}
	}
	return rm, nil
}

func contains(v []string, s string) bool {
	for _, vv := range v {
		if vv == s {
			return true
		}
	}
	return false
}

// return all names that could refer to this APIResource
func apiNames(a metav1.APIResource, gv schema.GroupVersion) []string {
	var out []string
	singularName := a.SingularName
	if singularName == "" {
		// TODO(ahmetb): sometimes SingularName is empty (e.g. Deployment), use lowercase Kind as fallback - investigate why
		singularName = strings.ToLower(a.Kind)
	}
	pluralName := a.Name
	shortNames := a.ShortNames
	names := append([]string{singularName, pluralName}, shortNames...)
	for _, n := range names {
		fmtBare := n                                                                // e.g. deployment
		fmtWithGroup := strings.Join([]string{n, gv.Group}, ".")                    // e.g. deployment.apps
		fmtWithGroupVersion := strings.Join([]string{n, gv.Version, gv.Group}, ".") // e.g. deployment.v1.apps

		out = append(out,
			fmtBare, fmtWithGroup, fmtWithGroupVersion)
	}
	return out
}

func main() {
	run("workload", "petc", "ns1")
}
