package wire

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// NumberError is a number of a workload body that a spec cannot store. A
// protobuf Struct, which holds the body wherever a spec is encoded (the raft log
// and snapshots, the database rows), keeps numbers as IEEE 754 doubles: one out
// of their range, or an integer that a double would change, is refused rather
// than stored changed. Every integer within ±2^53 is kept exactly.
type NumberError struct {
	// Path is the RFC 6901 JSON pointer of the number in the body.
	Path string
	// Number is the number as the body spells it.
	Number string
	// Reason says why the number cannot be stored.
	Reason string
}

func (e *NumberError) Error() string { return e.Path + ": " + e.Reason }

// CheckWorkloadBody reports what SpecToProto would refuse in body: nil, a
// *NumberError for the first number in key order that a Struct cannot hold
// exactly, or another error for a value that is not JSON. It changes nothing.
func CheckWorkloadBody(body map[string]any) error {
	if body == nil {
		return nil
	}
	_, err := configStruct(body, "workload body")
	return err
}

// normalizeConfigNumbers converts each json.Number of value, in place, to the
// float64 a Struct holds. It refuses one a double cannot hold exactly, the
// first in key order, with a *NumberError whose path is at followed by the
// number's place in value, instead of acknowledging a persistence write that
// changes its value.
func normalizeConfigNumbers(value any, at string) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			converted, err := normalizeConfigValue(typed[key], at+"/"+pointerToken(key))
			if err != nil {
				return err
			}
			typed[key] = converted
		}
	case []any:
		for index, item := range typed {
			converted, err := normalizeConfigValue(item, at+"/"+strconv.Itoa(index))
			if err != nil {
				return err
			}
			typed[index] = converted
		}
	}
	return nil
}

// normalizeConfigValue is the value a Struct holds for item, at path at.
func normalizeConfigValue(item any, at string) (any, error) {
	number, ok := item.(json.Number)
	if !ok {
		return item, normalizeConfigNumbers(item, at)
	}
	f, err := number.Float64()
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, &NumberError{Path: at, Number: number.String(), Reason: fmt.Sprintf("the number %s is out of range", number)}
	}
	exact, ok := new(big.Rat).SetString(number.String())
	if !ok {
		return nil, &NumberError{Path: at, Number: number.String(), Reason: fmt.Sprintf("%s is not a number", number)}
	}
	if exact.IsInt() && new(big.Rat).SetFloat64(f).Cmp(exact) != 0 {
		return nil, &NumberError{Path: at, Number: number.String(), Reason: fmt.Sprintf(
			"the integer %s cannot be stored exactly: integers must fit in ±2^53; send larger ones, such as 64-bit IDs, as strings", number)}
	}
	return f, nil
}

// pointerToken escapes a member name as a token of an RFC 6901 JSON pointer.
func pointerToken(name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
}
