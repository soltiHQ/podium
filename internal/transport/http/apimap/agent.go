package apimap

import (
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

func Agent(a *model.Agent) restv1.Agent {
	if a == nil {
		return restv1.Agent{}
	}
	return restv1.Agent{
		ID:   a.ID(),
		Name: a.Name(),

		OS:            a.OS(),
		Arch:          a.Arch(),
		Platform:      a.Platform(),
		UptimeSeconds: a.UptimeSeconds(),

		Metadata: a.MetadataAll(),
		Labels:   a.LabelsAll(),
		
		Endpoint:     a.Endpoint(),
		EndpointType: string(a.EndpointType()),
		APIVersion:   a.APIVersion().String(),
	}
}
