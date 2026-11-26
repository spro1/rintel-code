package web

import (
	"testing"

	"example.com/godemo/store"
)

func TestRoutes(t *testing.T) {
	if New(store.NewMemory(), func(string) {}).Routes() == nil {
		t.Fatal("no routes")
	}
}
