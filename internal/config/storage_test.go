//go:build windows

package config

import (
	"reflect"
	"testing"
)

func TestNormalizeStoragePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`E:\HyperV`, `E:\HyperV`},
		{`E:\hyperv`, `E:\hyperv`}, // case-insensitive leaf match, kept as-is
		{`E:\`, `E:\HyperV`},
		{`H:\VMs`, `H:\VMs\HyperV`},
		{`  H:\VMs\HyperV  `, `H:\VMs\HyperV`}, // trims blanks
		{``, ``},
		{`   `, ``},
	}
	for _, c := range cases {
		if got := normalizeStoragePath(c.in); got != c.want {
			t.Errorf("normalizeStoragePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseAdditionalStorage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "example from spec adds HyperV leaf and trims trailing separator",
			in:   `E:\HyperV;H:\HyperV;`,
			want: []string{`E:\HyperV`, `H:\HyperV`},
		},
		{
			name: "adds HyperV suffix when missing",
			in:   `E:\;H:\VMs`,
			want: []string{`E:\HyperV`, `H:\VMs\HyperV`},
		},
		{
			name: "dedupes same drive keeping first",
			in:   `E:\HyperV;E:\VM\TESTE\HyperV`,
			want: []string{`E:\HyperV`},
		},
		{
			name: "empty input",
			in:   ``,
			want: nil,
		},
		{
			name: "blank segments ignored",
			in:   `E:\HyperV; ; ;H:\HyperV`,
			want: []string{`E:\HyperV`, `H:\HyperV`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseAdditionalStorage(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseAdditionalStorage(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}
