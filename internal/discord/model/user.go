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
	// Doubles as "this account has a date of birth" for the Android client
	// (MeUser.hasBirthday = nsfwAllowance != UNKNOWN); leaving it unset makes
	// accounts created after 2021-02-05 hit the un-dismissable
	// REGISTER_AGE_GATE modal on every boot.
	NsfwAllowed bool `json:"nsfw_allowed"`
}

// AvatarPtr lifts a stored avatar hash into the payload's *string (nil
// when unset, so the client falls back to its default disc).
func AvatarPtr(hash string) *string {
	if hash == "" {
		return nil
	}
	return &hash
}

func ToUser(u *storage.User) *User {
	return &User{
		ID:            u.ID,
		Username:      u.Username,
		Discriminator: "0",
		Avatar:        AvatarPtr(u.Avatar),
		Email:         u.Email,
		Verified:      true,
		NsfwAllowed:   true,
	}
}
