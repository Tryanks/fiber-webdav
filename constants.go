package webdav

const (
	MethodMkcol     = "MKCOL"
	MethodCopy      = "COPY"
	MethodMove      = "MOVE"
	MethodLock      = "LOCK"
	MethodUnlock    = "UNLOCK"
	MethodPropfind  = "PROPFIND"
	MethodProppatch = "PROPPATCH"
)

var Methods = []string{
	MethodMkcol,
	MethodCopy, MethodMove,
	MethodLock, MethodUnlock,
	MethodPropfind, MethodProppatch,
}
