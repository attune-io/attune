/*
Copyright 2026.

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

package metrics

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
)

// OpenShift TLS profile names as defined by config.openshift.io/v1.
const (
	TLSProfileOld          = "Old"
	TLSProfileIntermediate = "Intermediate"
	TLSProfileModern       = "Modern"
	TLSProfileCustom       = "Custom"
)

// TLSMinVersionForProfile maps an OpenShift TLS profile name to the
// corresponding Go tls.Config MinVersion. Returns 0 for unrecognized
// profiles, which means "use Go defaults" (TLS 1.2).
func TLSMinVersionForProfile(profile string) uint16 {
	switch profile {
	case TLSProfileModern:
		return tls.VersionTLS13
	case TLSProfileIntermediate, "":
		return tls.VersionTLS12
	case TLSProfileOld:
		return tls.VersionTLS10 //nolint:gosec // mirrors OpenShift "Old" profile
	default:
		return 0
	}
}

// openshiftAPIServer is a minimal representation of the OpenShift
// apiserver.config.openshift.io/v1 resource, enough to extract the
// TLS profile type.
type openshiftAPIServer struct {
	Spec struct {
		TLSSecurityProfile *struct {
			Type string `json:"type"`
		} `json:"tlsSecurityProfile"`
	} `json:"spec"`
}

// DetectOpenShiftTLSProfile reads the OpenShift APIServer cluster config
// and returns the TLS minimum version. On vanilla Kubernetes (where the
// OpenShift API does not exist), it returns 0 (Go defaults).
func DetectOpenShiftTLSProfile(clientset *kubernetes.Clientset, logger logr.Logger) uint16 {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check if the OpenShift config API group exists.
	// ServerGroupsAndResources may return partial results alongside an error
	// (e.g., one API group fails discovery while others succeed). If we got
	// partial results, still check for the OpenShift group among them.
	_, resources, err := clientset.Discovery().ServerGroupsAndResources()
	if err != nil {
		if resources == nil {
			logger.V(1).Info("Cannot list API resources for TLS profile detection", "error", err)
			return 0
		}
		logger.V(1).Info("Partial API discovery failure, checking available groups", "error", err)
	}

	found := false
	for _, rl := range resources {
		if rl.GroupVersion == "config.openshift.io/v1" {
			for _, r := range rl.APIResources {
				if r.Name == "apiservers" {
					found = true
					break
				}
			}
		}
	}
	if !found {
		logger.V(1).Info("OpenShift config API not found, using Go TLS defaults")
		return 0
	}

	// Read the APIServer resource using a raw REST request to avoid
	// importing OpenShift API types.
	data, err := clientset.RESTClient().
		Get().
		AbsPath("/apis/config.openshift.io/v1/apiservers/cluster").
		DoRaw(ctx)
	if err != nil {
		logger.V(1).Info("Cannot read OpenShift APIServer config", "error", err)
		return 0
	}

	var apiServer openshiftAPIServer
	if err := json.Unmarshal(data, &apiServer); err != nil {
		logger.V(1).Info("Cannot parse OpenShift APIServer config", "error", err)
		return 0
	}

	if apiServer.Spec.TLSSecurityProfile == nil {
		logger.Info("OpenShift TLS profile not set, using Intermediate defaults")
		return tls.VersionTLS12
	}

	profileType := apiServer.Spec.TLSSecurityProfile.Type
	minVer := TLSMinVersionForProfile(profileType)
	logger.Info("Detected OpenShift TLS profile",
		"profile", profileType,
		"tlsMinVersion", fmt.Sprintf("0x%04x", minVer))
	return minVer
}
