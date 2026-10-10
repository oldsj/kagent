package env

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQuiesceDelayEnvironment(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{value: ""},
		{value: "0"},
		{value: "15m", want: 15 * time.Minute},
		{value: "1h", want: time.Hour}, // The store clamps parsed durations.
		{value: "invalid"},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv(QuiesceDelay.Name(), test.value)
			require.Equal(t, test.want, QuiesceDelay.Get())
		})
	}
}
