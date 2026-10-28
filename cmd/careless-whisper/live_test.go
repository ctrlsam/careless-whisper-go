package main

import (
	"reflect"
	"testing"
)

func TestSplitPairPhones(t *testing.T) {
	cases := map[string]struct {
		input string
		want  []string
	}{
		"empty is nil":          {"", nil},
		"whitespace is nil":     {"  ", nil},
		"single":                {"15551234567", []string{"15551234567"}},
		"trims each entry":      {" 1555, 1666 ", []string{"1555", "1666"}},
		"preserves empty slots": {"1555,,1777", []string{"1555", "", "1777"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := splitPairPhones(tc.input); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitPairPhones(%q) = %#v, want %#v", tc.input, got, tc.want)
			}
		})
	}
}
