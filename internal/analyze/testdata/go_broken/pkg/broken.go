package pkg

func H() int { return G() }

func G() int { return undefinedThing() } // undefinedThing is not declared -> type error
