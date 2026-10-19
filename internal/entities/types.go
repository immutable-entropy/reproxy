package entities

import (
	"net/netip"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Type is what an entity is.
type Type string

const (
	// TypeDomain is a registrable domain, e.g. example.com.
	TypeDomain Type = "domain"
	// TypeHost is an origin (https://api.example.com:8443), or an
	// address-only host (10.0.0.5) that the origins on that address sit
	// under.
	TypeHost Type = "host"
	// TypeURL is a templated URL on a host, e.g. https://host/users/{id}.
	TypeURL Type = "url"
)

// Types lists every type, outermost first, in the order the UI offers them.
var Types = []Type{TypeDomain, TypeHost, TypeURL}

// Valid reports whether t is one of Types.
func (t Type) Valid() bool { return t == TypeDomain || t == TypeHost || t == TypeURL }

// RegistrableDomain is the domain a host name is registered under (its
// eTLD+1, as example.com is for api.example.com), or "" when it has none:
// an IP address, a single label such as localhost, or a public suffix
// itself. The name may carry a port.
func RegistrableDomain(host string) string {
	name := BareHost(host)
	if name == "" || !strings.Contains(name, ".") {
		return ""
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return ""
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(strings.TrimSuffix(name, "."))
	if err != nil {
		return ""
	}
	return d
}

// DomainKey is the key of the domain entity for a registrable domain.
func DomainKey(name string) string { return strings.ToLower(name) }

// HostKey is the key of a host entity: scheme://host for an origin, and
// //name for a name host (scheme ""), the way a URL writes a host on any
// scheme, so an apex's key (//example.com) can't clash with its domain's
// (example.com).
func HostKey(scheme, host string) string {
	if scheme == "" {
		return "//" + BareHost(host)
	}
	return scheme + "://" + host
}

// parentOf is the type and address of the entity e sits under; ok is false
// for a root.
//
// A URL sits under its origin; an origin under the host for its name (the
// name or address alone, no scheme or port: what DNS knows it by); a name
// under its registrable domain, or, for one that has none (an address, a
// single label), nothing. A domain is a root.
func parentOf(e *Entity) (t Type, scheme, host string, ok bool) {
	switch e.Type {
	case TypeURL:
		return TypeHost, e.Scheme, e.Host, true
	case TypeHost:
		if e.Scheme != "" {
			return TypeHost, "", BareHost(e.Host), true
		}
		if d := RegistrableDomain(e.Host); d != "" {
			return TypeDomain, "", d, true
		}
	}
	return "", "", "", false
}

// keyOf is the key of the entity of type t at an address.
func keyOf(t Type, scheme, host, path string) string {
	switch t {
	case TypeDomain:
		return DomainKey(host)
	case TypeHost:
		return HostKey(scheme, host)
	}
	return scheme + "://" + host + path
}

// newEntity is an entity of type t at an address, with its attributes and
// parent worked out.
func newEntity(t Type, scheme, host, path, targetID, origin string) *Entity {
	e := &Entity{
		Type: t, Key: keyOf(t, scheme, host, path), Scheme: scheme, Host: host, Path: path,
		TargetID: targetID, Origin: origin,
	}
	switch t {
	case TypeDomain:
		e.Host = DomainKey(host)
		suffix, icann := publicsuffix.PublicSuffix(e.Host)
		e.DomainAttrs = &DomainAttrs{Suffix: suffix, ICANN: icann}
	case TypeHost:
		a := &HostAttrs{}
		if scheme == "" {
			e.Host = BareHost(host)
		} else {
			a.Port = portOf(scheme, host)
		}
		ip, err := netip.ParseAddr(BareHost(host))
		a.IP = err == nil && ip.IsValid()
		e.HostAttrs = a
	case TypeURL:
		e.URLAttrs = &URLAttrs{}
	}
	if pt, ps, ph, ok := parentOf(e); ok {
		e.Parent = keyOf(pt, ps, ph, "")
	}
	return e
}

// portOf is the port an origin is served on: the host's own, or the
// scheme's default.
func portOf(scheme, host string) int {
	if b := BareHost(host); len(host) > len(b) {
		rest := strings.TrimPrefix(host[strings.LastIndex(host, b)+len(b):], "]")
		if p, ok := strings.CutPrefix(rest, ":"); ok {
			n := 0
			for _, c := range p {
				if c < '0' || c > '9' {
					return 0
				}
				n = n*10 + int(c-'0')
			}
			return n
		}
	}
	if scheme == "https" {
		return 443
	}
	return 80
}
