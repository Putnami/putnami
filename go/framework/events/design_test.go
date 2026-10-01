package events

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"go.putnami.dev/app"
	protofeatures "go.putnami.dev/protocol/features"
)

func firstDesignHandler(context.Context, *Message[string]) error  { return nil }
func secondDesignHandler(context.Context, *Message[string]) error { return nil }

func TestDesignHandlerIDsDoNotDependOnRegistrationOrder(t *testing.T) {
	topic := NewTopic[string]("order.created")
	forward := describeEventHandlerIDs(t, []*HandlerDefinition{
		Handle(topic, firstDesignHandler),
		Handle(topic, secondDesignHandler),
	})
	reversed := describeEventHandlerIDs(t, []*HandlerDefinition{
		Handle(topic, secondDesignHandler),
		Handle(topic, firstDesignHandler),
	})

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("handler IDs changed with registration order:\nforward: %v\nreverse: %v", forward, reversed)
	}
	if len(forward) != 2 || forward[0] == forward[1] {
		t.Fatalf("handler IDs are not distinct: %v", forward)
	}
}

func describeEventHandlerIDs(t *testing.T, handlers []*HandlerDefinition) []string {
	t.Helper()
	output := t.TempDir()
	feature := app.NewModule("orders").Feature(app.Feature{
		ID:      "orders/consume",
		Name:    "Order consumption",
		Outcome: "Orders are consumed",
		Owner:   "samples",
	}).Use(Events(PluginConfig{Handlers: handlers}))
	application := app.New("events-design")
	application.Use(feature)

	if err := application.Describe(output, []string{"design"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(protofeatures.DesignGraphArtifact)))
	if err != nil {
		t.Fatalf("read design graph: %v", err)
	}
	graph, err := protofeatures.ParseDesignGraph(body)
	if err != nil {
		t.Fatalf("parse design graph: %v", err)
	}
	var ids []string
	for _, node := range graph.Nodes {
		if node.Kind == protofeatures.DesignNodeEventHandler {
			ids = append(ids, node.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
