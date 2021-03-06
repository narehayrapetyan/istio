// Copyright 2018 Istio Authors
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

package v1alpha3

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gogo/protobuf/jsonpb"

	"github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	mongo_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/mongo_proxy/v2"
	tcp_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/tcp_proxy/v2"
	"github.com/envoyproxy/go-control-plane/pkg/util"
	"github.com/gogo/protobuf/types"

	"istio.io/istio/pilot/pkg/model"

	"bytes"

	"k8s.io/apimachinery/pkg/util/json"
)

// buildInboundNetworkFilters generates a TCP proxy network filter on the inbound path
func buildInboundNetworkFilters(instance *model.ServiceInstance) []listener.Filter {
	clusterName := model.BuildSubsetKey(model.TrafficDirectionInbound, "",
		instance.Service.Hostname, instance.Endpoint.ServicePort)
	config := &tcp_proxy.TcpProxy{
		StatPrefix: fmt.Sprintf("%s|tcp|%d", model.TrafficDirectionInbound, instance.Endpoint.ServicePort.Port),
		Cluster:    clusterName,
	}

	return []listener.Filter{
		{
			Name:   util.TCPProxy,
			Config: messageToStruct(config),
		},
	}

}

func buildDeprecatedTCPRouteConfig(clusterName string, addresses []string) *DeprecatedTCPRouteConfig {
	route := &DeprecatedTCPRoute{
		Cluster: clusterName,
	}
	sort.Sort(sort.StringSlice(addresses))
	for _, addr := range addresses {
		tcpRouteAddr := addr
		if !strings.Contains(addr, "/") {
			tcpRouteAddr = addr + "/32"
		}
		route.DestinationIPList = append(route.DestinationIPList, tcpRouteAddr)
	}

	routeConfig := &DeprecatedTCPRouteConfig{Routes: []*DeprecatedTCPRoute{route}}

	return routeConfig
}

// buildOutboundNetworkFilters generates TCP proxy network filter for outbound connections. In addition, it generates
// protocol specific filters (e.g., Mongo filter)
// this function constructs deprecated_v1 routes, until the filter chain match is ready
func buildOutboundNetworkFilters(clusterName string, addresses []string, port *model.Port) []listener.Filter {

	// destination port is unnecessary with use_original_dst since
	// the listener address already contains the port
	filterConfig := &DeprecatedTCPProxyFilterConfig{
		StatPrefix:  fmt.Sprintf("%s|tcp|%d", model.TrafficDirectionOutbound, port.Port),
		RouteConfig: buildDeprecatedTCPRouteConfig(clusterName, addresses),
	}

	//deprecatedConfig := &DeprecatedFilterConfigInV2{
	//	DeprecatedV1: true,
	//	Value:filterConfig,
	//}

	//if len(addresses) > 0 {
	//	sort.Sort(sort.StringSlice(addresses))
	//	route.DestinationIpList = append(route.DestinationIpList, convertAddressListToCidrList(addresses)...)
	//}

	trueValue := types.Value{
		Kind: &types.Value_BoolValue{
			BoolValue: true,
		},
	}
	data, err := json.Marshal(filterConfig)
	if err != nil {
		return nil
	}
	pbs := &types.Struct{}
	if err := jsonpb.Unmarshal(bytes.NewReader(data), pbs); err != nil {
		return nil
	}

	structValue := types.Value{
		Kind: &types.Value_StructValue{
			StructValue: pbs,
		},
	}

	// FIXME
	tcpFilter := listener.Filter{
		Name: util.TCPProxy,
		Config: &types.Struct{Fields: map[string]*types.Value{
			"deprecated_v1": &trueValue,
			"value":         &structValue,
		}},
		//DeprecatedV1: &listener.Filter_DeprecatedV1{
		//	Type: "",
		//},
	}

	filterstack := make([]listener.Filter, 0)
	switch port.Protocol {
	case model.ProtocolMongo:
		filterstack = append(filterstack, buildOutboundMongoFilter())
	}
	filterstack = append(filterstack, tcpFilter)

	return filterstack
}

func buildOutboundMongoFilter() listener.Filter {
	// TODO: add a watcher for /var/lib/istio/mongo/certs
	// if certs are found use, TLS or mTLS clusters for talking to MongoDB.
	// User is responsible for mounting those certs in the pod.
	config := &mongo_proxy.MongoProxy{
		StatPrefix: "mongo",
		// TODO enable faults in mongo
	}

	return listener.Filter{
		Name:   util.MongoProxy,
		Config: messageToStruct(config),
	}
}

// DeprecatedTCPRoute definition
type DeprecatedTCPRoute struct {
	Cluster           string   `json:"cluster"`
	DestinationIPList []string `json:"destination_ip_list,omitempty"`
	DestinationPorts  string   `json:"destination_ports,omitempty"`
	SourceIPList      []string `json:"source_ip_list,omitempty"`
	SourcePorts       string   `json:"source_ports,omitempty"`
}

// DeprecatedTCPRouteByRoute sorts TCP routes over all route sub fields.
type DeprecatedTCPRouteByRoute []*DeprecatedTCPRoute

func (r DeprecatedTCPRouteByRoute) Len() int {
	return len(r)
}

func (r DeprecatedTCPRouteByRoute) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

func (r DeprecatedTCPRouteByRoute) Less(i, j int) bool {
	if r[i].Cluster != r[j].Cluster {
		return r[i].Cluster < r[j].Cluster
	}

	compare := func(a, b []string) bool {
		lenA, lenB := len(a), len(b)
		min := lenA
		if min > lenB {
			min = lenB
		}
		for k := 0; k < min; k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return lenA < lenB
	}

	if less := compare(r[i].DestinationIPList, r[j].DestinationIPList); less {
		return less
	}
	if r[i].DestinationPorts != r[j].DestinationPorts {
		return r[i].DestinationPorts < r[j].DestinationPorts
	}
	if less := compare(r[i].SourceIPList, r[j].SourceIPList); less {
		return less
	}
	if r[i].SourcePorts != r[j].SourcePorts {
		return r[i].SourcePorts < r[j].SourcePorts
	}
	return false
}

// DeprecatedTCPProxyFilterConfig definition
type DeprecatedTCPProxyFilterConfig struct {
	StatPrefix  string                    `json:"stat_prefix"`
	RouteConfig *DeprecatedTCPRouteConfig `json:"route_config"`
}

// DeprecatedTCPRouteConfig (or generalize as RouteConfig or L4RouteConfig for TCP/UDP?)
type DeprecatedTCPRouteConfig struct {
	Routes []*DeprecatedTCPRoute `json:"routes"`
}

// DeprecatedFilterConfigInV2 definition
type DeprecatedFilterConfigInV2 struct {
	DeprecatedV1 bool                            `json:"deprecated_v1"`
	Value        *DeprecatedTCPProxyFilterConfig `json:"value"`
}
