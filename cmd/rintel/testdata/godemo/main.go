package main

import (
	"log"
	"net/http"

	"example.com/godemo/store"
	"example.com/godemo/web"
)

func main() {
	srv := web.New(store.NewMemory(), func(name string) { log.Println("created", name) })
	log.Fatal(http.ListenAndServe(":8080", srv.Routes()))
}
