package main

import (
	"github.com/Tryanks/fiber-webdav"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/log"
	"github.com/gofiber/fiber/v2/middleware/logger"
)

func main() {
	app := fiber.New(fiber.Config{
		RequestMethods: webdav.ExtendedMethods,
	})
	app.Use(logger.New())

	app.Use("/", webdav.New(webdav.Config{
		Prefix: "/",
		Root:   webdav.LocalFileSystem("."),
		Lock:   true,
	}))

	err := app.Listen(":8080")
	if err != nil {
		log.Fatal(err)
	}
}
