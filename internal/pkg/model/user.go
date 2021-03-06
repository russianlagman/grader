package model

import (
	"github.com/google/uuid"
)

type User struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Password string    `json:"-"`
	IsAdmin  bool      `json:"-"`
}

func (u *User) Identity() string {
	return u.ID.String()
}
