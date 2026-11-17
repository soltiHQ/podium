package model

import "testing"

func TestGVKAcceptsAGrouplessAPIVersion(t *testing.T) {
	for _, tc := range []struct {
		apiVersion     string
		group, version string
		ok             bool
	}{
		{"solti.io/v1", "solti.io", "v1", true},
		{"v1", "", "v1", true},
		{"", "", "", false},
		{"/v1", "", "", false},
		{"group/", "", "", false},
		{"a/b/c", "", "", false},
	} {
		gvk, err := Workload{APIVersion: tc.apiVersion, Kind: "Widget", SchemaFingerprint: "abc"}.GVK()
		if (err == nil) != tc.ok {
			t.Errorf("%q: error %v, want ok=%v", tc.apiVersion, err, tc.ok)
			continue
		}
		if tc.ok && (gvk.Group != tc.group || gvk.Version != tc.version || gvk.Kind != "Widget" || gvk.SchemaFingerprint != "abc") {
			t.Errorf("%q: %+v", tc.apiVersion, gvk)
		}
	}
	if _, err := (Workload{APIVersion: "v1"}).GVK(); err == nil {
		t.Error("an empty kind is accepted")
	}
}
