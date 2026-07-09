package main

import "testing"

func TestEnvPort(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "no env set falls back to default",
			env:  map[string]string{},
			want: "44317",
		},
		{
			name: "bare PORT used when SOBS_PORT unset",
			env:  map[string]string{"PORT": "8080"},
			want: "8080",
		},
		{
			name: "bare SOBS_PORT takes priority over PORT",
			env:  map[string]string{"SOBS_PORT": "9000", "PORT": "8080"},
			want: "9000",
		},
		{
			name: "kubernetes service-link SOBS_PORT is ignored, falls through to PORT",
			env:  map[string]string{"SOBS_PORT": "tcp://10.152.183.152:44317", "PORT": "4317"},
			want: "4317",
		},
		{
			name: "kubernetes service-link SOBS_PORT with no PORT falls through to default",
			env:  map[string]string{"SOBS_PORT": "tcp://10.152.183.152:44317"},
			want: "44317",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"SOBS_PORT", "PORT"} {
				t.Setenv(k, tc.env[k])
			}
			if got := envPort("44317", "SOBS_PORT", "PORT"); got != tc.want {
				t.Errorf("envPort() = %q, want %q", got, tc.want)
			}
		})
	}
}
