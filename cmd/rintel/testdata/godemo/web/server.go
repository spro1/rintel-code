package web

import (
	"fmt"
	"net/http"

	"example.com/godemo/store"
)

type Server struct {
	store  store.Store
	notify func(string)
}

func New(s store.Store, notify func(string)) *Server {
	return &Server{store: s, notify: notify}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", s.show)
	mux.HandleFunc("POST /users", func(w http.ResponseWriter, r *http.Request) {
		s.create(w, r)
	})
	return mux
}

func (s *Server) show(w http.ResponseWriter, r *http.Request) {
	fail := func(code int) { w.WriteHeader(code) }
	u, err := s.store.Find(1)
	if err != nil {
		fail(http.StatusNotFound)
		return
	}
	fmt.Fprint(w, u.Name)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	u := &store.User{ID: 1, Name: r.FormValue("name")}
	if err := s.store.Save(u); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.notify(u.Name)
}
