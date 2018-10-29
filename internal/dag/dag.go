// Copyright © 2018 Heptio
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

// Package dag provides a data model, in the form of a directed acyclic graph,
// of the relationship between Kubernetes Ingress, Service, and Secret objects.
package dag

import (
	"time"

	"k8s.io/api/core/v1"

	"github.com/envoyproxy/go-control-plane/envoy/api/v2/auth"
	ingressroutev1 "github.com/heptio/contour/apis/contour/v1beta1"
)

// A DAG represents a directed acylic graph of objects representing the relationship
// between Kubernetes Ingress objects, the backend Services, and Secret objects.
// The DAG models these relationships as Roots and Vertices.
type DAG struct {
	// roots are the roots of this dag
	roots []Vertex

	// status computed while building this dag.
	statuses []Status
}

// Visit calls fn on each root of this DAG.
func (d *DAG) Visit(fn func(Vertex)) {
	for _, r := range d.roots {
		fn(r)
	}
}

// Statuses returns a slice of Status objects associated with
// the computation of this DAG.
func (d *DAG) Statuses() []Status {
	return d.statuses
}

type Route struct {
	Prefix       string
	object       interface{} // one of Ingress or IngressRoute
	httpServices map[servicemeta]*HTTPService

	// Should this route generate a 301 upgrade if accessed
	// over HTTP?
	HTTPSUpgrade bool

	// Is this a websocket route?
	// TODO(dfc) this should go on the service
	Websocket bool

	// A timeout applied to requests on this route.
	// A timeout of zero implies "use envoy's default"
	// A timeout of -1 represents "infinity"
	// TODO(dfc) should this move to service?
	Timeout time.Duration

	// RetryOn specifies the conditions under which retry takes place.
	// If empty, retries will not be performed.
	RetryOn string

	// NumRetries specifies the allowed number of retries.
	// Ignored if RetryOn is blank, or defaults to 1 if RetryOn is set.
	NumRetries int

	// PerTryTimeout specifies the timeout per retry attempt.
	// Ignored if RetryOn is blank.
	PerTryTimeout time.Duration

	// Indicates that during forwarding, the matched prefix (or path) should be swapped with this value
	PrefixRewrite string
}

type ServiceVertex interface {
	Vertex
	toMeta() servicemeta
}

func (r *Route) addService(sv ServiceVertex) {
	if r.httpServices == nil {
		r.httpServices = make(map[servicemeta]*HTTPService)
	}
	r.httpServices[sv.toMeta()] = sv.(*HTTPService)
}

func (r *Route) Visit(f func(Vertex)) {
	for _, c := range r.httpServices {
		f(c)
	}
}

// A VirtualHost represents an insecure HTTP host.
type VirtualHost struct {
	// Port is the port that the VirtualHost will listen on.
	// Expected values are 80 and 443, but others are possible
	// if the VirtualHost is generated inside Contour.
	Port int

	Host   string
	routes map[string]*Route
}

func (v *VirtualHost) addRoute(route *Route) {
	if v.routes == nil {
		v.routes = make(map[string]*Route)
	}
	v.routes[route.Prefix] = route
}

func (v *VirtualHost) Visit(f func(Vertex)) {
	for _, r := range v.routes {
		f(r)
	}
}

// A SecureVirtualHost represents a HTTP host protected by TLS.
type SecureVirtualHost struct {
	VirtualHost

	// TLS minimum protocol version. Defaults to auth.TlsParameters_TLS_AUTO
	MinProtoVersion auth.TlsParameters_TlsProtocol

	secret *Secret
}

func (s *SecureVirtualHost) Data() map[string][]byte {
	if s.secret == nil {
		return nil
	}
	return s.secret.Data()
}

func (s *SecureVirtualHost) Visit(f func(Vertex)) {
	s.VirtualHost.Visit(f)
	f(s.secret)
}

type Visitable interface {
	Visit(func(Vertex))
}

type Vertex interface {
	Visitable
}

// Service represents a raw Kuberentes Service as a DAG vertex.
// A Service is a leaf in the DAG.
type Service struct {
	Object *v1.Service

	*v1.ServicePort
	Weight int

	// The load balancer type to use when picking a host in the cluster.
	// See https://www.envoyproxy.io/docs/envoy/latest/api-v2/api/v2/cds.proto#envoy-api-enum-cluster-lbpolicy
	LoadBalancerStrategy string

	// Circuit breaking limits

	// Max connections is maximum number of connections
	// that Envoy will make to the upstream cluster.
	MaxConnections int

	// MaxPendingRequests is maximum number of pending
	// requests that Envoy will allow to the upstream cluster.
	MaxPendingRequests int

	// MaxRequests is the maximum number of parallel requests that
	// Envoy will make to the upstream cluster.
	MaxRequests int

	// MaxRetries is the maximum number of parallel retries that
	// Envoy will allow to the upstream cluster.
	MaxRetries int
}

func (s *Service) Name() string      { return s.Object.Name }
func (s *Service) Namespace() string { return s.Object.Namespace }

// HTTPService represents a Kuberneres Service object which speaks
// HTTP/1.1 or HTTP/2.0.
type HTTPService struct {
	Service

	// Protocol is the layer 7 protocol of this service
	// One of "", "h2", or "h2c".
	Protocol string

	HealthCheck *ingressroutev1.HealthCheck // TODO(dfc) HealthCheck should be generalised and moved to Service.
}

func (s *HTTPService) Visit(func(Vertex)) {
	// Visit is defined on HTTPService, not Service, so the latter
	// cannot be inserted into the DAG nor interface asserted from a Vertex.
}

type servicemeta struct {
	name        string
	namespace   string
	port        int32
	weight      int
	strategy    string
	healthcheck string // %#v of *ingressroutev1.HealthCheck
}

func (s *HTTPService) toMeta() servicemeta {
	return servicemeta{
		name:        s.Object.Name,
		namespace:   s.Object.Namespace,
		port:        s.Port,
		weight:      s.Weight,
		strategy:    s.LoadBalancerStrategy,
		healthcheck: healthcheckToString(s.HealthCheck),
	}
}

// Secret represents a K8s Secret for TLS usage as a DAG Vertex. A Secret is
// a leaf in the DAG.
type Secret struct {
	object *v1.Secret
}

func (s *Secret) Name() string       { return s.object.Name }
func (s *Secret) Namespace() string  { return s.object.Namespace }
func (s *Secret) Visit(func(Vertex)) {}

// Data returns the contents of the backing secret's map.
func (s *Secret) Data() map[string][]byte {
	return s.object.Data
}

func (s *Secret) toMeta() meta {
	return meta{
		name:      s.object.Name,
		namespace: s.object.Namespace,
	}
}
