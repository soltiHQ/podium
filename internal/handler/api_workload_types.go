package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
)

// WorkloadTypes handles /api/v1/workload-types.
//
// Supported:
//   - GET /api/v1/workload-types
func (a *API) WorkloadTypes(w http.ResponseWriter, r *http.Request) {
	route.Resource(w, r, routepath.ApiWorkloadTypes,
		route.Endpoint{Method: http.MethodGet, Perm: enum.SpecsGet, Fn: a.workloadTypeList},
	)
}

// WorkloadTypeRouter handles /api/v1/workload-types/{group}/{version}/{kind}.
// The group may contain dots; a type without a group is {version}/{kind}.
//
// Supported:
//   - GET /api/v1/workload-types/{group}/{version}/{kind}
func (a *API) WorkloadTypeRouter(w http.ResponseWriter, r *http.Request) {
	route.Resource(w, r, "",
		route.Endpoint{Method: http.MethodGet, Perm: enum.SpecsGet, Fn: a.workloadTypeDetail},
	)
}

func (a *API) workloadTypeList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	types, err := a.catalogSVC.Types(r.Context())
	if err != nil {
		a.logger.Error().Err(err).Msg("workload type list failed")
		response.FromError(w, r, mode, err)
		return
	}
	items := make([]restv1.WorkloadType, 0, len(types))
	for _, t := range types {
		dto := restv1.WorkloadType{APIVersion: t.APIVersion, Kind: t.Kind, Fingerprints: make([]restv1.WorkloadFingerprint, 0, len(t.Fingerprints))}
		for _, f := range t.Fingerprints {
			dto.Fingerprints = append(dto.Fingerprints, restv1.WorkloadFingerprint{Fingerprint: f.Fingerprint, Agents: f.Agents, Received: f.Schema != nil})
		}
		items = append(items, dto)
	}
	response.OK(w, r, mode, &responder.View{Data: items})
}

func (a *API) workloadTypeDetail(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, routepath.ApiWorkloadType), "/"), "/")
	var apiVersion, kind string
	switch len(parts) {
	case 2:
		apiVersion, kind = parts[0], parts[1]
	case 3:
		apiVersion, kind = parts[0]+"/"+parts[1], parts[2]
	}
	if kind == "" || strings.HasPrefix(apiVersion, "/") || strings.HasSuffix(apiVersion, "/") {
		response.NotFound(w, r, mode)
		return
	}
	t, err := a.catalogSVC.Type(r.Context(), apiVersion, kind)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			a.logger.Error().Err(err).Str("api_version", apiVersion).Str("kind", kind).Msg("workload type get failed")
		}
		response.FromError(w, r, mode, err)
		return
	}
	dto := restv1.WorkloadTypeDetail{APIVersion: t.APIVersion, Kind: t.Kind, Fingerprints: make([]restv1.WorkloadFingerprintDetail, 0, len(t.Fingerprints))}
	for _, f := range t.Fingerprints {
		dto.Fingerprints = append(dto.Fingerprints, restv1.WorkloadFingerprintDetail{Fingerprint: f.Fingerprint, Agents: f.Agents, Received: f.Schema != nil, Schema: f.Schema})
	}
	response.OK(w, r, mode, &responder.View{Data: dto})
}
