package grpc

// interopConsumerSource is the consumer the interop test compiles and runs. It
// is an ordinary Putnami application: it declares one service binding and
// resolves three generated clients from the container, exactly the way a
// deployment does. It writes no URL, no header and no interceptor of its own —
// the three clients differ only in which transport their contract declares
// first.
const interopConsumerSource = `package main

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/client"
	"go.putnami.dev/inject"

	"generated.example/interop/connectjson"
	"generated.example/interop/connectproto"
	"generated.example/interop/restclient"
)

func fail(context string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", context, err)
	os.Exit(1)
}

// canonical re-encodes a decoded value with sorted members and the exact
// numeric text it arrived with, so a 64-bit value is compared as it was
// received rather than through a float.
func canonical(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		fail("encode", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		fail("decode", err)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		fail("re-encode", err)
	}
	return string(out)
}

type failure struct {
	Code   string ` + "`json:\"code\"`" + `
	Status int    ` + "`json:\"status\"`" + `
}

func failureOf(err error) failure {
	if err == nil {
		fail("declared error", stderrors.New("the provider answered a missing item"))
	}
	var remote *client.RemoteError
	if !stderrors.As(err, &remote) {
		fail("declared error", fmt.Errorf("%T is not a typed remote error: %v", err, err))
	}
	return failure{Code: remote.Code(), Status: remote.StatusCode}
}

func main() {
	if len(os.Args) < 2 {
		fail("arguments", stderrors.New("the provider URL is required"))
	}
	module := app.NewModule("interop-consumer")
	module.Use(client.Services(client.ServicesOptions{
		ClientID: "interop.consumer",
		Services: map[string]client.ServiceBinding{"interop": {
			URL: os.Args[1],
			Credentials: map[string]client.CredentialBinding{
				"apiKey": {Source: client.CredentialSourceStatic, Value: "interop-key"},
			},
		}},
	}))
	restclient.RegisterInteropClient(module)
	connectjson.RegisterInteropClient(module)
	connectproto.RegisterInteropClient(module)

	application := app.New("interop-consumer")
	application.Use(module)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- application.Start(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !application.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !application.IsRunning() {
		fail("consumer application", <-done)
	}

	restValue, err := application.Context().Get(inject.TokenOf[*restclient.InteropClient]())
	if err != nil {
		fail("resolve rest client", err)
	}
	jsonValue, err := application.Context().Get(inject.TokenOf[*connectjson.InteropClient]())
	if err != nil {
		fail("resolve connect+json client", err)
	}
	protoValue, err := application.Context().Get(inject.TokenOf[*connectproto.InteropClient]())
	if err != nil {
		fail("resolve connect+proto client", err)
	}
	rest := restValue.(*restclient.InteropClient)
	overJSON := jsonValue.(*connectjson.InteropClient)
	overProto := protoValue.(*connectproto.InteropClient)

	report := map[string]any{}

	restItem, err := rest.GetItems(ctx, restclient.GetItemsInput{Path: restclient.GetItemsPath{Id: "i-1"}})
	if err != nil {
		fail("rest unary", err)
	}
	report["restItem"] = canonical(restItem)
	jsonItem, err := overJSON.GetItems(ctx, connectjson.GetItemsInput{Path: connectjson.GetItemsPath{Id: "i-1"}})
	if err != nil {
		fail("connect+json unary", err)
	}
	report["connectJsonItem"] = canonical(jsonItem)
	protoItem, err := overProto.GetItems(ctx, connectproto.GetItemsInput{Path: connectproto.GetItemsPath{Id: "i-1"}})
	if err != nil {
		fail("connect+proto unary", err)
	}
	report["connectProtoItem"] = canonical(protoItem)

	_, restErr := rest.GetItems(ctx, restclient.GetItemsInput{Path: restclient.GetItemsPath{Id: "missing"}})
	report["restError"] = failureOf(restErr)
	_, jsonErr := overJSON.GetItems(ctx, connectjson.GetItemsInput{Path: connectjson.GetItemsPath{Id: "missing"}})
	report["connectJsonError"] = failureOf(jsonErr)
	_, protoErr := overProto.GetItems(ctx, connectproto.GetItemsInput{Path: connectproto.GetItemsPath{Id: "missing"}})
	report["connectProtoError"] = failureOf(protoErr)

	restStream, err := rest.ListItemsWatch(ctx, restclient.ListItemsWatchInput{})
	if err != nil {
		fail("rest stream", err)
	}
	values := []string{}
	for message := range restStream.Messages() {
		values = append(values, message.Id)
	}
	if err := restStream.Err(); err != nil {
		fail("rest stream terminal", err)
	}
	report["restStream"] = values

	jsonStream, err := overJSON.ListItemsWatch(ctx, connectjson.ListItemsWatchInput{})
	if err != nil {
		fail("connect+json stream", err)
	}
	values = []string{}
	for message := range jsonStream.Messages() {
		values = append(values, message.Id)
	}
	if err := jsonStream.Err(); err != nil {
		fail("connect+json stream terminal", err)
	}
	report["connectJsonStream"] = values

	protoStream, err := overProto.ListItemsWatch(ctx, connectproto.ListItemsWatchInput{})
	if err != nil {
		fail("connect+proto stream", err)
	}
	values = []string{}
	for message := range protoStream.Messages() {
		values = append(values, message.Id)
	}
	if err := protoStream.Err(); err != nil {
		fail("connect+proto stream terminal", err)
	}
	report["connectProtoStream"] = values

	encoded, err := json.Marshal(report)
	if err != nil {
		fail("report", err)
	}
	fmt.Println(string(encoded))
	cancel()
	<-done
	if err := application.Stop(context.Background()); err != nil {
		fail("stop", err)
	}
}
`
