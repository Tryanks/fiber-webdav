package main

import (
	"fmt"
	"github.com/Tryanks/fiber-webdav"
	"github.com/gofiber/fiber/v3"
)

func main() {
	app := fiber.New(fiber.Config{
		Immutable:      true,
		RequestMethods: webdav.ExtendedMethods,
	})

	//root, err := webdav.NewRootFileSystem("/tmp")
	//if err != nil {
	//	panic(err)
	//}
	//w := webdav.NewWebdavServer("/webdav", root, webdav.NewMemLS())

	w := webdav.NewWebdavServer("", webdav.NewMemFS(), webdav.NewMemLS())
	w.Logger = func(i int, err error) {
		fmt.Printf("Status code: %d, Error: %s\n", i, err)
	}

	app.All("*", w.ServeFiber)

	err := app.Listen(":3000")
	if err != nil {
		panic(err)
	}
}
