# fiber-webdav

[![Go Reference](https://pkg.go.dev/badge/github.com/Tryanks/fiber-webdav.svg)](https://pkg.go.dev/github.com/Tryanks/fiber-webdav)

A Go-Fiber library for [WebDAV]. Forked from [emersion/go-webdav](https://github.com/emersion/go-webdav).

## Installation

```bash
go get -u github.com/Tryanks/fiber-webdav
```

## Usage

### Basic WebDAV Server

Here's a simple example of how to create a WebDAV server using Fiber:

```go
package main

import (
	"github.com/Tryanks/fiber-webdav"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/log"
	"github.com/gofiber/fiber/v2/middleware/logger"
)

func main() {
	// Create a new Fiber app with WebDAV extended methods
	app := fiber.New(fiber.Config{
		RequestMethods: webdav.ExtendedMethods,
	})

	// Add logger middleware
	app.Use(logger.New())

	// Mount WebDAV handler at root path
	app.Use("/", webdav.New(webdav.Config{
		Prefix: "/",
		Root:   webdav.LocalFileSystem("."), // Serve current directory
		Lock:   true,                        // Enable WebDAV locking
	}))

	// Start server on port 8080
	err := app.Listen(":8080")
	if err != nil {
		log.Fatal(err)
	}
}
```

### Configuration Options

The `webdav.Config` struct accepts the following options:

- `Prefix`: The URL path prefix to mount the WebDAV server on
- `Root`: The base directory for the WebDAV server (implements `webdav.FileSystem` interface)
- `Lock`: Boolean to enable WebDAV locking support

### WebDAV Methods Support

To enable WebDAV support in your Fiber application, you must initialize the Fiber app with extended request methods:

```go
fiber.Config{
    RequestMethods: webdav.ExtendedMethods,
}
```

## License

[MIT from emersion](https://github.com/emersion/go-webdav/blob/master/LICENSE)

[WebDAV]: https://tools.ietf.org/html/rfc4918
