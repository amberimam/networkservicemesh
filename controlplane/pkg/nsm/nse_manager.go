package nsm

import (
	"context"

	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/properties"

	"github.com/pkg/errors"

	"github.com/networkservicemesh/networkservicemesh/pkg/tools/spanhelper"

	"github.com/sirupsen/logrus"

	"github.com/networkservicemesh/networkservicemesh/controlplane/api/connection"
	"github.com/networkservicemesh/networkservicemesh/controlplane/api/registry"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/api/nsm"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/model"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/serviceregistry"
)

type nseManager struct {
	serviceRegistry serviceregistry.ServiceRegistry
	model           model.Model
	props           *properties.Properties
}

func (nsem *nseManager) GetEndpoint(ctx context.Context, requestConnection *connection.Connection, ignoreEndpoints map[registry.EndpointNSMName]*registry.NSERegistration) (*registry.NSERegistration, error) {
	span := spanhelper.FromContext(ctx, "GetEndpoint")
	defer span.Finish()
	span.LogObject("request", requestConnection)
	span.LogObject("ignores", ignoreEndpoints)
	// Handle case we are remote NSM and asked for particular endpoint to connect to.
	targetEndpoint := requestConnection.GetNetworkServiceEndpointName()
	myNsemName := nsem.model.GetNsm().GetName()
	targetNsemName := requestConnection.GetDestinationNetworkServiceManagerName()
	span.LogObject("targetEndpoint", targetEndpoint)
	span.LogObject("targetNsemName", targetNsemName)
	if len(targetEndpoint) > 0 {
		if len(targetNsemName) > 0 && myNsemName == targetNsemName {
			endpoint := nsem.model.GetEndpoint(targetEndpoint)
			if endpoint != nil && ignoreEndpoints[endpoint.Endpoint.GetEndpointNSMName()] == nil {
				return endpoint.Endpoint, nil
			} else {
				return nil, errors.Errorf("Could not find endpoint with name: %s at local registry", targetEndpoint)
			}
		}
	}

	// Get endpoints, do it every time since we do not know if list are changed or not.
	discoveryClient, err := nsem.serviceRegistry.DiscoveryClient(ctx)
	if err != nil {
		span.LogError(err)
		return nil, err
	}
	nseRequest := &registry.FindNetworkServiceRequest{
		NetworkServiceName: requestConnection.GetNetworkService(),
	}
	span.LogObject("nseRequest", nseRequest)
	endpointResponse, err := discoveryClient.FindNetworkService(ctx, nseRequest)
	span.LogObject("nseResponse", endpointResponse)
	if err != nil {
		span.LogError(err)
		return nil, err
	}
	var endpoint *registry.NetworkServiceEndpoint
	if len(targetEndpoint) > 0 {
		endpoint = nsem.getTargetEndpoint(endpointResponse.GetNetworkServiceEndpoints(), targetEndpoint, targetNsemName)
		if endpoint == nil {
			err = errors.Errorf("failed to find targeted NSE %s (NSMgr=%s) for NetworkService %s. Checked: %d endpoints",
				targetEndpoint, targetNsemName, requestConnection.GetNetworkService(), len(endpointResponse.GetNetworkServiceEndpoints()))
			span.LogError(err)
			return nil, err
		}
	} else {
		endpoints := nsem.filterEndpoints(endpointResponse.GetNetworkServiceEndpoints(), endpointResponse.NetworkServiceManagers, ignoreEndpoints)

		if len(endpoints) == 0 {
			err = errors.Errorf("failed to find NSE for NetworkService %s. Checked: %d of total NSEs: %d",
				requestConnection.GetNetworkService(), len(ignoreEndpoints), len(endpoints))
			span.LogError(err)
			return nil, err
		}

		endpoint = nsem.model.GetSelector().SelectEndpoint(requestConnection, endpointResponse.GetNetworkService(), endpoints)
		if endpoint == nil {
			err = errors.Errorf("failed to find NSE for NetworkService %s. Checked: %d of total NSEs: %d",
				requestConnection.GetNetworkService(), len(ignoreEndpoints), len(endpoints))
			span.LogError(err)
			return nil, err
		}
	}
	span.LogObject("endpoint", endpoint)
	return &registry.NSERegistration{
		NetworkServiceManager:  endpointResponse.GetNetworkServiceManagers()[endpoint.GetNetworkServiceManagerName()],
		NetworkServiceEndpoint: endpoint,
		NetworkService:         endpointResponse.GetNetworkService(),
	}, nil
}

/**
ctx - we assume it is big enought to perform connection.
*/
func (nsem *nseManager) CreateNSEClient(ctx context.Context, endpoint *registry.NSERegistration) (nsm.NetworkServiceClient, error) {
	span := spanhelper.FromContext(ctx, "createNSEClient")
	defer span.Finish()
	logger := span.Logger()
	if nsem.IsLocalEndpoint(endpoint) {
		modelEp := nsem.model.GetEndpoint(endpoint.GetNetworkServiceEndpoint().GetName())
		if modelEp == nil {
			return nil, errors.Errorf("Endpoint not found: %v", endpoint)
		}
		logger.Infof("Create local NSE connection to endpoint: %v", modelEp)
		client, conn, err := nsem.serviceRegistry.EndpointConnection(span.Context(), modelEp)
		if err != nil {
			span.LogError(err)
			// We failed to connect to local NSE.
			nsem.cleanupNSE(ctx, modelEp)
			return nil, err
		}
		return &endpointClient{connection: conn, client: client}, nil
	} else {
		logger.Infof("Create remote NSE connection to endpoint: %v", endpoint)
		ctx, cancel := context.WithTimeout(span.Context(), nsem.props.HealRequestConnectTimeout)
		defer cancel()
		client, conn, err := nsem.serviceRegistry.RemoteNetworkServiceClient(ctx, endpoint.GetNetworkServiceManager())
		if err != nil {
			return nil, err
		}
		return &nsmClient{client: client, connection: conn}, nil
	}
}

func (nsem *nseManager) IsLocalEndpoint(endpoint *registry.NSERegistration) bool {
	return nsem.model.GetNsm().GetName() == endpoint.GetNetworkServiceEndpoint().GetNetworkServiceManagerName()
}

func (nsem *nseManager) CheckUpdateNSE(ctx context.Context, reg *registry.NSERegistration) bool {
	pingCtx, pingCancel := context.WithTimeout(ctx, nsem.props.HealRequestConnectCheckTimeout)
	defer pingCancel()

	client, err := nsem.CreateNSEClient(pingCtx, reg)
	if err == nil && client != nil {
		_ = client.Cleanup()
		return true
	}

	return false
}

func (nsem *nseManager) cleanupNSE(ctx context.Context, endpoint *model.Endpoint) {
	// Remove endpoint from model and put workspace into BAD state.
	nsem.model.DeleteEndpoint(ctx, endpoint.EndpointName())
	logrus.Infof("NSM: Remove Endpoint since it is not available... %v", endpoint)
}

func (nsem *nseManager) filterEndpoints(endpoints []*registry.NetworkServiceEndpoint, managers map[string]*registry.NetworkServiceManager, ignoreEndpoints map[registry.EndpointNSMName]*registry.NSERegistration) []*registry.NetworkServiceEndpoint {
	result := []*registry.NetworkServiceEndpoint{}
	// Do filter of endpoints
	for _, candidate := range endpoints {
		endpointName := registry.NewEndpointNSMName(candidate, managers[candidate.NetworkServiceManagerName])
		if ignoreEndpoints[endpointName] == nil {
			result = append(result, candidate)
		}
	}
	return result
}

func (nsem *nseManager) getTargetEndpoint(endpoints []*registry.NetworkServiceEndpoint, targetEndpoint, targetNSManager string) *registry.NetworkServiceEndpoint {
	// find matching endpoint in list
	for _, candidate := range endpoints {
		if candidate.GetName() == targetEndpoint {
			return candidate
		}
	}
	return nil
}