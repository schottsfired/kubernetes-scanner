/*
 * © 2023 Snyk Limited
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/exp/slices"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	k8sdiscovery "k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

type Config struct {
	Scanning       Scan   `json:"scanning"`
	MetricsAddress string `json:"metricsAddress"`
	ProbeAddress   string `json:"probeAddress"`
	// OrganizationID is the snyk organization ID where data should be routed to.
	OrganizationID string `json:"organizationID"`

	// Egress contains configuration for everything that's related to sending data to Snyk's
	// backend.
	Egress *Egress `json:"egress"`

	// ClusterName should be the "friendly" name of the cluster where the scanner is running in.
	// For example, "prod-us" or "dev-eu."
	ClusterName string `json:"clusterName"`

	Scheme     *runtime.Scheme `json:"-"`
	RestConfig *rest.Config    `json:"-"`
}

type Egress struct {
	// HTTPClientTimeout sets the timeout for the HTTP client that is being used for connections to
	// the Snyk backend.
	HTTPClientTimeout metav1.Duration `json:"httpClientTimeout"`

	// SnykAPIBaseURL defines the endpoint where the scanner will send data to.
	SnykAPIBaseURL string `json:"snykAPIBaseURL"`

	// SnykServiceAccountToken is the token of the Snyk Service Account. Is not read from the config
	// file, can only be set through the environment variable.
	SnykServiceAccountToken string `json:"-" env:"SNYK_SERVICE_ACCOUNT_TOKEN"`
}

func (e Egress) validate() error {
	url, err := url.Parse(e.SnykAPIBaseURL)
	if err != nil {
		return fmt.Errorf("could not parse Snyk API Base URL %v: %w", e.SnykAPIBaseURL, err)
	}

	if url.Scheme == "" {
		return fmt.Errorf("Snyk API Base URL has no scheme set")
	}

	if e.SnykServiceAccountToken == "" {
		return fmt.Errorf("no Snyk service account token set")
	}

	return nil
}

// default values for config settings
const (
	// HTTPClientDefaultTimeout is the default value for the HTTPClientTimeout setting.
	HTTPClientDefaultTimeout = 5 * time.Second

	// SnykAPIDefaultBaseURL is the default endpoint that the scanner will talk to.
	SnykAPIDefaultBaseURL = "https://app.snyk.io"
)

type Scan struct {
	Types []ScanType `json:"types"`
	// RequeueAfter defines the duration after which an object is requeued when we've visited it.
	// Note that due to the event handlers, objects that are being changed will be requeued earlier
	// in such cases.
	RequeueAfter metav1.Duration `json:"requeueAfter"`
}

// Read reads the config file from the specificied flag "-config" and returns a
// struct that contains all options, including other flags.
func Read(configFile string) (*Config, error) {
	if configFile == "" {
		return nil, fmt.Errorf("no config file set!")
	}

	b, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("could not read config file: %w", err)
	}

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("could not get kubernetes REST config: %v", err)
	}

	c := &Config{
		Scheme:     runtime.NewScheme(),
		RestConfig: restCfg,
		Egress: &Egress{
			HTTPClientTimeout:       metav1.Duration{Duration: HTTPClientDefaultTimeout},
			SnykAPIBaseURL:          SnykAPIDefaultBaseURL,
			SnykServiceAccountToken: os.Getenv("SNYK_SERVICE_ACCOUNT_TOKEN"),
		},
	}

	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("could not unmarshal config file: %w", err)
	}

	if c.OrganizationID == "" {
		return nil, fmt.Errorf("organization ID is missing in config file")
	}

	if err := c.Egress.validate(); err != nil {
		return nil, fmt.Errorf("could not validate egress settings: %w", err)
	}

	return c, nil
}

type Discovery interface {
	preferredVersionForGroup(string) (string, error)
	versionsForGroup(string) ([]string, error)
	findGVK(schema.GroupVersionResource) (schema.GroupVersionKind, error)
}

type ScanType struct {
	// TODO: The "*" group / resource specifier isn't implemented yet (and maybe never will).
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	// Versions is an optional field to specify which exact versions should be scanned. If unset,
	// the scanner will use the API Server's preferred version.
	Versions []string `json:"versions"`
	// Namespaces allows to restrict scanning to specific namespaces. An empty list means all
	// namespaces.
	Namespaces []string `json:"namespaces"`
}

// GetGVKs returns all the GVKs that are defined in the ScanType and are available on the server.
func (st ScanType) GetGVKs(d Discovery, log logr.Logger) ([]schema.GroupVersionKind, error) {
	var gvks []schema.GroupVersionKind
	for _, group := range st.APIGroups {
		versions, err := st.getVersions(group, d)
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				return nil, fmt.Errorf("could not get versions for group %v: %w", group, err)
			}

			log.Info("skipping group as it does not exist", "group", group)
			continue
		}

		for _, version := range versions {
			for _, resource := range st.Resources {
				gvr := schema.GroupVersionResource{
					Group:    group,
					Version:  version,
					Resource: resource,
				}
				gvk, err := d.findGVK(gvr)
				if err != nil {
					if !k8serrors.IsNotFound(err) {
						return nil, fmt.Errorf("could not get GVK for GVR %v: %w", gvr, err)
					}

					log.Info("skipping GVR as resource does not exist within groupversion",
						"group", group, "resource", resource, "version", version)
					continue
				}
				gvks = append(gvks, gvk)
			}
		}
	}

	return gvks, nil
}

func (st ScanType) getVersions(group string, d Discovery) ([]string, error) {
	switch {
	case len(st.Versions) == 0:
		version, err := d.preferredVersionForGroup(group)
		if err != nil {
			return nil, fmt.Errorf("could not get preferred version for group %v: %w", group, err)
		}
		return []string{version}, nil

	case slices.Contains(st.Versions, "*"):
		return d.versionsForGroup(group)

	default:
		return st.Versions, nil
	}
}

func (c *Config) Discovery() (Discovery, error) {
	// TODO: should we cache a discovery helper?
	cs, err := kubernetes.NewForConfig(c.RestConfig)
	if err != nil {
		return nil, fmt.Errorf("could not create kubernetes clientset: %w", err)
	}

	discovery, err := newDiscoveryHelper(cs.Discovery())
	if err != nil {
		return nil, fmt.Errorf("could not create discovery helper: %w", err)
	}

	return discovery, nil
}

type discoveryHelper struct {
	k8sdiscovery.DiscoveryInterface
	// to cache all group versions that we retrieve.
	resourcesForGroupVersion map[schema.GroupVersion][]metav1.APIResource
	groups                   []metav1.APIGroup
}

func newDiscoveryHelper(discoveryClient k8sdiscovery.DiscoveryInterface) (*discoveryHelper, error) {
	groups, err := discoveryClient.ServerGroups()
	if err != nil {
		return nil, err
	}

	return &discoveryHelper{
		DiscoveryInterface:       discoveryClient,
		resourcesForGroupVersion: make(map[schema.GroupVersion][]metav1.APIResource),
		groups:                   groups.Groups,
	}, nil
}

func (d *discoveryHelper) preferredVersionForGroup(apiGroup string) (string, error) {
	for _, group := range d.groups {
		if group.Name == apiGroup {
			return group.PreferredVersion.Version, nil
		}
	}
	return "", newNotFoundError(schema.GroupVersionResource{Group: apiGroup})
}
func (d *discoveryHelper) versionsForGroup(apiGroup string) ([]string, error) {
	for _, group := range d.groups {
		if group.Name == apiGroup {
			var versions []string
			for _, ver := range group.Versions {
				versions = append(versions, ver.Version)
			}
			return versions, nil
		}
	}
	return nil, fmt.Errorf("group %v does not exist", apiGroup)
}

// findGVK returns the GroupVersionKind for the given GVR.
func (d *discoveryHelper) findGVK(gvr schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	gv := gvr.GroupVersion()

	resources, ok := d.resourcesForGroupVersion[gv]
	if !ok {
		list, err := d.ServerResourcesForGroupVersion(gv.String())
		if err != nil {
			return schema.GroupVersionKind{}, fmt.Errorf(
				"could not get server resources for groupversion %v: %w", gv, err,
			)
		}

		d.resourcesForGroupVersion[gv] = list.APIResources
		resources = list.APIResources
	}

	for _, res := range resources {
		if res.Name == gvr.Resource {
			return schema.GroupVersionKind{
				Group:   gv.Group,
				Version: gv.Version,
				Kind:    res.Kind,
			}, nil
		}
	}

	return schema.GroupVersionKind{}, newNotFoundError(gvr)
}

func newNotFoundError(gvr schema.GroupVersionResource) error {
	return k8serrors.NewNotFound(gvr.GroupResource(), gvr.Resource)
}
