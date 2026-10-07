package runtime_test

import (
	"errors"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/stretchr/testify/require"
)

func TestTerminalFailure(t *testing.T) {
	cause := errors.New("private process cause")
	err := runtime.NewTerminalFailure("Authorization: Custom Q7m9v2R8d4", cause)
	require.ErrorIs(t, err, cause)
	require.Equal(t, "upstream error (details withheld: possible credential)", err.PublicMessage())
	require.Equal(t, err.PublicMessage(), err.Error())
	require.NotContains(t, err.Error(), cause.Error())
	require.Equal(t, "Harness runtime execution failed", runtime.NewTerminalFailure(" ", cause).PublicMessage())
	var zero runtime.TerminalFailure
	require.Equal(t, "Harness runtime execution failed", zero.PublicMessage())
}
