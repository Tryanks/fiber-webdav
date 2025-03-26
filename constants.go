package webdav

import "github.com/gofiber/fiber/v3"

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

var ExtendedMethods = append(fiber.DefaultMethods[:], Methods...)
