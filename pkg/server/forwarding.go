// Copyright (c) 2015 Jan Broer. All rights reserved.
// Use of this source code is governed by The MIT License (MIT) that can be
// found in the LICENSE file.

package server

import (
	"log"
	"strings"

	"github.com/miekg/dns"
)

// ServeDNSForward resolves a query by forwarding to a recursive nameserver
func (s *Server) ServeDNSForward(w dns.ResponseWriter, req *dns.Msg) *dns.Msg {
	name := req.Question[0].Name
	nameDots := dns.CountLabel(name) - 1
	refuse := false

	switch {
	case s.config.NoRec:
		log.Printf("D! [%d] Refusing query, recursion disabled", req.Id)
		refuse = true
	case len(s.config.Nameservers) == 0:
		log.Printf("D! [%d] Refusing query, no nameservers configured", req.Id)
		refuse = true
	case nameDots < s.config.FwdNdots && !s.config.EnableSearch:
		log.Printf("D! [%d] Refusing query, qname '%s' too short to forward", req.Id, name)
		refuse = true
	}

	if refuse {
		m := new(dns.Msg)
		m.SetRcode(req, dns.RcodeRefused)
		return m
	}

	StatsForwardCount.Inc(1)

	var searchEnabled, didAbsolute, didSearch bool
	var absoluteRes, searchRes *dns.Msg // responses from absolute/search lookups
	var absoluteErr, searchErr error    // errors from absolute/search lookups

	tcp := isTCP(w)

	if s.config.EnableSearch && len(s.config.SearchDomains) > 0 {
		searchEnabled = true
	}

	// If there are enough dots in the name, start with trying to
	// resolve the literal name
	if nameDots >= s.config.Ndots {
		if nameDots >= s.config.FwdNdots {
			log.Printf("D! [%d] Doing initial absolute lookup", req.Id)
			absoluteRes, absoluteErr = s.forwardQuery(req, tcp)
			if absoluteErr != nil {
				log.Printf("E! [%d] Error looking up literal qname '%s' with upstreams: %v", req.Id, name, absoluteErr)
			}

			if absoluteErr == nil && absoluteRes.Rcode == dns.RcodeSuccess {
				log.Printf("D! [%d] Initial lookup yielded result. Response to client: %s",
					req.Id, dns.RcodeToString[absoluteRes.Rcode])
				absoluteRes.Compress = true
				absoluteRes.Id = req.Id
				return absoluteRes
			}
			didAbsolute = true
		} else {
			log.Printf("D! [%d] Not doing absolute lookup, name too short: '%s'", req.Id, name)
		}
	}

	// We do at least one level of search if search is enabled
	// and we didn't previously fail to query the upstreams
	if absoluteErr == nil && searchEnabled {
		log.Printf("D! [%d] Doing search lookup", req.Id)
		searchRes, searchErr = s.forwardSearch(req, tcp)
		if searchErr != nil {
			log.Printf("E! [%d] Error looking up qname '%s' with search: %v", req.Id, name, searchErr)
		}

		if searchErr == nil && searchRes.Rcode == dns.RcodeSuccess {
			log.Printf("D! [%d] Search lookup yielded result. Response to client: %s",
				req.Id, dns.RcodeToString[searchRes.Rcode])
			searchRes.Compress = true
			searchRes.Id = req.Id
			return searchRes
		}
		didSearch = true
	}

	// If we didn't yet do an absolute lookup then do that now
	// if there are enough dots in the name and searching did
	// not previously fail
	if searchErr == nil && !didAbsolute {
		if nameDots >= s.config.FwdNdots {
			log.Printf("D! [%d] Doing absolute lookup", req.Id)
			absoluteRes, absoluteErr = s.forwardQuery(req, tcp)
			if absoluteErr != nil {
				log.Printf("E! [%d] Error resolving literal qname '%s': %v", req.Id, name, absoluteErr)
			}

			if absoluteErr == nil && absoluteRes.Rcode == dns.RcodeSuccess {
				log.Printf("D! [%d] Absolute lookup yielded result. Response to client: %s",
					req.Id, dns.RcodeToString[absoluteRes.Rcode])
				absoluteRes.Compress = true
				absoluteRes.Id = req.Id
				return absoluteRes
			}
			didAbsolute = true
		} else {
			log.Printf("D! [%d] Not doing absolute lookup, name too short: '%s'", req.Id, name)
		}
	}

	// If we got here, we failed to get a positive result for the query.
	// If we did an absolute query, return that result, otherwise return
	// a no-data response with the rcode from the last search we did.
	if didAbsolute && absoluteErr == nil {
		log.Printf("D! [%d] Failed to resolve query. Returning response of absolute lookup: %s",
			req.Id, dns.RcodeToString[absoluteRes.Rcode])
		absoluteRes.Compress = true
		absoluteRes.Id = req.Id
		return absoluteRes
	}

	if didSearch && searchErr == nil {
		log.Printf("D! [%d] Failed to resolve query. Returning no-data response: %s",
			req.Id, dns.RcodeToString[searchRes.Rcode])
		m := new(dns.Msg)
		m.SetRcode(req, searchRes.Rcode)
		return m
	}

	// If we got here, we either failed to forward the query or the qname was too
	// short to forward.
	log.Printf("D! [%d] Error forwarding query. Returning SRVFAIL.", req.Id)
	m := new(dns.Msg)
	m.SetRcode(req, dns.RcodeServerFailure)
	return m
}

// forwardSearch resolves a query by suffixing with search paths
func (s *Server) forwardSearch(req *dns.Msg, tcp bool) (*dns.Msg, error) {
	var r *dns.Msg
	var nodata *dns.Msg   // stores the copy of a NODATA reply
	var searchName string // stores the current name suffixed with search domain
	var err error
	var didSearch bool
	name := req.Question[0].Name // original qname
	reqCopy := req.Copy()

	for _, domain := range s.config.SearchDomains {
		if strings.HasSuffix(name, domain) {
			continue
		}

		searchName = strings.ToLower(appendDomain(name, domain))
		reqCopy.Question[0] = dns.Question{searchName, reqCopy.Question[0].Qtype, reqCopy.Question[0].Qclass}
		didSearch = true
		r, err = s.forwardQuery(reqCopy, tcp)
		if err != nil {
			// No Server currently available, give up
			break
		}

		switch r.Rcode {
		case dns.RcodeSuccess:
			// In case of NO_DATA keep searching, otherwise a wildcard entry
			// could keep us from finding the answer higher in the search list
			if len(r.Answer) == 0 && !r.MsgHdr.Truncated {
				nodata = r.Copy()
				continue
			}
		case dns.RcodeNameError:
			fallthrough
		case dns.RcodeServerFailure:
			// try next search element if any
			continue
		}
		// anything else implies that we are done searching
		break
	}

	if !didSearch {
		m := new(dns.Msg)
		m.SetRcode(req, dns.RcodeNameError)
		return m, nil
	}

	if err == nil {
		if r.Rcode == dns.RcodeSuccess {
			if len(r.Answer) > 0 {
				cname := new(dns.CNAME)
				cname.Hdr = dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 360}
				cname.Target = searchName
				answers := []dns.RR{cname}
				for _, rr := range r.Answer {
					answers = append(answers, rr)
				}
				r.Answer = answers
			}
			// If we ever got a NODATA, return this instead of a negative result
		} else if nodata != nil {
			r = nodata
		}
		// Restore original question
		r.Question[0] = req.Question[0]
	}

	if err != nil && nodata != nil {
		r = nodata
		r.Question[0] = req.Question[0]
		err = nil
	}

	return r, err
}

// forwardQuery sends the query to nameservers retrying once on error
func (s *Server) forwardQuery(req *dns.Msg, tcp bool) (*dns.Msg, error) {
	var nservers []string // Nameservers to use for this query
	var nsIdx int
	var r *dns.Msg
	var err error

	nservers = s.config.Nameservers

	// Check whether the name matches a stub zone
	if s.config.Stub != nil {
		for zone, srv := range s.config.Stub {
			if strings.HasSuffix(req.Question[0].Name, zone) {
				nservers = srv
				StatsStubForwardCount.Inc(1)
				break
			}
		}
	}

	for try := 1; try <= 2; try++ {
		log.Printf("D! [%d] Querying upstream %s for qname '%s'",
			req.Id, nservers[nsIdx], req.Question[0].Name)

		switch tcp {
		case false:
			r, _, err = s.dnsUDPClient.Exchange(req, nservers[nsIdx])
		case true:
			r, _, err = s.dnsTCPClient.Exchange(req, nservers[nsIdx])
		}

		if err == nil {
			log.Printf("D! [%d] Response code from upstream: %s", req.Id, dns.RcodeToString[r.Rcode])
			switch r.Rcode {
			// SUCCESS
			case dns.RcodeSuccess:
				fallthrough
			case dns.RcodeNameError:
				fallthrough
			// NO RECOVERY
			case dns.RcodeFormatError:
				fallthrough
			case dns.RcodeRefused:
				fallthrough
			case dns.RcodeNotImplemented:
				return r, err
			}
		}

		if err != nil {
			log.Printf("D! [%d] Failed to query upstream %s for qname '%s': %v",
				req.Id, nservers[nsIdx], req.Question[0].Name, err)
		}

		// Continue with next available Server
		if len(nservers)-1 > nsIdx {
			nsIdx++
		} else {
			nsIdx = 0
		}
	}

	return r, err
}

// ServeDNSReverse is the handler for DNS requests for the reverse zone. If nothing is found
// locally the request is forwarded to the forwarder for resolution.
func (s *Server) ServeDNSReverse(w dns.ResponseWriter, req *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Compress = true
	m.Authoritative = false
	m.RecursionAvailable = true
	if records, err := s.PTRRecords(req.Question[0]); err == nil && len(records) > 0 {
		m.Answer = records
		return m
	}
	// Always forward if not found locally.
	return s.ServeDNSForward(w, req)
}
