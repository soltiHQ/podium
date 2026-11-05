package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type taskContractServer struct {
	taskv1.UnimplementedTaskServiceServer
	badName    bool
	missingUID bool
	deleteUID  string
	logUID     string
	list       *taskv1.ListTasksResponse // what ListTasks answers, unless listErr is set
	listErr    error
}

func (s *taskContractServer) resource() *taskv1.Task {
	name := "task"
	uid := "u"
	if s.badName {
		name = "different"
	}
	if s.missingUID {
		uid = ""
	}
	return &taskv1.Task{Metadata: &taskv1.ObjectMeta{Name: name, Uid: uid, ResourceVersion: "opaque", Generation: 2}, Spec: &taskv1.TaskSpec{Slot: "slot"}, Status: &taskv1.TaskStatus{ObservedGeneration: 2, Phase: taskv1.TaskPhase_TASK_PHASE_RUNNING}}
}

func (s *taskContractServer) GetTask(context.Context, *taskv1.GetTaskRequest) (*taskv1.GetTaskResponse, error) {
	return &taskv1.GetTaskResponse{Task: s.resource()}, nil
}

func (s *taskContractServer) ListTasks(context.Context, *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	return s.list, s.listErr
}

func (s *taskContractServer) ApplyTask(context.Context, *taskv1.ApplyTaskRequest) (*taskv1.ApplyTaskResponse, error) {
	return &taskv1.ApplyTaskResponse{Task: s.resource()}, nil
}

func (s *taskContractServer) DeleteTask(_ context.Context, r *taskv1.DeleteTaskRequest) (*taskv1.DeleteTaskResponse, error) {
	s.deleteUID = r.GetPreconditions().GetUid()
	return &taskv1.DeleteTaskResponse{}, nil
}

func (s *taskContractServer) StreamTaskLogs(r *taskv1.StreamTaskLogsRequest, stream grpc.ServerStreamingServer[taskv1.StreamTaskLogsResponse]) error {
	s.logUID = r.GetTaskUid()
	return stream.Send(&taskv1.StreamTaskLogsResponse{Kind: &taskv1.StreamTaskLogsResponse_RunStarted{RunStarted: &taskv1.RunStarted{Generation: 2, Attempt: 1}}})
}

func grpcContractProxy(t *testing.T, s *taskContractServer, opts ...grpc.DialOption) *grpcProxyV1 {
	t.Helper()
	return grpcContractProxyOver(t, s, nil, opts...)
}

// grpcContractProxyOver is grpcContractProxy over a server that has serverOpts.
func grpcContractProxyOver(t *testing.T, s taskv1.TaskServiceServer, serverOpts []grpc.ServerOption, opts ...grpc.DialOption) *grpcProxyV1 {
	t.Helper()
	l := &memoryListener{conns: make(chan net.Conn), done: make(chan struct{})}
	server := grpc.NewServer(serverOpts...)
	taskv1.RegisterTaskServiceServer(server, s)
	go func() { _ = server.Serve(l) }()
	opts = append([]grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient("passthrough:///test", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = l.Close() })
	return &grpcProxyV1{conn: conn}
}

func TestGRPCResourceIdentityAndUIDPinning(t *testing.T) {
	s := &taskContractServer{}
	p := grpcContractProxy(t, s)
	ctx := context.Background()
	resource, err := p.GetTask(ctx, "task")
	if err != nil || resource.Task.UID != "u" || resource.Task.ResourceVersion != "opaque" {
		t.Fatalf("resource %+v %v", resource, err)
	}
	if err = p.DeleteTask(ctx, "task"); err != nil || s.deleteUID != "u" {
		t.Fatalf("delete UID %q %v", s.deleteUID, err)
	}
	stream, err := p.StreamTaskLogsForUID(ctx, "task", "selected-uid")
	if err != nil {
		t.Fatal(err)
	}
	event := <-stream
	if event.GetRunStarted().GetGeneration() != 2 {
		t.Fatal("generation lost")
	}
	if s.logUID != "selected-uid" {
		t.Fatalf("log follows latest incarnation %s", s.logUID)
	}
}

func TestGRPCRejectsCounterfeitResourceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server *taskContractServer
	}{{"different name", &taskContractServer{badName: true}}, {"missing UID", &taskContractServer{missingUID: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			p := grpcContractProxy(t, tc.server)
			if _, err := p.GetTask(context.Background(), "task"); err == nil {
				t.Fatal("counterfeit Get accepted")
			}
			m := httpTestManifest(t, "task")
			if _, err := p.ApplyTask(context.Background(), TaskSubmission{Spec: m}); err == nil {
				t.Fatalf("counterfeit Apply accepted: %s", tc.name)
			}
		})
	}
}

// A page the client refuses for its size is tagged, so that a caller can ask for
// a smaller one. A refusal by the agent is not: ResourceExhausted also carries
// its quotas. The Kind is Unavailable either way.
func TestGRPCListTasksTagsAnAnswerOverTheReceiveLimit(t *testing.T) {
	padding := strings.Repeat("x", 4096)
	big := &taskv1.Task{Metadata: &taskv1.ObjectMeta{Name: "task", Uid: "u"}, Status: &taskv1.TaskStatus{Error: &padding}}
	for _, tc := range []struct {
		name   string
		server *taskContractServer
		tagged bool
	}{
		{"answer over the receive limit", &taskContractServer{list: &taskv1.ListTasksResponse{Tasks: []*taskv1.Task{big}}}, true},
		{"quota refused by the agent", &taskContractServer{listErr: status.Error(codes.ResourceExhausted, "retained task limit reached")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := grpcContractProxy(t, tc.server, grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1024)))

			_, err := p.ListTasks(context.Background(), TaskFilter{Limit: 1000})

			if !errors.Is(err, ErrListTasks) || status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("ListTasks = %v (%v), want a list tasks error that is ResourceExhausted", err, status.Code(err))
			}
			if got := errors.Is(err, ErrResponseTooLarge); got != tc.tagged {
				t.Fatalf("ErrResponseTooLarge = %v, want %v: %v", got, tc.tagged, err)
			}
			if kind := errkind.Classify(err); kind != errkind.Unavailable {
				t.Fatalf("kind = %v, want Unavailable: tagging must not change the classification", kind)
			}
		})
	}
}

type memoryListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *memoryListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, io.EOF
	}
}
func (l *memoryListener) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (*memoryListener) Addr() net.Addr { return memoryAddress{} }
func (l *memoryListener) DialContext(ctx context.Context) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		_ = server.Close()
		_ = client.Close()
		return nil, io.EOF
	case <-ctx.Done():
		_ = server.Close()
		_ = client.Close()
		return nil, ctx.Err()
	}
}

type memoryAddress struct{}

func (memoryAddress) Network() string { return "memory" }
func (memoryAddress) String() string  { return "test" }
