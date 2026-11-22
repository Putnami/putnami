# Fixture

A complete file type-checks:

```go
package main

import "strings"

func main() { _ = strings.ToUpper("ok") }
```

A fragment may use names it does not declare:

```go
result := strings.ToUpper(input)
```

A block declares a type the next one uses:

```go
type User struct {
    Name string
}
```

```go
user := User{Name: "Ada"}
_ = user.Email
```

A call to a function the package does not have:

```go
strings.NoSuchFunction("x")
```

An argument of the wrong type, through an import another block declares:

```go
_ = strings.ToUpper(42)
```

Pseudo-code is marked:

```go nocheck
func handler(...) { ... }
```

```go
func handler(...) { ... }
```

A package no block of the document imports, found by name:

```go
a := app.New("x")
a.Use(app.NoSuchPlugin())
```

A block of statements may return, but a function it writes is checked:

```go
if ready {
    return nil
}
split := func(s string) string {
    return strings.Cut(s, ",")
}
```

A workspace package whose name is unique, with no selector it exports:

```go
container := inject.NoSuchConstructor()
```

A third-party package or a variable that shares a workspace package's name
stays a name the fragment does not declare:

```go
server := grpc.NewServer()
runner.Run(ctx)
data, err := proto.Marshal(msg)
```

A dependency's name does not take over a framework module's drift, and a
variable named like a dependency stays a name the fragment does not declare:

```go
cfg := config.Load(42)
plugins := grpc.NewPlugins()
fmt.Println(state.Get("key"), module.Path)
```
