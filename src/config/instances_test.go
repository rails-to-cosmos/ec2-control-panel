package config

import "testing"

func TestCanRead(t *testing.T) {
	cases := []struct {
		name    string
		cfg     InstanceConfig
		user    string
		isAdmin bool
		want    bool
	}{
		{"admin sees a closed instance", InstanceConfig{}, "bob", true, true},
		{"empty readers means admins only", InstanceConfig{}, "bob", false, false},
		{"listed reader", InstanceConfig{Readers: []string{"bob"}}, "bob", false, true},
		{"unlisted reader", InstanceConfig{Readers: []string{"alice"}}, "bob", false, false},
		{"public", InstanceConfig{Readers: []string{ReadersPublic}}, "bob", false, true},
		{"owner without a readers list", InstanceConfig{Owner: "bob"}, "bob", false, true},
		{"owner excluded from readers", InstanceConfig{Owner: "bob", Readers: []string{"alice"}}, "bob", false, true},
		{"anonymous never matches an empty owner", InstanceConfig{}, "", false, false},
		{"owner match is case-sensitive", InstanceConfig{Owner: "Bob"}, "bob", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.CanRead(tc.user, tc.isAdmin); got != tc.want {
				t.Errorf("CanRead(%q, %v) = %v, want %v", tc.user, tc.isAdmin, got, tc.want)
			}
		})
	}
}
