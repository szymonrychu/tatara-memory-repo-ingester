package pkg

func G() int { return 1 }

func F() int { return G() + 1 }
