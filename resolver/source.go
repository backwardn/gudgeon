package resolver

import (
	"net"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/miekg/dns"
)

const (
	ttl = 60 // default to a small ttl because some things (fire tv/kodi I'm looking at you) will hammer the DNS
)

type Source interface {
	Name() string
	Answer(rCon *RequestContext, context *ResolutionContext, request *dns.Msg) (*dns.Msg, error)
}

func NewSource(sourceSpecification string) Source {
	// a source that exists as a file is a hostfile source
	if _, err := os.Stat(sourceSpecification); !os.IsNotExist(err) {
		source, err := newZoneSourceFromFile(sourceSpecification)
		// if a source was returned but has an error...
		if source != nil && err != nil {
			log.Errorf("Parsing zone file: %s", err)
		}
		// if no source was returned (no records found in source format)
		if source == nil {
			return newHostFileSource(sourceSpecification)
		}
		return source
	}

	// a source that is an IP or that has other hallmarks of an address is a dns source
	if ip := net.ParseIP(sourceSpecification); ip != nil || strings.Contains(sourceSpecification, ":") || strings.Contains(sourceSpecification, "/") {
		return newDnsSource(sourceSpecification)
	}

	// fall back to looking for a resolver source which will basically be a no-op resolver in the event
	// that the named resolver doesn't exist
	return newResolverSource(sourceSpecification)
}
