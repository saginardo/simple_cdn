package domain

import (
	"errors"
	"net/netip"
	"strings"
)

// ParseSecurityIP gives events, persisted bans and firewall rules the same
// canonical public address. Mapped IPv4 addresses share the native IPv4 key.
func ParseSecurityIP(value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, errors.New("security IP must be an unscoped public address")
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() {
		return netip.Addr{}, errors.New("security IP must be a public address")
	}
	return address, nil
}
