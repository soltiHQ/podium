package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// An HTTP agent answers in JSON, and every list that the proxy builds from it is
// decoded here one element at a time, so that the first element past the bound of
// the list fails the call before it is decoded. An empty element takes three bytes
// of JSON ("{},") and a few hundred on the heap once built, so the 4 MiB cap on a
// body does not bound what decoding it costs. The structs the proxy decodes hold no
// other list: the items of a ListTasks page, the runs of a ListTaskRuns page and
// the conditions of a task are all of them, and what the structs do not name is
// skipped without being built.

// decodeList decodes the JSON array in data, the value of the key name, one element
// at a time, onto list. An element that would be the one after limit fails with
// over() before it is decoded, and check, when it is not nil, sees every element
// decoded and may refuse it. list holds what an earlier key of the same name in the
// object decoded: a key that comes twice adds to the same list, so the bound holds
// whatever the keys. A null adds nothing, as for a slice.
func decodeList[T any](data []byte, name string, list []T, limit int, over func() error, check func(*T) error) ([]T, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return list, err
	}
	if token == nil {
		return list, nil
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return list, fmt.Errorf("%s is not an array", name)
	}
	for dec.More() {
		if len(list) == limit {
			return list, over()
		}
		var element T
		if err = dec.Decode(&element); err != nil {
			return list, err
		}
		if check != nil {
			if err = check(&element); err != nil {
				return list, err
			}
		}
		list = append(list, element)
	}
	_, err = dec.Token() // the closing bracket
	return list, err
}

// taskItems is the items of a ListTasks page, at most limit of them. An item is
// refused whose ID is over MaxTaskIDBytes.
type taskItems struct {
	limit int
	list  []taskResource
}

func (t *taskItems) UnmarshalJSON(data []byte) (err error) {
	t.list, err = decodeList(data, "items", t.list, t.limit,
		func() error { return tooManyItems(t.limit) },
		func(item *taskResource) error { return checkTaskID(item.Metadata.Name) })
	return err
}

// taskConditions is the conditions of a task, at most MaxTaskConditions of them. A
// task with more fails the decode with ErrTooManyConditions: a bound of the task,
// which no smaller page avoids.
type taskConditions []taskConditionResource

func (c *taskConditions) UnmarshalJSON(data []byte) (err error) {
	*c, err = decodeList(data, "conditions", *c, MaxTaskConditions, tooManyConditions, nil)
	return err
}

// taskRunResource is one run of a ListTaskRuns page.
type taskRunResource struct {
	Generation uint64     `json:"generation"`
	Attempt    int        `json:"attempt"`
	Phase      string     `json:"phase"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Error      string     `json:"error"`
	ExitCode   *int32     `json:"exitCode"`
	Workload   struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	} `json:"workload"`
}

// runItems is the runs of a ListTaskRuns page, at most limit of them: the page size
// the request named.
type runItems struct {
	limit int
	list  []taskRunResource
}

func (r *runItems) UnmarshalJSON(data []byte) (err error) {
	r.list, err = decodeList[taskRunResource](data, "runs", r.list, r.limit, func() error { return tooManyItems(r.limit) }, nil)
	return err
}
