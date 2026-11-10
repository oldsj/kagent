package substrate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
)

func TestResolveDeadlines(t *testing.T) {
	scenarios := []struct {
		name                       string
		call, lifecycle, operation time.Duration
		want                       Deadlines
		wantErr                    bool
	}{
		{name: "Given no configured deadlines, when they resolve, assert that the snapshot-sized defaults apply",
			want: Deadlines{Call: 30 * time.Second, Lifecycle: 5 * time.Minute, Operation: 10 * time.Minute}},
		{name: "Given configured deadlines, when they resolve, assert that they are kept",
			call: time.Minute, lifecycle: 20 * time.Minute, operation: time.Hour,
			want: Deadlines{Call: time.Minute, Lifecycle: 20 * time.Minute, Operation: time.Hour}},
		{name: "Given an attempt bound equal to the lifecycle call bound, when they resolve, assert that startup is refused",
			lifecycle: 10 * time.Minute, operation: 10 * time.Minute, wantErr: true},
		{name: "Given a lifecycle call bound above the default attempt bound, when they resolve, assert that startup is refused",
			lifecycle: 15 * time.Minute, wantErr: true},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			got, err := ResolveDeadlines(scenario.call, scenario.lifecycle, scenario.operation)

			if scenario.wantErr {
				require.ErrorContains(t, err, "operationTimeout")
				return
			}
			require.NoError(t, err)
			require.Equal(t, scenario.want, got)
		})
	}
}

func TestLifecycleCallDeadlines(t *testing.T) {
	t.Run("Given a lifecycle call bound, when snapshot-moving calls run, assert that they use it and other calls do not", func(t *testing.T) {
		fake := &deadlineFake{remaining: map[string]time.Duration{}}
		client := &Client{ControlClient: fake, cfg: Config{CallTimeout: time.Minute, LifecycleCallTimeout: time.Hour}}

		givenEveryActorCall(t, client)

		require.InDelta(t, time.Minute.Seconds(), fake.remaining["GetActor"].Seconds(), 5)
		for _, method := range []string{"ResumeActor", "RevertActor", "SuspendActor"} {
			require.InDelta(t, time.Hour.Seconds(), fake.remaining[method].Seconds(), 5, method)
		}
	})

	t.Run("Given no lifecycle call bound, when a suspend runs, assert that it falls back to the call bound", func(t *testing.T) {
		fake := &deadlineFake{remaining: map[string]time.Duration{}}
		client := &Client{ControlClient: fake, cfg: Config{CallTimeout: time.Minute}}

		_, err := client.SuspendActor(t.Context(), "team-a", "actor")

		require.NoError(t, err)
		require.InDelta(t, time.Minute.Seconds(), fake.remaining["SuspendActor"].Seconds(), 5)
	})
}

func givenEveryActorCall(t *testing.T, client *Client) {
	t.Helper()
	_, err := client.GetActor(t.Context(), "team-a", "actor")
	require.NoError(t, err)
	_, err = client.ResumeActor(t.Context(), "team-a", "actor")
	require.NoError(t, err)
	_, err = client.RevertActor(t.Context(), "team-a", "actor")
	require.NoError(t, err)
	_, err = client.SuspendActor(t.Context(), "team-a", "actor")
	require.NoError(t, err)
}

// deadlineFake records how long the calling context allows each RPC to run.
type deadlineFake struct {
	ateapipb.ControlClient
	remaining map[string]time.Duration
}

func (f *deadlineFake) record(method string, ctx context.Context) {
	deadline, ok := ctx.Deadline()
	if !ok {
		f.remaining[method] = -1
		return
	}
	f.remaining[method] = time.Until(deadline)
}

func (f *deadlineFake) GetActor(ctx context.Context, _ *ateapipb.GetActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.record("GetActor", ctx)
	return &ateapipb.Actor{}, nil
}

func (f *deadlineFake) ResumeActor(ctx context.Context, _ *ateapipb.ResumeActorRequest, _ ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	f.record("ResumeActor", ctx)
	return &ateapipb.ResumeActorResponse{}, nil
}

func (f *deadlineFake) RevertActor(ctx context.Context, _ *ateapipb.RevertActorRequest, _ ...grpc.CallOption) (*ateapipb.RevertActorResponse, error) {
	f.record("RevertActor", ctx)
	return &ateapipb.RevertActorResponse{}, nil
}

func (f *deadlineFake) SuspendActor(ctx context.Context, _ *ateapipb.SuspendActorRequest, _ ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	f.record("SuspendActor", ctx)
	return &ateapipb.SuspendActorResponse{Actor: &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}}}, nil
}
