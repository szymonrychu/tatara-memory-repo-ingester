package consumer

import "example.com/dep/pkg"

// UseDepFunc calls the cross-repo exported func Do.
func UseDepFunc() int {
	return pkg.Do()
}

// UseDepMethod calls the cross-repo exported method M on T.
func UseDepMethod() int {
	t := pkg.T{}
	return t.M()
}
