`duck-impl` brings some duck typing developing experiences to Go, which are very common in other languages.

Sample usage:

In TypeScript, people can do this:

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

With `duck-impl`, now you can do this in Go:

```go
//go:generate go run github.com/ojxio/duck-impl/cmd/duck-impl -struct myStruct -interface Foo -outputFile Foo.gen.go

package main
import (
    "fmt"
)
type Foo interface {
    Bar() string
}

func main() {
    Baz(myStruct{
        Bar_: func() string {
            return "Hello, world!"
        },
    })
    // Or
    Baz(_Foo_{
        Bar_: func() string {
            return "Hello, world!"
        },
    })
}
```

Known issues:

- can't import dependent packages used by function's results