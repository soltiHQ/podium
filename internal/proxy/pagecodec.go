package proxy

import (
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
)

// maxAnswerElements is the most elements that the repeated fields and maps of one
// gRPC answer may hold in all, however deep in the message tree: 262144. An empty
// element takes two bytes on the wire and about a hundred on the heap, so the 4 MiB
// a client reads would hold two million of them, and decoding them would cost
// hundreds of MiB. An answer of exactly the budget allocated 32 MiB to decode in the
// tests, a refused flood 2 to 8.
const maxAnswerElements = 1 << 18

// conditionsField is the full name of the field of the conditions of a task status.
// The scan caps the field for each status message at MaxTaskConditions, and keys the
// cap by the name the schema gives it, not by its number: field 6 of ObjectMeta, the
// labels, has the number of it, and a task may carry as many labels as the budget
// allows.
var conditionsField = (&taskv1.TaskStatus{}).ProtoReflect().Descriptor().Fields().ByName("conditions").FullName()

// The numbers of the fields that hold the page of an answer.
const (
	tasksField protowire.Number = 1 // ListTasksResponse.tasks
	runsField  protowire.Number = 1 // ListTaskRunsResponse.runs
)

// answerCodec is the protobuf codec of one unary call to an agent. Before it hands
// the answer to proto.Unmarshal it scans the wire bytes of it against the message
// descriptor, and refuses an answer that holds more than MaxTaskConditions conditions
// in one task status, more than maxAnswerElements elements of repeated fields and
// maps in all, or more than limit entries of the page field (ListTasks and
// ListTaskRuns name one; the others pass 0). Otherwise it encodes and decodes as
// grpc-go's own protobuf codec does. A codec is made for each call: it records its
// refusal, which grpc-go would flatten into the text of an Internal status.
type answerCodec struct {
	page  protowire.Number
	limit int

	mu      sync.Mutex
	refusal error
}

// newAnswerCodec makes the codec of a call whose answer has a page field, the
// repeated field of the number page with room for limit entries; page 0 means none.
func newAnswerCodec(page protowire.Number, limit int) *answerCodec {
	return &answerCodec{page: page, limit: limit}
}

// option makes the call use the codec.
func (c *answerCodec) option() grpc.CallOption { return grpc.ForceCodecV2(c) }

// refused returns the decode error for an answer the codec refused, or nil if it
// refused none. The error is an ErrTooManyItems for a page over its limit, an
// ErrTooManyElements for an answer over the budget and an ErrTooManyConditions for a
// status over its cap.
func (c *answerCodec) refused() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refusal
}

// Name is empty so that the call keeps the content-type application/grpc that a
// call without a forced codec sends; a name would add a content-subtype to it.
func (*answerCodec) Name() string { return "" }

func (*answerCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("proxy: marshal %T: not a protobuf message", v)
	}
	b, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	return mem.BufferSlice{mem.SliceBuffer(b)}, nil
}

func (c *answerCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("proxy: unmarshal into %T: not a protobuf message", v)
	}
	buf := data.MaterializeToBuffer(mem.DefaultBufferPool())
	defer buf.Free()
	wire := buf.ReadOnlyData()
	if err := scanElements(wire, m.ProtoReflect().Descriptor(), c.page, c.limit); err != nil {
		err = fmt.Errorf("%w: %w", ErrDecode, err)
		c.mu.Lock()
		c.refusal = err
		c.mu.Unlock()
		return err
	}
	return proto.Unmarshal(wire, m)
}

// scanElements walks the wire bytes b of a message of the type md and counts the
// elements of every repeated field and map in the tree of messages under it, a value
// of a packed scalar field for each, and the conditions of every task status. It
// returns an error that wraps ErrTooManyConditions at the condition past
// MaxTaskConditions in one status message, which comes before the budget even when
// that condition is also the element past it; one that wraps ErrTooManyElements at
// the element past maxAnswerElements; and one that wraps ErrTooManyItems at the
// entry past limit of the field page of b itself (page 0: none). Another error means
// the bytes are not a message, or nest messages deeper than protobuf decodes.
// Nothing is decoded or allocated: a field the schema does not know, or that the
// schema's wire type does not fit, is skipped, as proto.Unmarshal keeps it as
// unknown bytes. A status that the sender splits in several occurrences of the task's
// status field is counted by occurrence, though the decoder merges them: occurrences
// that pass the budget are refused here, and the check after decoding sees the whole
// of those within it.
func scanElements(b []byte, md protoreflect.MessageDescriptor, page protowire.Number, limit int) error {
	s := scanner{maxDepth: protowire.DefaultRecursionLimit, page: page, limit: limit}
	return s.message(b, md, 0)
}

// scanner is the state of one scanElements: the page entries and the elements seen.
type scanner struct {
	maxDepth int
	page     protowire.Number
	limit    int

	entries  int // entries of the page field seen
	elements int // elements of all repeated fields seen
}

// message scans the fields of the message b of the type md, which is depth messages
// below the top.
func (s *scanner) message(b []byte, md protoreflect.MessageDescriptor, depth int) error {
	if depth > s.maxDepth {
		return fmt.Errorf("messages nested deeper than %d", s.maxDepth)
	}
	fields := md.Fields()
	conditions := 0 // entries of the conditions field in this message
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
			return protowire.ParseError(n)
		}
		value := b[:n]
		b = b[n:]
		if depth == 0 && num == s.page && typ == protowire.BytesType {
			if s.entries++; s.entries > s.limit {
				return tooManyItems(s.limit)
			}
		}
		if fd := fields.ByNumber(num); fd != nil {
			// A task is refused in any page that holds it, so its 65th condition is
			// refused before the budget counts it.
			if typ == protowire.BytesType && fd.FullName() == conditionsField {
				if conditions++; conditions > MaxTaskConditions {
					return tooManyConditions()
				}
			}
			if err := s.field(fd, typ, value, depth); err != nil {
				return err
			}
		}
	}
	return nil
}

// field scans one value of the field fd, of the wire type typ.
func (s *scanner) field(fd protoreflect.FieldDescriptor, typ protowire.Type, value []byte, depth int) error {
	kind := fd.Kind()
	repeated := fd.Cardinality() == protoreflect.Repeated
	if repeated && typ == protowire.BytesType && packable(kind) {
		payload, _ := protowire.ConsumeBytes(value)
		return s.count(packedValues(payload, kind))
	}
	if typ != wireType(kind) {
		return nil
	}
	if repeated {
		if err := s.count(1); err != nil {
			return err
		}
	}
	if typ == protowire.BytesType && fd.Message() != nil {
		payload, _ := protowire.ConsumeBytes(value)
		return s.message(payload, fd.Message(), depth+1)
	}
	return nil
}

// count adds n elements and refuses the answer that has too many.
func (s *scanner) count(n int) error {
	if s.elements += n; s.elements > maxAnswerElements {
		return tooManyElements(fmt.Sprintf("more than %d elements in an answer", maxAnswerElements))
	}
	return nil
}

// wireType is the wire type a field of the kind has when it is not packed.
func wireType(k protoreflect.Kind) protowire.Type {
	switch k {
	case protoreflect.BoolKind, protoreflect.EnumKind, protoreflect.Int32Kind, protoreflect.Sint32Kind,
		protoreflect.Uint32Kind, protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind:
		return protowire.VarintType
	case protoreflect.Sfixed32Kind, protoreflect.Fixed32Kind, protoreflect.FloatKind:
		return protowire.Fixed32Type
	case protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind, protoreflect.DoubleKind:
		return protowire.Fixed64Type
	case protoreflect.GroupKind:
		return protowire.StartGroupType
	}
	return protowire.BytesType // string, bytes, message
}

// packable reports whether a repeated field of the kind may be packed.
func packable(k protoreflect.Kind) bool {
	return wireType(k) != protowire.BytesType && k != protoreflect.GroupKind
}

// packedValues counts the values of a packed field of the kind in payload.
func packedValues(payload []byte, k protoreflect.Kind) int {
	switch wireType(k) {
	case protowire.Fixed32Type:
		return len(payload) / 4
	case protowire.Fixed64Type:
		return len(payload) / 8
	}
	n := 0 // varints: each ends in a byte without the continuation bit
	for _, c := range payload {
		if c < 0x80 {
			n++
		}
	}
	return n
}
