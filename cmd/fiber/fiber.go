package main

import (
	"fmt"
	"github.com/Tryanks/fiber-webdav"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"net/http"
)

func main() {
	app := fiber.New(fiber.Config{
		Immutable:      true,
		RequestMethods: append(fiber.DefaultMethods[:], webdav.Methods...),
	})

	app.All("/*", func() fiber.Handler {
		w := &webdav.Handler{
			FileSystem: webdav.NewMemFS(),
			LockSystem: webdav.NewMemLS(),
			Logger: func(request *http.Request, err error) {
				fmt.Println("\t", request.Method, request.URL.Path)
				if err != nil {
					fmt.Println("\t\tERROR:", err)
				}
			},
		}
		return adaptor.HTTPHandler(w)
	}())

	err := app.Listen(":3000")
	if err != nil {
		panic(err)
	}
}
