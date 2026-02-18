package apimap

import (
	v1 "github.com/soltiHQ/control-plane/api/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

func Agent(a *model.Agent) v1.Agent {
	if a == nil {
		return v1.Agent{}
	}
	return v1.Agent{
		ID:            a.ID(),
		Name:          a.Name(),
		Endpoint:      a.Endpoint(),
		OS:            a.OS(),
		Arch:          a.Arch(),
		Platform:      a.Platform(),
		UptimeSeconds: a.UptimeSeconds(),
		Metadata:      a.MetadataAll(),
		Labels:        a.LabelsAll(),
	}
}
