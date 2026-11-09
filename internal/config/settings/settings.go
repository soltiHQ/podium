// Package settings reads the shape of a configuration struct: which keys a YAML
// file may hold, which environment variable carries each setting, and how an
// environment value becomes a field.
//
// A setting is a leaf field of a struct tree. Its YAML key is the field's yaml
// tag (the lower-cased field name when untagged), and its environment variable
// is the prefix and the keys of its path in upper case, joined by "_": under the
// prefix SOLTI, http.read_header_timeout is SOLTI_HTTP_READ_HEADER_TIMEOUT. A
// field tagged yaml:"-" is not a setting unless it also carries an env tag,
// which makes it an env-only setting: it has that exact variable name and no
// YAML key. No other variable name is read.
//
// The leaf types are string, bool, the integer kinds, time.Duration and
// []string. Any other type makes [Walk] panic, so a setting of a new type fails
// the first test that loads the configuration instead of being ignored.
package settings

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var durationType = reflect.TypeOf(time.Duration(0))

// Setting is one leaf of a configuration struct.
type Setting struct {
	// Path is the YAML keys from the root to the setting. For an env-only
	// setting the last key is derived from its variable name, so the key a file
	// must not hold can be named.
	Path []string
	// Env is the variable that carries the setting.
	Env string
	// EnvOnly is true for a setting that a YAML file must not hold.
	EnvOnly bool
	// Value is the settable field.
	Value reflect.Value
}

// Key is the dotted YAML path of the setting.
func (s Setting) Key() string { return strings.Join(s.Path, ".") }

// Walk returns every setting of dst, which must be a pointer to a struct, in
// field order. prefix starts every derived variable name.
func Walk(prefix string, dst any) []Setting {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		panic(fmt.Sprintf("settings: Walk wants a pointer to a struct, got %T", dst))
	}
	return walk(prefix, v.Elem(), nil, nil)
}

func walk(prefix string, v reflect.Value, path []string, out []Setting) []Setting {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, skip := yamlName(f)
		if skip {
			env := f.Tag.Get("env")
			if env == "" {
				continue
			}
			checkLeaf(f)
			out = append(out, Setting{
				Path:    append(append([]string(nil), path...), envOnlyKey(prefix, path, f)),
				Env:     env,
				EnvOnly: true,
				Value:   v.Field(i),
			})
			continue
		}
		sub := append(append([]string(nil), path...), name)
		if f.Type.Kind() == reflect.Struct {
			out = walk(prefix, v.Field(i), sub, out)
			continue
		}
		checkLeaf(f)
		out = append(out, Setting{Path: sub, Env: envName(prefix, sub), Value: v.Field(i)})
	}
	return out
}

// envName is the variable of the setting at path: prefix and the keys in upper
// case, joined by "_".
func envName(prefix string, path []string) string {
	return prefix + "_" + strings.ToUpper(strings.Join(path, "_"))
}

// envOnlyKey is the key a YAML file would give an env-only field: its variable
// without the prefix and the keys above it, in lower case, so that
// SOLTI_AGENT_AUTH_ENROLL_SECRET under agent_auth is enroll_secret. A variable
// that does not follow that shape gives the lower-cased field name.
func envOnlyKey(prefix string, path []string, f reflect.StructField) string {
	env := f.Tag.Get("env")
	head := prefix + "_"
	if len(path) > 0 {
		head = envName(prefix, path) + "_"
	}
	if rest, ok := strings.CutPrefix(env, head); ok && rest != "" {
		return strings.ToLower(rest)
	}
	return strings.ToLower(f.Name)
}

func yamlName(f reflect.StructField) (name string, skip bool) {
	tag := f.Tag.Get("yaml")
	if tag == "-" {
		return "", true
	}
	name, _, _ = strings.Cut(tag, ",")
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	return name, false
}

func checkLeaf(f reflect.StructField) {
	switch t := f.Type; {
	case t == durationType, t.Kind() == reflect.String, t.Kind() == reflect.Bool,
		t.Kind() >= reflect.Int && t.Kind() <= reflect.Int64,
		t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.String:
		return
	}
	panic(fmt.Sprintf("settings: field %s has type %s, which no environment variable can set", f.Name, f.Type))
}

// Sections returns the YAML keys of the struct-valued fields directly under the
// root of dst, in field order.
func Sections(dst any) []string {
	t := reflect.TypeOf(dst)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		if name, skip := yamlName(f); f.IsExported() && !skip && f.Type.Kind() == reflect.Struct {
			out = append(out, name)
		}
	}
	return out
}

// ApplyEnv sets every setting of dst whose variable lookup finds. A value that
// does not parse fails, naming the variable and never the value, which may be a
// secret; all such failures are returned together.
func ApplyEnv(prefix string, dst any, lookup func(string) (string, bool)) error {
	var errs []error
	for _, s := range Walk(prefix, dst) {
		raw, ok := lookup(s.Env)
		if !ok {
			continue
		}
		if err := set(s.Value, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Env, err))
		}
	}
	return errors.Join(errs...)
}

func set(v reflect.Value, raw string) error {
	t := v.Type()
	switch {
	case t.Kind() == reflect.String:
		v.SetString(raw)
		return nil
	case t.Kind() == reflect.Slice:
		v.Set(reflect.ValueOf(splitList(raw)))
		return nil
	case raw == "":
		return errors.New("is empty, want a value")
	case t == durationType:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return errors.New("not a valid duration (like 30s or 1m30s)")
		}
		v.SetInt(int64(d))
	case t.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("not a valid boolean (true or false)")
		}
		v.SetBool(b)
	default:
		n, err := strconv.ParseInt(raw, 10, t.Bits())
		if err != nil {
			return errors.New("not a valid integer")
		}
		v.SetInt(n)
	}
	return nil
}

// splitList splits on commas and trims spaces around each item; an empty value
// is an empty list. Empty items stay, for the setting's own validation to name.
func splitList(raw string) []string {
	if raw == "" {
		return nil
	}
	items := strings.Split(raw, ",")
	for i := range items {
		items[i] = strings.TrimSpace(items[i])
	}
	return items
}

// CheckYAML reports the first key of the mapping node that dst does not take:
// an unknown key, with the keys known at its level, or the key of an env-only
// setting. file names the source in the message, with the key's line; prefix is
// the one [Walk] takes.
func CheckYAML(file, prefix string, node *yaml.Node, dst any) error {
	t := reflect.TypeOf(dst)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return checkNode(file, node, t, prefix, nil)
}

func checkNode(file string, n *yaml.Node, t reflect.Type, prefix string, path []string) error {
	if n.Kind != yaml.MappingNode {
		return nil // the decoder reports a value of the wrong shape
	}
	type field struct {
		typ reflect.Type
		env string // set for an env-only setting
	}
	var (
		fields = map[string]field{}
		known  []string
	)
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, skip := yamlName(f)
		if skip {
			if env := f.Tag.Get("env"); env != "" {
				fields[envOnlyKey(prefix, path, f)] = field{f.Type, env}
			}
			continue
		}
		fields[name] = field{typ: f.Type}
		known = append(known, name)
	}
	sort.Strings(known)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		full := strings.Join(append(append([]string(nil), path...), k.Value), ".")
		f, ok := fields[k.Value]
		switch {
		case !ok:
			return fmt.Errorf("%s:%d: unknown key %q (known: %s)", file, k.Line, full, strings.Join(known, ", "))
		case f.env != "":
			return fmt.Errorf("%s:%d: %s is read from %s only; remove it from the file", file, k.Line, full, f.env)
		case f.typ.Kind() == reflect.Struct:
			if err := checkNode(file, v, f.typ, prefix, append(append([]string(nil), path...), k.Value)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Closest returns the candidate within edit distance 2 of name, the nearest and,
// among equals, the first in sorted order; false when none is that near.
func Closest(name string, candidates []string) (string, bool) {
	sorted := append([]string(nil), candidates...)
	sort.Strings(sorted)
	best, bestDist := "", 3
	for _, c := range sorted {
		if d := distance(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best, bestDist <= 2
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
