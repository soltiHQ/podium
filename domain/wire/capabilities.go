package wire

import (
	"strings"

	agentv1 "github.com/soltiHQ/control-plane/api/gen/solti/agent/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

// CapabilitiesToProto encodes the complete runner routing snapshot.
func CapabilitiesToProto(runners []model.RunnerCapability) *agentv1.AgentCapabilities {
	out := &agentv1.AgentCapabilities{Runners: make([]*agentv1.RunnerCapability, 0, len(runners))}
	for _, runner := range runners {
		p := &agentv1.RunnerCapability{Name: runner.Name, Labels: map[string]string{}, Workloads: make([]*agentv1.WorkloadType, 0, len(runner.Workloads))}
		for key, value := range runner.Labels {
			p.Labels[key] = value
		}
		for _, workload := range runner.Workloads {
			apiVersion := workload.Version
			if workload.Group != "" {
				apiVersion = workload.Group + "/" + workload.Version
			}
			p.Workloads = append(p.Workloads, &agentv1.WorkloadType{ApiVersion: apiVersion, Kind: workload.Kind})
		}
		out.Runners = append(out.Runners, p)
	}
	return out
}

// CapabilitiesFromProto decodes discovery and persisted runner snapshots.
func CapabilitiesFromProto(in *agentv1.AgentCapabilities) []model.RunnerCapability {
	out := make([]model.RunnerCapability, 0, len(in.GetRunners()))
	for _, runner := range in.GetRunners() {
		r := model.RunnerCapability{Name: runner.GetName(), Labels: map[string]string{}, Workloads: make([]model.GroupVersionKind, 0, len(runner.GetWorkloads()))}
		for key, value := range runner.GetLabels() {
			r.Labels[key] = value
		}
		for _, workload := range runner.GetWorkloads() {
			group, version, found := strings.Cut(workload.GetApiVersion(), "/")
			if !found {
				version, group = group, ""
			}
			r.Workloads = append(r.Workloads, model.GroupVersionKind{Group: group, Version: version, Kind: workload.GetKind()})
		}
		out = append(out, r)
	}
	return out
}
