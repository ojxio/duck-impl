🦆 `duck-impl` brings some duck typing developing experiences to Go, which are very common in other languages.

## Features

### Generate a duck struct for a your given interface, so that you can use anonymous functions to implement interfaces on runtime.

Sample usage:

In TypeScript, people can do:

```typescript
type Foo interface {
    Bar() string
}

function Baz(foo Foo) {
    fmt.Println(foo.Bar())
}

Baz({
    Bar: () => "Hello, world!",
});
```

Now, you can also do in Go:

```go
//go:generate go run github.com/ojxio/duck-impl -struct myStruct -interface Foo -outputFile Foo.gen.go

package main
import (
    "fmt"
)
type Foo interface {
    Bar() string
}

func main() {
    // use the name you provided by `-struct` flag
    Baz(myStruct{
        Bar_: func() string {
            return "Hello, world!"
        },
    })
    // Or, use the implicit name of the struct
    Baz(_Foo_{
        Bar_: func() string {
            return "Hello, world!"
        },
    })
}
```