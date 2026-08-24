package model

import "github.com/h4ks-com/voidbar/internal/storage"

type User struct {
	ID            string  `json:"id"`
	Username      string  `json:"username"`
	Discriminator string  `json:"discriminator"`
	Avatar        *string `json:"avatar"`
	Bot           bool    `json:"bot"`
	System        bool    `json:"system"`
	Flags         int     `json:"flags"`
	PublicFlags   int     `json:"public_flags"`
	Email         string  `json:"email"`
	Verified      bool    `json:"verified"`
	MFAEnabled    bool    `json:"mfa_enabled"`
}

func ToUser(u *storage.User) *User {
	return &User{
		ID:            u.ID,
		Username:      u.Username,
		Discriminator: "0",
		Avatar:        nil,
		Email:         u.Email,
		Verified:      true,
	}
}
