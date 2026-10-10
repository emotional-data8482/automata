package core_test

import (
	"context"
	"fmt"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// Summary verifies ownership and status without depending on readable history.
func ExampleRunHandle_Summary() {
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		panic(err)
	}
	defer runtime.Close()
	agent, err := core.New(script(say("done")), core.AgentConfig{MaxTurns: 2})
	if err != nil {
		panic(err)
	}
	ref, err := runtime.Register("support", "v1", agent)
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := runtime.Run(ctx, ref, "help", core.WithDeadline(time.Now().Add(time.Second)),
		core.WithIdempotencyKey("tenant-a", "ticket-1"), core.WithLabels(map[string]string{"queue": "support"}))
	if err != nil {
		panic(err)
	}
	summary, err := runtime.Handle(result.RunID).Summary(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(summary.Scope, summary.Status, summary.Labels["queue"])
	// Output: tenant-a completed support
}
