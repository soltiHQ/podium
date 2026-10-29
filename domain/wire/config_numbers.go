package wire

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
)

// Struct numbers are float64. Reject integers that cannot be represented
// exactly instead of acknowledging a persistence write that changes its value.
func normalizeConfigNumbers(value any) error {
	convert := func(number json.Number) (float64, error) {
		f, err := number.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, fmt.Errorf("number %s cannot be represented", number)
		}
		exact, ok := new(big.Rat).SetString(number.String())
		if !ok {
			return 0, fmt.Errorf("invalid number %s", number)
		}
		if exact.IsInt() && new(big.Rat).SetFloat64(f).Cmp(exact) != 0 {
			return 0, fmt.Errorf("integer %s cannot be represented losslessly by protobuf Struct; encode it as a string", number)
		}
		return f, nil
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if number, ok := item.(json.Number); ok {
				converted, err := convert(number)
				if err != nil {
					return err
				}
				typed[key] = converted
			} else if err := normalizeConfigNumbers(item); err != nil {
				return err
			}
		}
	case []any:
		for index, item := range typed {
			if number, ok := item.(json.Number); ok {
				converted, err := convert(number)
				if err != nil {
					return err
				}
				typed[index] = converted
			} else if err := normalizeConfigNumbers(item); err != nil {
				return err
			}
		}
	}
	return nil
}
