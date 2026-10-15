package cachekey

import (
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// A movable consumer key can be confirmed when the producer starts only if
// every job that can move it finishes before then or runs after the consumer.
func TestSettlesBeforeOrdersEveryWriterAroundTheProducer(t *testing.T) {
	job := func(name string, dependsOn ...string) *jobmodel.ScheduledJob {
		planned := declaredJob(name, name, &extension.TaskDeclaration{})
		planned.DependsOn = dependsOn
		return planned
	}
	before := job("test~layers")
	producer := job("test~env", before.Key())
	consumer := job("test~run", producer.Key())
	after := job("package~layers", consumer.Key())
	concurrent := job("build~layers")
	byKey := JobsByKey([]*jobmodel.ScheduledJob{before, producer, consumer, after, concurrent})

	cases := []struct {
		name    string
		writers []string
		want    bool
	}{
		{name: "a writer the producer waits for", writers: []string{before.Key()}, want: true},
		{name: "a writer that waits for the consumer", writers: []string{after.Key()}, want: true},
		{name: "a writer the schedule does not order", writers: []string{before.Key(), concurrent.Key()}, want: false},
	}
	for _, tc := range cases {
		movement := KeyMovement{Writers: map[string][]string{consumer.Key(): tc.writers}}
		if got := SettlesBefore(producer, consumer, movement, byKey); got != tc.want {
			t.Errorf("%s: SettlesBefore = %v, want %v", tc.name, got, tc.want)
		}
	}

	folded := job("test~run", producer.Key(), concurrent.Key())
	byKey[folded.Key()] = folded
	if SettlesBefore(producer, folded, KeyMovement{}, byKey) {
		t.Error("a consumer that folds the key of a job the producer does not wait for was confirmed")
	}
}
