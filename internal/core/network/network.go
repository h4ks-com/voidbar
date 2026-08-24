// Package network implements the create-or-join of IRC networks from a
// connection string (Voidbar's invite), membership management and the glue
// that spawns per-user upstream connections and mirrors IRC state into
// Discord gateway events.
package network

import (
	"errors"
	"fmt"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/irc/connstr"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

var ErrBadConnString = errors.New("invalid connection string")

type Service struct {
	store   *storage.Storage
	gw      *gateway.Server
	sf      *util.Snowflake
	manager *ircmanage.Manager
}

func NewService(store *storage.Storage, gw *gateway.Server, sf *util.Snowflake, manager *ircmanage.Manager) *Service {
	return &Service{store: store, gw: gw, sf: sf, manager: manager}
}

// Join parses a connection string (the invite), creates the network if it
// does not exist, records membership and spawns the user's own upstream
// connection. It is idempotent: joining the same connection string again
// returns the existing network.
func (s *Service) Join(userID, raw string) (*storage.Network, error) {
	conn, err := connstr.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadConnString, err)
	}

	connID := conn.ID()
	net, err := s.store.NetworkByConnID(connID)
	if errors.Is(err, storage.ErrNotFound) {
		net = &storage.Network{
			ID:        s.sf.New(),
			ConnID:    connID,
			Name:      conn.DisplayName(),
			Host:      conn.Host,
			Port:      conn.Port,
			TLS:       conn.TLS,
			Password:  conn.Password,
			CreatedBy: userID,
			CreatedAt: time.Now().UTC(),
		}
		if err := s.store.UpsertNetwork(net); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	// Nickname is per-user; the connection string may not carry one, and even
	// if it does, each member keeps their own identity.
	nick := username(userID)
	if u, err := s.store.GetUserByID(userID); err == nil && u.Username != "" {
		nick = u.Username
	}

	mem, err := s.store.GetMembership(net.ID, userID)
	if errors.Is(err, storage.ErrNotFound) {
		mem = &storage.Membership{
			UserID:    userID,
			NetworkID: net.ID,
			Nick:      nick,
			Username:  nick,
			Realname:  nick,
			AutoJoin:  conn.Channels,
			JoinedAt:  time.Now().UTC(),
		}
		if err := s.store.UpsertMembership(mem); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	if s.manager != nil {
		s.manager.EnsureConn(userID, net.ID)
	}
	return net, nil
}

// Network returns the network record by id.
func (s *Service) Network(id string) (*storage.Network, error) {
	return s.store.GetNetwork(id)
}

// MembershipFor returns the user's membership on a network.
func (s *Service) MembershipFor(userID, netID string) (*storage.Membership, error) {
	return s.store.GetMembership(netID, userID)
}

func username(userID string) string {
	// Snowflakes are too long for nicks; fall back to a stable short form.
	h := util.SHA256Hex(userID)
	return "vb_" + h[:9]
}

// GuildsForUser assembles the Discord wire representation of every network
// the user belongs to, for READY.
func (s *Service) GuildsForUser(userID string) ([]any, error) {
	memberships, err := s.store.ListMembershipsForUser(userID)
	if err != nil {
		return nil, err
	}
	guilds := make([]any, 0, len(memberships))
	for _, m := range memberships {
		net, err := s.store.GetNetwork(m.NetworkID)
		if err != nil {
			continue
		}
		guilds = append(guilds, map[string]any{
			"id":          net.ID,
			"name":        net.Name,
			"unavailable": false,
			"joined_at":   m.JoinedAt.Format(time.RFC3339),
		})
	}
	return guilds, nil
}
