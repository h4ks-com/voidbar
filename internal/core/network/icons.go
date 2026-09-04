package network

import (
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/h4ks-com/voidbar/internal/storage"
)

// iconHTTP fetches upstream network icons. Icons are small, so a tight
// timeout beats a stuck download holding the notify path. TLS is pinned
// to 1.2: some ISP-level DPI setups reset 1.3 handshakes to lesser-known
// hosts (observed live: schannel at 1.2 reaches the same URL Go's 1.3
// default could not).
var iconHTTP = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{MaxVersion: tls.VersionTLS12},
	},
}

// GuildIconValue renders the guild "icon" field: the mirrored hash when
// the upstream advertises draft/ICON, nil otherwise (the client then
// draws its letter-tile fallback).
func GuildIconValue(net *storage.Network) any {
	if net == nil || net.IconHash == "" {
		return nil
	}
	return net.IconHash
}

// OnNetworkIcon mirrors an upstream draft/ICON URL into the avatar store
// and re-announces the guild so connected clients fetch the new icon.
// Called from the IRC manager's ISUPPORT hook (SetIconNotifier); the
// return reports success so the manager can retry failed mirrors on the
// next reconnect.
func (s *Service) OnNetworkIcon(userID, networkID, iconURL string) bool {
	net, err := s.store.GetNetwork(networkID)
	if err != nil {
		s.log.Warn("icon: network gone", "user", userID, "network", networkID, "err", err)
		return false
	}
	if net.IconURL == iconURL && net.IconHash != "" {
		return true // already mirrored
	}
	fetchURL := strings.ReplaceAll(iconURL, "{size}", "128")
	resp, err := iconHTTP.Get(fetchURL)
	if err != nil {
		s.log.Warn("icon: fetch failed", "user", userID, "url", fetchURL, "err", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.log.Warn("icon: fetch status", "user", userID, "url", fetchURL, "status", resp.StatusCode)
		return false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		s.log.Warn("icon: read failed", "user", userID, "url", fetchURL, "err", err)
		return false
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || strings.HasPrefix(ct, "text/") {
		ct = "image/png"
	}
	hash, err := s.store.PutAvatar(data, ct)
	if err != nil {
		s.log.Warn("icon: store failed", "user", userID, "err", err)
		return false
	}
	if err := s.store.SetNetworkIcon(networkID, hash, iconURL); err != nil {
		s.log.Warn("icon: persist failed", "user", userID, "err", err)
		return false
	}
	updated, err := s.store.GetNetwork(networkID)
	if err != nil {
		updated = net
		updated.IconHash, updated.IconURL = hash, iconURL
	}
	s.log.Info("network icon mirrored", "user", userID, "network", networkID, "hash", hash, "bytes", len(data))
	if m, err := s.store.GetMembership(networkID, userID); err == nil && s.gw != nil {
		s.gw.Dispatch(userID, "GUILD_UPDATE", s.guildUpdatePayload(m, updated))
	}
	return true
}
