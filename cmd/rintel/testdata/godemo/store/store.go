// Package store keeps users.
package store

import "errors"

// ErrNotFound is returned when a user does not exist.
var ErrNotFound = errors.New("not found")

type User struct {
	ID   int
	Name string
}

// Store is what the web layer needs.
type Store interface {
	Find(id int) (*User, error)
	Save(u *User) error
}

type Memory struct {
	users map[int]*User
}

func NewMemory() *Memory {
	return &Memory{users: map[int]*User{}}
}

func (m *Memory) Find(id int) (*User, error) {
	if u, ok := m.users[id]; ok {
		return u, nil
	}
	return nil, ErrNotFound
}

func (m *Memory) Save(u *User) error {
	if u == nil || u.Name == "" {
		return errors.New("invalid user")
	}
	m.users[u.ID] = u
	return nil
}
