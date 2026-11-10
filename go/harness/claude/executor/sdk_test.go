package executor

import "testing"

func TestSelectedDriver(t *testing.T) {
	for _, test := range []struct {
		env     []string
		want    driverMode
		wantErr bool
	}{
		{env: nil, want: modeProcess},
		{env: []string{"KAGENT_CLAUDE_DRIVER=process"}, want: modeProcess},
		{env: []string{"KAGENT_CLAUDE_DRIVER=process", "KAGENT_CLAUDE_DRIVER=sdk"}, want: modeSDK},
		{env: []string{"KAGENT_CLAUDE_DRIVER=node"}, wantErr: true},
	} {
		got, err := selectedDriver(test.env)
		if (err != nil) != test.wantErr || got != test.want {
			t.Errorf("selectedDriver(%v) = %q, %v", test.env, got, err)
		}
	}
}
