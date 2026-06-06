package pkg

// Do is an exported function.
func Do() int { return 1 }

// T is an exported type with an exported method.
type T struct{}

// M is an exported method on T.
func (t T) M() int { return 2 }
