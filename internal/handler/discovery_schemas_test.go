package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/soltiHQ/control-plane/api/gen/solti/agent/v1"
	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/service/workload"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
)

const exchangeSchema = `{"type":"object"}`

func schemaSyncRequest(t *testing.T, fp string, offer bool) *discoverv1.SyncRequest {
	t.Helper()
	req := &discoverv1.SyncRequest{
		Id: "schema-agent", Name: "Agent", Endpoint: "http://10.0.0.5:8085",
		EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_HTTP, ApiVersion: 1,
		Capabilities: &agentv1.AgentCapabilities{Runners: []*agentv1.RunnerCapability{{
			Name:      "default",
			Workloads: []*agentv1.WorkloadType{{ApiVersion: "solti.io/v1", Kind: "Subprocess", SchemaFingerprint: fp}},
		}}},
	}
	if offer {
		req.Schemas = []*discoverv1.WorkloadSchema{{Fingerprint: fp, Schema: []byte(exchangeSchema)}}
	}
	return req
}

func TestDiscoverySchemaExchangeOverBothTransports(t *testing.T) {
	fp, err := workloadschema.Fingerprint([]byte(exchangeSchema))
	if err != nil {
		t.Fatal(err)
	}
	logger := zerolog.Nop()
	for _, transport := range []string{"http", "grpc"} {
		t.Run(transport, func(t *testing.T) {
			store := inmemory.New()
			svc := agent.New(store, logger)
			hub := event.NewHub(logger)
			defer hub.Close()
			catalog := workload.New(store, logger)
			sync := func(req *discoverv1.SyncRequest) []string {
				if transport == "http" {
					h := NewHTTPDiscovery(logger, svc, hub, discoveryLimiter(), heartbeatBuckets(), nil, false)
					h.UseCatalog(catalog)
					body, err := protojson.Marshal(req)
					if err != nil {
						t.Fatal(err)
					}
					res := syncHTTP(h, string(body), "")
					if res.Code != http.StatusOK {
						t.Fatalf("HTTP %d %s", res.Code, res.Body.String())
					}
					var out struct {
						WantSchemas []string `json:"wantSchemas"`
					}
					if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
						t.Fatal(err)
					}
					return out.WantSchemas
				}
				g := NewGRPCDiscovery(logger, svc, hub, discoveryLimiter(), heartbeatBuckets(), nil, false)
				g.UseCatalog(catalog)
				resp, err := g.Sync(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				return resp.GetWantSchemas()
			}
			if got := sync(schemaSyncRequest(t, fp, false)); !slices.Equal(got, []string{fp}) {
				t.Fatalf("first sync wants %v, want [%s]", got, fp)
			}
			if got := sync(schemaSyncRequest(t, fp, true)); len(got) != 0 {
				t.Fatalf("sync that offers the schema still wants %v", got)
			}
			if _, err := store.GetWorkloadSchema(context.Background(), fp); err != nil {
				t.Fatalf("schema not stored: %v", err)
			}
			if got := sync(schemaSyncRequest(t, fp, false)); len(got) != 0 {
				t.Fatalf("known schema wanted again: %v", got)
			}
		})
	}
}
