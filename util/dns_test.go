package util

import (
	"net"
	"testing"

	"github.com/miekg/dns"
)

func TestIsEmpty(t *testing.T) {

	response := &dns.Msg{
		MsgHdr: dns.MsgHdr{
			Authoritative:     false,
			AuthenticatedData: false,
			CheckingDisabled:  false,
			RecursionDesired:  false,
			Opcode:            dns.OpcodeQuery,
		},
	}

	if !IsEmptyResponse(response) {
		t.Errorf("Expected empty response right away")
	}

	// add empty answer set
	response.Answer = make([]dns.RR, 100)
	if !IsEmptyResponse(response) {
		t.Errorf("Expected empty response even when response has answer size")
	}

	// add an empty A record
	response.Answer[0] = &dns.A{
		Hdr: dns.RR_Header{Name: "test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0},
	}
	if !IsEmptyResponse(response) {
		t.Errorf("Expected empty response even with empty A record")
	}

	// add an empty AAAA record
	response.Answer[1] = &dns.AAAA{
		Hdr: dns.RR_Header{Name: "test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0},
	}
	if !IsEmptyResponse(response) {
		t.Errorf("Expected empty response even with empty AAAA record")
	}

	// add a nil A record
	response.Answer[2] = &dns.A{
		Hdr: dns.RR_Header{Name: "test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0},
		A:   nil,
	}
	if !IsEmptyResponse(response) {
		t.Errorf("Expected empty response even with nil A record")
	}

	// add something that will work
	response.Answer[3] = &dns.A{
		Hdr: dns.RR_Header{Name: "test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0},
		A:   net.ParseIP("127.0.0.1"),
	}
	if IsEmptyResponse(response) {
		t.Errorf("Did not expect empty response after adding A record")
	}
}
