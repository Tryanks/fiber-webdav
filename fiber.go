package webdav

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/log"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"strings"
)

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

type Config struct {
	// Prefix is the URL path prefix to mount the WebDAV server on
	Prefix string

	// Root is the base directory for the WebDAV server
	Root FileSystem

	// Lock enables WebDAV locking support
	Lock bool
}

func New(config ...Config) fiber.Handler {
	if len(config) == 0 {
		log.Warn("webdav: configuration is nil - using empty handler")
		return func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusBadRequest).SendString("webdav: configuration required")
		}
	}
	c := config[0]
	prefix := c.Prefix

	w := &Handler{FileSystem: c.Root}
	if c.Lock {
		w.LockSystem = NewLockSystem()
	}
	handler := adaptor.HTTPHandler(w)
	return func(c *fiber.Ctx) error {
		c.Path(strings.TrimLeft(c.Path(), prefix))
		return handler(c)
	}
}
